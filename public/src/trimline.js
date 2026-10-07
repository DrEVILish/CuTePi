// Cue Inspector trim timeline (DESIGN §5.5): the ftl-themes .waveform with
// the trim window as a .waveform-region, markers for In/Out, the fade
// boundaries and the trim length, a playhead and zoom.
//
// Loaded once (not inline in the inspector partial): it renders whatever
// #trim-timeline the inspector holds, on load and after every inspector
// swap, so its state (zoom window, parsed peaks) lives here.
//
// - The seek input is inert: clicking the waveform does nothing. It is the
//   playhead: when the selected cue is running, the bars colour in up to its
//   position, taken from the server (the Active Cues partial's
//   data-position), never predicted.
// - The region's two edges are the trim In and Out. They have no name: a
//   capture listener writes the Trim In/Out fields, and the form's own
//   change trigger saves.
// - The true trim points live in the timeline's dataset. While zoomed, an
//   edge whose point is outside the window would be clamped by the browser
//   with no event, so it is disabled and never read back.
// - Fades: each bar is drawn at the level that plays, the peak times the
//   engine's fade gain (same curve as gsp fadeShape), so the waveform shows
//   what the audience hears. Markers name the fade boundaries.
(function () {
  "use strict";
  var view = null;          // zoom window {from,to} in seconds; null = whole file
  var viewFile = "";        // the file the window belongs to
  var zoomMode = false, box = null, zoomAnim = null;
  var waveCache = null, wavePending = false;
  var peaksStore = { order: [], byUrl: {} };
  var pressing = false;     // pointer down in the timeline (a drag in progress)
  var MAX_BINS = 16000;     // media.MaxWaveformBins (GET /api/media/:name/wave)

  function tl() { return document.getElementById("trim-timeline"); }
  function num(v, d) { var n = parseFloat(v); return isNaN(n) ? d : n; }

  // hh:mm:ss.mmm, the format the server renders and ParseTime reads.
  function fmtClock(ms) {
    ms = Math.max(0, Math.round(ms));
    var p2 = function (n) { return (n < 10 ? "0" : "") + n; };
    var p3 = function (n) { return (n < 100 ? (n < 10 ? "00" : "0") : "") + n; };
    return p2(Math.floor(ms / 3600000)) + ":" + p2(Math.floor((ms % 3600000) / 60000)) + ":" +
      p2(Math.floor((ms % 60000) / 1000)) + "." + p3(ms % 1000);
  }
  function parseFadeSecs(str) {
    if (!str) return 0;
    var parts = String(str).trim().split(":");
    if (parts.length === 0 || parts.length > 3) return 0;
    var sec = parseFloat(parts[parts.length - 1]);
    if (isNaN(sec)) return 0;
    var mult = 1;
    for (var i = parts.length - 2; i >= 0; i--) {
      mult *= 60;
      var n = parseFloat(parts[i]);
      if (isNaN(n) || n < 0) return 0;
      sec += n * mult;
    }
    return sec < 0 ? 0 : sec;
  }
  // gsp fadeShape, mirrored: what is drawn is what the engine ramps.
  function fadeShape(curve, t) {
    if (t <= 0) return 0;
    if (t >= 1) return 1;
    if (curve === "smooth") return t * t * (3 - 2 * t);
    if (curve === "log") return Math.log10(1 + 9 * t);
    if (curve === "exp") { var k = 3; return (Math.exp(k * t) - 1) / (Math.exp(k) - 1); }
    return t;
  }

  // Trim state from the server's render (ms in the dataset); posEnd 0 means
  // "no trim out", shown at the full duration.
  function state(t) {
    var dur = num(t.dataset.duration, 0);
    var s = num(t.dataset.posStart, 0) / 1000, e = num(t.dataset.posEnd, 0) / 1000;
    if (!(e > 0)) e = dur;
    return { start: s, end: e, dur: dur };
  }
  function fades() {
    var fin = document.querySelector("[data-fade-in]");
    var fout = document.querySelector("[data-fade-out]");
    var curve = document.getElementById("insp-fadecurve") || document.getElementById("insp-img-fadecurve");
    return { fin: fin ? parseFadeSecs(fin.value) : 0, fout: fout ? parseFadeSecs(fout.value) : 0,
      curve: curve ? curve.value : "linear" };
  }
  // The engine's gain at media time sec: fade-in from the trim In, fade-out
  // into the trim Out. Outside the trim the material is shown at full level
  // (it is what you choose trim points from).
  function gainAt(sec, st, f) {
    if (sec < st.start || sec > st.end) return 1;
    var g = 1;
    if (f.fin > 0 && sec < st.start + f.fin) g = Math.min(g, fadeShape(f.curve, (sec - st.start) / f.fin));
    if (f.fout > 0 && sec > st.end - f.fout) g = Math.min(g, fadeShape(f.curve, (st.end - sec) / f.fout));
    return g;
  }
  function win(st) {
    if (!view) return { from: 0, to: st.dur };
    return { from: Math.max(0, view.from), to: Math.min(st.dur, view.to) };
  }

  // --- Peaks ---------------------------------------------------------------
  // The stored envelope (/api/media/:name/peaks?v=, cached by the browser)
  // is fetched once per clip and kept parsed.
  function storedPeaks(t) {
    var url = t.dataset.waveUrl;
    if (!url) return null;
    var hit = peaksStore.byUrl[url];
    if (Array.isArray(hit)) return hit;
    if (hit !== "pending") {
      peaksStore.byUrl[url] = "pending";
      fetch(url).then(function (r) {
        if (!r.ok) throw new Error(String(r.status));
        return r.json();
      }).then(function (peaks) {
        peaksStore.byUrl[url] = Array.isArray(peaks) ? peaks : [];
        peaksStore.order.push(url);
        while (peaksStore.order.length > 6) delete peaksStore.byUrl[peaksStore.order.shift()];
        render();
      }).catch(function () { delete peaksStore.byUrl[url]; });
    }
    return null;
  }
  // Zoomed in past the stored envelope's resolution: fetch the window's own
  // envelope (an ffmpeg decode, so only for windows under 60 s).
  function requestWave(t, from, to, want) {
    if (wavePending || zoomAnim) return;
    var dur = num(t.dataset.duration, 0), span = to - from, pad = span * 0.25;
    var cFrom = Math.max(0, from - pad), cTo = Math.min(dur, to + pad);
    if (!(cTo > cFrom)) return;
    var bins = Math.max(64, Math.min(MAX_BINS, Math.round(want * (cTo - cFrom) / span)));
    var file = t.dataset.filename;
    wavePending = true;
    fetch("/api/media/" + encodeURIComponent(file) + "/wave?from=" + cFrom + "&to=" + cTo + "&bins=" + bins)
      .then(function (r) { wavePending = false; if (!r.ok) throw new Error(String(r.status)); return r.json(); })
      .then(function (peaks) {
        if (Array.isArray(peaks) && peaks.length > 0) waveCache = { file: file, from: cFrom, to: cTo, peaks: peaks };
        render();
      }).catch(function () { wavePending = false; });
  }
  // Max-hold sample of src (covering [sFrom,sTo]) at the centre of each of
  // n bars over [from,to].
  function levels(src, sFrom, sTo, from, to, n) {
    var out = new Array(n), per = (to - from) / n, len = src.length, scale = len / (sTo - sFrom);
    for (var i = 0; i < n; i++) {
      var a = Math.floor((from + i * per - sFrom) * scale);
      var b = Math.max(a + 1, Math.floor((from + (i + 1) * per - sFrom) * scale));
      var m = 0;
      for (var j = Math.max(0, a); j < Math.min(len, b); j++) if (src[j] > m) m = src[j];
      out[i] = m;
    }
    return out;
  }

  // --- Render --------------------------------------------------------------
  // Region and markers sit on the edges' thumb centres (half a handle in
  // from each end), so a marker line meets its handle.
  function placer(wf) {
    var w = wf.clientWidth || 1;
    var hw = window.matchMedia && matchMedia("(any-pointer: coarse)").matches ? 20 : 8;
    return function (f) { return (hw / 2 + f * (w - hw)) / w; };
  }
  function edgeStep(span) {
    var raw = span / 1000, p = Math.pow(10, Math.floor(Math.log10(raw)));
    return Math.max(0.001, Math.ceil(raw / p) * p);
  }
  function setMarker(el, show, at, text, nearEnd) {
    if (!el) return;
    el.hidden = !show;
    if (!show) return;
    el.style.setProperty("--at", at.toFixed(4));
    el.textContent = text;
    if (nearEnd !== undefined) el.classList.toggle("is-end", nearEnd);
  }

  function render() {
    var t = tl();
    if (!t) return;
    var wf = t.querySelector(".waveform");
    if (!wf) return;
    watch(wf);
    // Hidden (another inspector tab): nothing to measure; the size watch
    // renders it when it shows.
    if (!wf.clientWidth) return;
    var st = state(t);
    if (!(st.dur > 0)) return;
    if (t.dataset.filename !== viewFile) { view = null; viewFile = t.dataset.filename; waveCache = null; }
    var w = win(st), span = w.to - w.from;
    if (!(span > 0)) return;
    var f = fades();
    var at = placer(wf);
    var frac = function (sec) { return (sec - w.from) / span; };
    var inView = function (sec) { return sec >= w.from - 1e-6 && sec <= w.to + 1e-6; };

    // Bars: about one per 3 px, created once per width.
    var bars = wf.querySelector(".waveform-bars");
    var n = Math.max(32, Math.floor((wf.clientWidth || 600) / 3));
    if (bars.childElementCount !== n) {
      var frag = document.createDocumentFragment();
      for (var i = 0; i < n; i++) frag.appendChild(document.createElement("i"));
      bars.replaceChildren(frag);
    }
    var stored = storedPeaks(t);
    var src = stored, sFrom = 0, sTo = st.dur;
    if (waveCache && waveCache.file === t.dataset.filename && w.from >= waveCache.from && w.to <= waveCache.to) {
      src = waveCache.peaks; sFrom = waveCache.from; sTo = waveCache.to;
    } else if (stored && view && span <= 60 && stored.length * span / st.dur < n) {
      requestWave(t, w.from, w.to, n);
    }
    wf.classList.toggle("is-loading", t.dataset.wavePending === "1" || (!!t.dataset.waveUrl && !src));
    var lv = src ? levels(src, sFrom, sTo, w.from, w.to, n) : null;
    var kids = bars.children, per = span / n;
    for (var k = 0; k < n; k++) {
      var level = lv ? lv[k] * gainAt(w.from + (k + 0.5) * per, st, f) : 0;
      kids[k].style.setProperty("--level", level.toFixed(3));
    }

    // Playhead (seek input, inert): this cue's position from the server.
    var seek = wf.querySelector(":scope > input[type=range]");
    var pos = playheadOf(t);
    seek.min = w.from; seek.max = w.to;
    seek.value = pos !== null && inView(pos) ? pos : w.from;
    wf.classList.toggle("has-playhead", pos !== null && inView(pos));

    // Trim region: edges span the window; an edge outside it is disabled.
    var region = wf.querySelector(".waveform-region");
    var eIn = document.getElementById("trim-edge-in"), eOut = document.getElementById("trim-edge-out");
    var step = edgeStep(span);
    [eIn, eOut].forEach(function (e) { e.min = w.from; e.max = w.to; e.step = step; });
    eIn.disabled = !inView(st.start);
    eOut.disabled = !inView(st.end);
    eIn.value = Math.min(w.to, Math.max(w.from, st.start));
    eOut.value = Math.min(w.to, Math.max(w.from, st.end));
    eIn.setAttribute("aria-valuetext", fmtClock(st.start * 1000));
    eOut.setAttribute("aria-valuetext", fmtClock(st.end * 1000));
    var rs = Math.max(0, Math.min(1, frac(st.start))), re = Math.max(0, Math.min(1, frac(st.end)));
    region.style.setProperty("--start", rs.toFixed(4));
    region.style.setProperty("--end", re.toFixed(4));

    // Markers: In / Out times (chips outside the window, flipped at the
    // ends), fade boundaries (chips inside), the trim length.
    setMarker(document.getElementById("trim-mk-in"), inView(st.start), at(rs),
      "In " + fmtClock(st.start * 1000), at(rs) > 0.12);
    setMarker(document.getElementById("trim-mk-out"), inView(st.end), at(re),
      "Out " + fmtClock(st.end * 1000), at(re) > 0.88);
    var finEnd = Math.min(st.end, st.start + f.fin), foutStart = Math.max(st.start, st.end - f.fout);
    setMarker(document.getElementById("trim-mk-fadein"), f.fin > 0 && inView(finEnd), at(frac(finEnd)),
      "Fade in " + f.fin.toFixed(3).replace(/\.?0+$/, "") + " s", at(frac(finEnd)) > 0.85);
    setMarker(document.getElementById("trim-mk-fadeout"), f.fout > 0 && inView(foutStart), at(frac(foutStart)),
      "Fade out " + f.fout.toFixed(3).replace(/\.?0+$/, "") + " s", at(frac(foutStart)) > 0.15);
    var len = document.getElementById("trim-length");
    if (len) {
      len.textContent = "Length " + fmtClock((st.end - st.start) * 1000);
      len.style.setProperty("--at", Math.max(0.08, Math.min(0.92, at((rs + re) / 2))).toFixed(4));
    }
  }

  // The selected cue's playback position, as the server last reported it
  // (Active Cues partial), or null when it is not running.
  function playheadOf(t) {
    var li = document.querySelector('#activecues li[data-cue-pos="' + t.dataset.cuePos + '"]');
    if (!li || li.dataset.position === undefined) return null;
    var p = parseFloat(li.dataset.position);
    return isNaN(p) ? null : p;
  }

  // --- Editing -------------------------------------------------------------
  function writeFields(t) {
    var st = state(t);
    var a = document.getElementById("insp-in"), b = document.getElementById("insp-out");
    if (a) a.value = fmtClock(st.start * 1000);
    if (b) b.value = fmtClock(st.end * 1000);
  }
  // An edge moved (drag or keys): only that edge's value is taken (the other
  // may be clamped by the zoom window), kept the minimum length apart.
  function edgeMoved(e) {
    var t = tl();
    if (!t || !(e.target instanceof HTMLInputElement)) return false;
    var isIn = e.target.id === "trim-edge-in", isOut = e.target.id === "trim-edge-out";
    if (!isIn && !isOut) return false;
    var st = state(t), v = parseFloat(e.target.value), gap = 0.05;
    if (isIn) t.dataset.posStart = Math.round(Math.max(0, Math.min(v, st.end - gap)) * 1000);
    else t.dataset.posEnd = Math.round(Math.min(st.dur, Math.max(v, st.start + gap)) * 1000);
    writeFields(t);
    render();
    return true;
  }
  document.addEventListener("input", function (e) { edgeMoved(e); }, true);
  // Capture: the fields hold the new value before the form's change trigger
  // (200 ms later) reads them.
  document.addEventListener("change", function (e) { edgeMoved(e); }, true);
  function save() {
    var form = document.getElementById("inspector-form");
    if (form && window.htmx) htmx.trigger(form, "ctp-save");
  }

  // A drag must not be cut short by an inspector re-render (ui.js asks).
  document.addEventListener("pointerdown", function (e) {
    var t = tl();
    if (t && e.target instanceof Element && t.contains(e.target)) pressing = true;
  }, true);
  var release = function () {
    if (!pressing) return;
    pressing = false;
    document.dispatchEvent(new Event("ctp-trim-released"));
  };
  document.addEventListener("pointerup", release, true);
  document.addEventListener("pointercancel", release, true);

  // --- Zoom ----------------------------------------------------------------
  function setView(nfrom, nto, dur) {
    if (nto - nfrom < 0.05) return;
    if (zoomAnim) { cancelAnimationFrame(zoomAnim); zoomAnim = null; }
    var target = (nfrom <= 0 && nto >= dur) ? null : { from: Math.max(0, nfrom), to: Math.min(dur, nto) };
    var start = view ? { from: view.from, to: view.to } : { from: 0, to: dur };
    var t0 = performance.now();
    function frame(now) {
      var k = Math.min(1, (now - t0) / 200), e = 1 - Math.pow(1 - k, 3);
      var goal = target || { from: 0, to: dur };
      view = { from: start.from + (goal.from - start.from) * e, to: start.to + (goal.to - start.to) * e };
      if (e >= 1) { view = target; zoomAnim = null; }
      render();
      if (e < 1) zoomAnim = requestAnimationFrame(frame);
    }
    zoomAnim = requestAnimationFrame(frame);
  }
  function zoomStep(dir) {
    var t = tl();
    if (!t) return;
    var st = state(t), w = win(st), span = w.to - w.from, from = w.from, to = w.to;
    if (dir < 0) { from += span * 0.25; to -= span * 0.25; } else { from -= span * 0.5; to += span * 0.5; }
    setView(from, to, st.dur);
  }
  function disarmZoom() {
    zoomMode = false;
    var t = tl();
    if (t) t.classList.remove("trim-zoom-active");
    var b = document.getElementById("trim-zoom");
    if (b) b.setAttribute("aria-pressed", "false");
  }
  // Zoom armed: drag a box over the waveform to zoom into it.
  document.addEventListener("pointerdown", function (e) {
    if (!zoomMode || !(e.target instanceof Element)) return;
    var wf = e.target.closest("#trim-timeline .waveform");
    if (!wf) return;
    var r = wf.getBoundingClientRect();
    box = { wf: wf, f0: r.width ? (e.clientX - r.left) / r.width : 0, lo: 0, hi: 0 };
    e.preventDefault();
    wf.setPointerCapture(e.pointerId);
  });
  document.addEventListener("pointermove", function (e) {
    if (!box) return;
    var r = box.wf.getBoundingClientRect();
    if (!r.width) return;
    var f1 = Math.max(0, Math.min(1, (e.clientX - r.left) / r.width));
    box.lo = Math.min(box.f0, f1); box.hi = Math.max(box.f0, f1);
    var sel = document.getElementById("trim-zoomsel");
    if (sel) { sel.style.left = (box.lo * 100) + "%"; sel.style.width = ((box.hi - box.lo) * 100) + "%"; sel.classList.add("trim-zoomsel-active"); }
  });
  function endBox(apply) {
    if (!box) return;
    var sel = document.getElementById("trim-zoomsel");
    if (sel) { sel.classList.remove("trim-zoomsel-active"); sel.style.width = ""; sel.style.left = ""; }
    var t = tl();
    if (apply && t && box.hi - box.lo > 0.02) {
      var st = state(t), w = win(st), span = w.to - w.from;
      setView(w.from + span * box.lo, w.from + span * box.hi, st.dur);
    }
    box = null;
    disarmZoom();
  }
  document.addEventListener("pointerup", function () { endBox(true); });
  document.addEventListener("pointercancel", function () { endBox(false); });
  // Zoomed in: the wheel over the waveform pans the window.
  document.addEventListener("wheel", function (e) {
    if (!(e.target instanceof Element) || !e.target.closest("#trim-timeline") || !view) return;
    var t = tl(), st = state(t), span = view.to - view.from;
    if (!(span > 0) || span >= st.dur - 0.05) return;
    e.preventDefault();
    if (zoomAnim) { cancelAnimationFrame(zoomAnim); zoomAnim = null; }
    var d = Math.abs(e.deltaY) > Math.abs(e.deltaX) ? e.deltaY : e.deltaX;
    if (e.deltaMode === 1) d *= 16; else if (e.deltaMode === 2) d *= 100;
    var shift = d * span / 600, nf = view.from + shift, nt = view.to + shift;
    if (nf < 0) { nt -= nf; nf = 0; }
    if (nt > st.dur) { nf -= nt - st.dur; nt = st.dur; }
    view = { from: Math.max(0, nf), to: nt };
    render();
  }, { passive: false });

  // Buttons under the timeline: clear trim and zoom.
  document.addEventListener("click", function (e) {
    if (!(e.target instanceof Element)) return;
    var btn = e.target.closest("#trim-clear, #trim-zoom, #trim-zoom-in, #trim-zoom-out, #trim-zoom-reset");
    var t = tl();
    if (!btn || !t) return;
    var st = state(t);
    if (btn.id === "trim-clear") {
      t.dataset.posStart = 0; t.dataset.posEnd = Math.round(st.dur * 1000);
      writeFields(t); render(); save();
    } else if (btn.id === "trim-zoom") {
      zoomMode = !zoomMode;
      t.classList.toggle("trim-zoom-active", zoomMode);
      btn.setAttribute("aria-pressed", zoomMode ? "true" : "false");
    } else if (btn.id === "trim-zoom-in") zoomStep(-1);
    else if (btn.id === "trim-zoom-out") zoomStep(1);
    else if (btn.id === "trim-zoom-reset") { setView(0, st.dur, st.dur); disarmZoom(); }
  });

  // --- When to render --------------------------------------------------------
  document.body.addEventListener("htmx:after:swap", function (e) {
    var tgt = e.detail && e.detail.ctx && e.detail.ctx.target;
    if (tgt && (tgt.id === "cueinspector-body" || tgt.id === "cueinspector-collapse")) Promise.resolve().then(render);
  });
  document.addEventListener("cutepi-activecues", render); // new positions from the server
  document.addEventListener("input", function (e) {
    if (e.target instanceof Element && e.target.matches("[data-fade-in],[data-fade-out],#insp-fadecurve,#insp-img-fadecurve")) render();
  });
  // The waveform's own size: the window, the inspector's drag handle, and
  // its tab being shown (it is laid out at zero width while hidden).
  var resizeTimer = null, watched = null;
  var ro = window.ResizeObserver ? new ResizeObserver(function () {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(render, 30);
  }) : null;
  function watch(wf) {
    if (!ro || wf === watched) return;
    if (watched) ro.unobserve(watched);
    watched = wf;
    ro.observe(wf);
  }

  window.ctpTrim = { render: render, fadeShape: fadeShape, pressing: function () { return pressing; } };
  render();
})();
