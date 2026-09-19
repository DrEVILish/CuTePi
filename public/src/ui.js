// Menus, hover popouts and the rest of this file are plain DOM code and must
// survive a failed htmx bundle (an old browser can kill htmax.js while the
// code below runs fine). Stub the htmx surface so nothing below throws.
window.htmx = window.htmx || {
  on() {}, process() {}, trigger() {}, config: {},
  ajax() { return Promise.reject(new Error("htmx unavailable")); },
};
// Preserve the htmx-2 behaviour (previously set via the htmx-config meta's
// noSwap:["4xx","5xx"]) that HTTP error responses never replace the DOM. The
// backend renders error.html partials on 4xx/5xx (e.g. DELETE /api/media
// returns 409 when the file is currently playing); those must not be swapped
// into the #cuesheet/#mediapool targets. Only 204/304 are no-swap by default
// in htmx 4, so suppress 4xx/5xx swaps explicitly here.
htmx.on("htmx:before:swap", (e) => {
  if (e.detail.ctx.response.raw.status >= 400) {
    e.preventDefault();
  }
});

// Failed htmx requests are never swapped (above), so without this a failed
// action (play, fade, group ops, ...) looks exactly like a no-op click.
// Surface every 4xx/5xx as a dismissible toast, extracting the server's
// message from the error.html body when present.
htmx.on("htmx:response:error", (e) => {
  const ctx = e.detail?.ctx || {};
  let detail = "";
  try {
    detail = new DOMParser().parseFromString(ctx.text || "", "text/html")
      .body.textContent.replace(/\s+/g, " ").trim();
  } catch (err) { /* body may be empty or non-HTML; status alone still shows */ }
  if (detail.length > 160) detail = detail.slice(0, 159) + "…";
  const status = ctx.response?.raw?.status ?? "?";
  showToast("Request failed (" + status + ")" + (detail ? ": " + detail + "" : ""));
  // Mirror the failure into the server log viewer so transient 404s/500s are
  // visible from the logs page, not just the browser console.
  try {
    const path = ctx.request?.path || ctx.sourceElement?.getAttribute?.("hx-get") ||
      ctx.sourceElement?.getAttribute?.("hx-post") || ctx.sourceElement?.getAttribute?.("hx-put") || "";
    const qs = new URLSearchParams({ status: String(status), path: String(path), detail });
    navigator.sendBeacon?.("/api/logs/client", qs.toString());
  } catch (err) { /* reporting is best-effort */ }
});

// --- Inspector panel: sequenced swaps (fixes out-of-order UI updates) ---
// Requests into the inspector panel are issued from several independent
// triggers (form save PUT, WS-sync refresh, cuesheet observer refresh,
// group/cue inspector GETs). Responses may arrive in any order, and a stale
// one that lands last would show the wrong waveform/trim for the selection.
// Every request aimed at the inspector gets a monotonically increasing seq
// stamp at REQUEST time; a swap carrying an OLDER stamp than the last one
// applied is dropped.
let inspSeqIssued = 0, inspSeqApplied = 0;
const isInspectorTarget = (t) => t && (t.id === "cueinspector-body" || t.id === "cueinspector-collapse");
document.body.addEventListener("htmx:before:request", (e) => {
  const ctx = e.detail?.ctx;
  if (!isInspectorTarget(ctx?.target)) return;
  ctx.__inspSeq = ++inspSeqIssued;
});
document.body.addEventListener("htmx:before:swap", (e) => {
  const ctx = e.detail?.ctx;
  if (!isInspectorTarget(ctx?.target)) return;
  const seq = ctx.__inspSeq;
  if (typeof seq !== "number") return;
  if (seq < inspSeqApplied) {
    e.preventDefault(); // stale response - drop it
    return;
  }
  inspSeqApplied = seq;
});

// Error responses are deliberately not swapped into application panels. Keep
// the originating YouTube form useful by showing its sanitized server error
// in the modal instead of failing silently.

// A GROUP inspector fetch that 404s means the group is gone (deleted while
// its panel was shown): the auto-follow would keep re-requesting it forever
// (stale dataset.groupId, endless red toasts). Fall back to the empty cue
// inspector once — the stale id clears with the swap.
document.body.addEventListener("htmx:responseError", (e) => {
  const ctx = e.detail;
  if (!isInspectorTarget(ctx?.target)) return;
  const path = ctx.request?.path || ctx.sourceElement?.getAttribute?.("hx-get") || "";
  if (/\/api\/group\/\d+\/inspector/.test(path) && window.htmx) {
    htmx.ajax("GET", "/api/cue/inspector?_=" + Date.now(), {target: "#cueinspector-body", swap: "outerHTML"});
  }
});

htmx.on("htmx:after:request", (e) => {
  const el = e.detail.ctx?.sourceElement;
  const ok = (e.detail.ctx?.request?.status ?? 500) < 400;
  const ui = () => window.bootstrap;
  if (el && el.id === "youtube-dl") {
    const info = document.getElementById("info");
    if (info) info.textContent = ok ? "Download complete" : "Download failed. Check the server log.";
    if (ok) hideModal("ytdlModal");
    return;
  }
  if (el && el.id === "dropform" && ok) hideModal("uploadModal");
  if (el && el.id === "testHideBtn" && ok) hideModal("testModal");
  if (el && el.id === "deleteConfirmBtn" && ok) hideModal("deleteModal");
  if (el && el.id === "showTestBtn" && ok) showModal("testModal");
  if (el && el.id === "settingsForm") {
    const status = document.getElementById("settingsStatus");
    if (!status) return;
    if (ok) {
      try {
        const body = JSON.parse(e.detail.ctx.text || "{}");
        const sPort = document.getElementById("settingsPort");
        const sPoll = document.getElementById("settingsPollInterval");
        if (sPort) sPort.value = body.port;
        if (sPoll) sPoll.value = body.pollInterval;
        const authState = document.getElementById("settingsAuthState");
        if (authState && typeof body.authEnabled === "boolean") {
          authState.textContent = body.authEnabled
            ? "A password is currently set."
            : "No password set (open access).";
        }
        status.textContent = body.message || "Saved.";
        status.className = "text-success";
      } catch (err) {
        status.textContent = "Saved (could not read response).";
        status.className = "text-success";
      }
    } else {
      status.textContent = "Save failed - check the values (port 1-65535, poll interval >= 10ms).";
      status.className = "text-danger";
    }
  }
  void ui;
});

function showModal(id) {
  if (!window.bootstrap) return;
  const m = bootstrap.Modal.getOrCreateInstance(document.getElementById(id));
  if (m) m.show();
}
function hideModal(id) {
  if (!window.bootstrap) return;
  const m = bootstrap.Modal.getInstance(document.getElementById(id));
  if (m) m.hide();
}

// One dismissible bootstrap toast per error; the container is created lazily
// so every page that loads ui.js gets feedback with no markup changes.
function showToast(message) {
  let holder = document.getElementById("ctp-toasts");
  if (!holder) {
    holder = document.createElement("div");
    holder.id = "ctp-toasts";
    holder.className = "toast-container position-fixed bottom-0 end-0 p-3";
    holder.style.zIndex = 2000; // above modals (1055)
    document.body.appendChild(holder);
  }
  const el = document.createElement("div");
  el.className = "toast align-items-center text-bg-danger border-0";
  el.setAttribute("role", "alert");
  el.innerHTML =
    '<div class="d-flex"><div class="toast-body"></div>' +
    '<button type="button" class="btn-close btn-close-white me-2 m-auto" data-bs-dismiss="toast" aria-label="Close"></button></div>';
  el.querySelector(".toast-body").textContent = message;
  holder.appendChild(el);
  if (window.bootstrap && bootstrap.Toast) {
    new bootstrap.Toast(el, { delay: 6000 }).show();
    el.addEventListener("hidden.bs.toast", () => el.remove());
  } else {
    // No bootstrap JS (shouldn't happen): show it raw so it is never silent.
    el.classList.add("show");
    setTimeout(() => el.remove(), 6000);
  }
}

// Theme allowlist: server-rendered boot script in header.html already knows
// the discovered files; this copy refreshes from /api/themes (same source,
// routes.Themes) so newly added theme files validate client-side too. The
// hardcoded set is only the fallback when the endpoint is unreachable.
const appThemes = new Set(["lcars", "qlab", "blue-future", "custom"]);
fetch("/api/themes", { headers: { Accept: "application/json" } })
  .then((resp) => (resp.ok ? resp.json() : []))
  .then((list) => {
    if (Array.isArray(list)) list.forEach((t) => { if (t && t.name) appThemes.add(t.name); });
  })
  .catch(() => {});
const CUSTOM_THEME_KEY = "cutepi.customTheme";

// Inject (or update) a <style> that applies the saved custom theme's design
// tokens to :root[data-theme=custom]. The pasted JSON is a flat map of
// --ctp-* CSS variable names to values.
function applyCustomTheme(tokens) {
  let style = document.getElementById("cutepi-custom-theme");
  if (!style) {
    style = document.createElement("style");
    style.id = "cutepi-custom-theme";
    document.head.appendChild(style);
  }
  if (!tokens || typeof tokens !== "object") {
    style.textContent = "";
    return;
  }
  const decls = Object.entries(tokens)
    .map(([k, v]) => "  " + k + ": " + v + ";")
    .join("\n");
  style.textContent = ":root[data-theme=custom] {\n" + decls + "\n}";
}

// Edit/Show mode (footer toggle): Show mode arms scheduled triggers and
// test-pattern output; Edit mode is for building. The server persists the
// mode and renders the initial body class; this only flips it live.
function applyShowMode(show) {
  document.body.classList.toggle("show-mode", show);
  const t = document.getElementById("mode-toggle");
  if (t) {
    t.setAttribute("aria-checked", show ? "true" : "false");
    const label = t.querySelector(".mode-label");
    if (label) label.textContent = show ? "SHOW" : "EDIT";
  }
  // Test output is only available in Show mode.
  const tests = document.getElementById("showTestBtn");
  if (tests) tests.disabled = !show;
  // The Cue Inspector is unavailable in Show mode (see layout.css): keep
  // the footer expand button disabled too so it cannot be reopened.
  const inspExpand = document.getElementById("cueinspector-expand-btn");
  if (inspExpand) inspExpand.disabled = show;
}
// Show mode is for performance: the sheet is locked. Editing gestures
// (delete, context menu, drag-drop, media add, inline edit) refuse with a
// toast; transport (GO/arrows/space) keeps working.
function isShowMode() { return document.body.classList.contains("show-mode"); }
function needEditMode() {
  if (!isShowMode()) return true;
  showToast("Switch to EDIT mode to change the sheet");
  return false;
}
// Inline editors stay inert in Show mode: intercept the double-click in
// capture phase, before htmx's bubble-phase dblclick trigger can open one.
document.addEventListener("dblclick", (e) => {
  if (!(e.target instanceof Element)) return;
  if (isShowMode() && e.target.closest(".cue-inline-edit")) {
    e.preventDefault();
    e.stopImmediatePropagation();
    showToast("Switch to EDIT mode to change the sheet");
  }
}, true);
document.addEventListener("click", (e) => {
  if (!(e.target instanceof Element)) return;
  if (!e.target.closest("#mode-toggle")) return;
  const show = !document.body.classList.contains("show-mode");
  fetch("/api/setting/showmode", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: show ? "showmode=1" : "",
  }).then((resp) => { if (resp.ok) applyShowMode(show); })
    .catch(() => {});
});
applyShowMode(document.body.classList.contains("show-mode"));

// The GO button flashes while a scheduled cue fires within the minute.
// Polled, not pushed: schedules approach without touching sheet state.
function pollScheduleFlash() {
  fetch("/api/schedule/next", { headers: { Accept: "application/json" } })
    .then((resp) => (resp.ok ? resp.json() : null))
    .then((s) => {
      const btn = document.getElementById("go-btn");
      if (!btn) return;
      btn.classList.toggle("go-imminent",
        !!(s && s.armed && !s.none && typeof s.dueInSec === "number" && s.dueInSec < 60));
    })
    .catch(() => {});
}
pollScheduleFlash();
setInterval(pollScheduleFlash, 10000);

function applyAppTheme(theme) {
  if (!appThemes.has(theme)) theme = "blue-future";
  document.documentElement.dataset.theme = theme;
  if (theme === "custom") {
    let tokens = null;
    try { tokens = JSON.parse(localStorage.getItem(CUSTOM_THEME_KEY)); } catch (e) {}
    applyCustomTheme(tokens);
  }
  try {
    localStorage.setItem("cutepi.theme", theme);
  } catch (e) {}
}

document.addEventListener("change", (e) => {
  if (e.target.id === "settingsTheme") {
    const value = e.target.value;
    if (value === "custom") {
      const el = document.getElementById("settingsCustomTheme");
      if (el) {
        try {
          const tokens = JSON.parse(el.value);
          localStorage.setItem(CUSTOM_THEME_KEY, el.value);
          applyCustomTheme(tokens);
        } catch (err) {
          const live = document.getElementById("settingsStatus");
          if (live) {
            live.textContent = "Custom theme JSON is invalid.";
            live.className = "text-danger";
          }
          el.value = localStorage.getItem(CUSTOM_THEME_KEY) || "{}";
          return; // don't switch theme on invalid paste
        }
      }
    }
    applyAppTheme(value);
  }
});

window.addEventListener("keydown", (e) => {
  const active = document.activeElement;
  const tag = active && active.nodeName ? active.nodeName.toLowerCase() : "";
  const itype = tag === "input" ? String(active.type || "").toLowerCase() : "";
  // Arrow keys always drive row selection except where the user is actively
  // editing text or driving a control that owns its arrows (text inputs,
  // selects, textareas, buttons). Radios (the colour swatches) and
  // checkboxes give up their native arrow behaviour so arrows always move
  // the cue row selection; Space keeps toggling those controls natively and
  // only plays/stops from plain (non-control) focus.
  const plain = !active || (tag !== "input" && tag !== "textarea" && tag !== "select" && tag !== "button");
  const arrowTarget = plain || (tag === "input" && (itype === "radio" || itype === "checkbox"));
  const arrows = ["ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight"];
  if (plain && ["Space"].concat(arrows).indexOf(e.code) > -1) {
    e.preventDefault();
  } else if (arrowTarget && arrows.indexOf(e.code) > -1) {
    e.preventDefault(); // stop native radio/checkbox arrow navigation
  }
  // Space (not Enter) is GO: Enter is reserved for menu/inline-edit commit
  // and must never fire the selected cue.
  if (plain && ["Space"].indexOf(e.code) > -1) {
    htmx.trigger("#spaceBar", "spaceBar")
  }
  // Shift+Up/Down extends the multi-selection instead of moving it.
  if (arrowTarget && e.shiftKey && ["ArrowUp", "ArrowDown"].indexOf(e.code) > -1) {
    e.preventDefault();
    htmx.ajax("POST", e.code === "ArrowUp" ? "/api/cue/prev?extend=1" : "/api/cue/next?extend=1",
      {target: "#cuesheet", swap: "outerHTML"});
  } else if (arrowTarget) {
    if (["ArrowUp"].indexOf(e.code) > -1) {
      htmx.trigger("#ArrowUp", "ArrowUp")
    }
    if (["ArrowDown"].indexOf(e.code) > -1) {
      htmx.trigger("#ArrowDown", "ArrowDown")
    }
    // Left/right arrow on a selected cue group closes/opens it (no-ops on
    // cues — the server answers 200 and re-renders unchanged).
    if (["ArrowRight"].indexOf(e.code) > -1) {
      htmx.trigger("#ArrowRight", "arrowRight")
    }
    if (["ArrowLeft"].indexOf(e.code) > -1) {
      htmx.trigger("#ArrowLeft", "arrowLeft")
    }
  }
  // Escape = stop — but only when no context menu holds the gesture: menus
  // use Escape to close (their own listeners run for the same keydown), and
  // closing a menu must not also kill playback.
  if (plain && ["Escape"].indexOf(e.code) > -1) {
    const menuOpen = document.querySelector(".cue-context-menu:not([hidden])");
    if (!menuOpen) htmx.trigger("#esc", "esc")
  }
  // Ctrl/Cmd+A selects every rendered cue (plain focus only).
  if ((e.ctrlKey || e.metaKey) && e.code === "KeyA" && plain) {
    e.preventDefault();
    htmx.ajax("POST", "/api/cue/selectall", {target: "#cuesheet", swap: "outerHTML"});
  }
  // Cmd/Ctrl+Backspace deletes the highlighted cues (anchor + range).
  if ((e.ctrlKey || e.metaKey) && e.code === "Backspace" && plain) {
    e.preventDefault();
    if (!needEditMode()) return;
    const rows = [...document.querySelectorAll('#cuesheet tr.cue[data-cue-pos]')];
    const positions = rows
      .filter((r) => r.classList.contains("table-warning") || r.dataset.cueSel === "1")
      .map((r) => parseInt(r.dataset.cuePos, 10))
      .filter((n) => Number.isFinite(n));
    // Selected group blocks come as their own payload: whole folder tree
    // and its member cues disappear (§5.4).
    const groups = [...document.querySelectorAll("#cuesheet tr.cue-group-header[data-group-id]")]
      .filter((r) => r.classList.contains("table-warning"))
      .map((r) => parseInt(r.dataset.groupId, 10))
      .filter((n) => Number.isFinite(n));
    if (!positions.length && !groups.length) return;
    fetch("/api/cue/bulk", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ op: "delete", positions, groups }),
    })
      .then((res) => {
        if (!res.ok) throw new Error("server returned " + res.status);
        return res.text();
      })
      .then((html) => {
        const current = document.getElementById("cuesheet");
        if (!current) return;
        const wrapper = document.createElement("div");
        wrapper.innerHTML = html.trim();
        const replacement = wrapper.firstElementChild;
        if (replacement && replacement.id === "cuesheet") {
          current.replaceWith(replacement);
          if (window.htmx) htmx.process(replacement);
        }
      })
      .catch((err) => console.error("CuTePi: delete failed", err));
  }
  // F8: jump the selection to the next broken cue (failed last play or  // missing source), wrapping at the sheet end. Fires the row's own select
  // endpoint so the whole app follows the moved selection.
  if (e.code === "F8") {
    e.preventDefault();
    const rows = [...document.querySelectorAll("#cuesheet tr.cue[data-cue-pos]")];
    const broken = (r) => r.dataset.cueResult === "2" || r.dataset.cueMissing === "true";
    // Anchor first, set member as fallback: the two can disagree (the
    // template sets them independently), and F8 must follow the visible
    // selection either way.
    const sel = document.querySelector("#cuesheet tr.cue.table-warning")
      || document.querySelector('#cuesheet tr.cue[data-cue-sel="1"]');
    let start = rows.findIndex((r) => r === sel) + 1;
    let hit = null;
    for (let n = 0; n < rows.length && !hit; n++) {
      const r = rows[(start + n) % rows.length];
      if (broken(r) && r !== sel) hit = r;
    }
    if (hit) {
      hit.scrollIntoView({block: "center"});
      htmx.ajax("POST", "/api/cue/" + hit.dataset.cuePos, {target: "#cuesheet", swap: "outerHTML"});
    }
  }
}, false);

// Media tile hover popout (§5.3): when the info overlay would spill out of
// a small tile, render the same details in a floating card near the tile.
(function () {
  function hidePopout(thumb) {
    if (thumb && thumb.__popout) {
      thumb.__popout.remove();
      thumb.__popout = null;
    }
    if (thumb) thumb.classList.remove("media-popout-active");
  }
  document.addEventListener("mouseover", (e) => {
    if (!(e.target instanceof Element)) return;
    const thumb = e.target.closest(".media-thumbnail");
    if (!thumb) return;
    const info = thumb.querySelector(".media-info");
    if (!info) return;
    // Rows are ellipsized (scrollWidth > clientWidth = truncated) and the
    // overlay clips, so "fits" needs both checks.
    const rows = info.querySelectorAll(".media-info-row, .media-info-name");
    const clipped = info.scrollHeight > thumb.clientHeight + 2 ||
      [...rows].some((r) => r.scrollWidth > r.clientWidth + 1);
    if (!clipped) return; // fits: normal overlay
    hidePopout(thumb);
    const pop = document.createElement("div");
    pop.className = "media-info-popout";
    pop.innerHTML = info.innerHTML;
    document.body.appendChild(pop);
    const r = thumb.getBoundingClientRect();
    const pr = pop.getBoundingClientRect();
    let top = r.top - pr.height - 6;
    if (top < 6) top = r.bottom + 6;
    const left = Math.max(6, Math.min(r.left, window.innerWidth - pr.width - 6));
    pop.style.left = left + "px";
    pop.style.top = top + "px";
    thumb.__popout = pop;
    thumb.classList.add("media-popout-active");
  });
  document.addEventListener("mouseout", (e) => {
    if (!(e.target instanceof Element)) return;
    const thumb = e.target.closest(".media-thumbnail");
    if (thumb && !(e.relatedTarget && thumb.contains(e.relatedTarget))) hidePopout(thumb);
  });
})();

// Header connection tooltip (§5.2): hovering the broadcast-pin shows a
// styled card with server/client connection facts.
(function () {
  let pop = null, hideTimer = null;
  async function show(pin) {
    try {
      const res = await fetch("/api/serverinfo");
      const info = await res.json();
      hide();
      pop = document.createElement("div");
      pop.className = "hover-popout";
      const up = info.uptimeS >= 3600
        ? Math.floor(info.uptimeS / 3600) + "h " + Math.floor((info.uptimeS % 3600) / 60) + "m"
        : Math.floor(info.uptimeS / 60) + "m " + (info.uptimeS % 60) + "s";
      pop.innerHTML =
        '<div><strong>' + (info.live ? "Live" : "Offline") + "</strong> — " + info.clients +
        " client" + (info.clients === 1 ? "" : "s") + " connected</div>" +
        "<div>Server uptime: " + up + "</div>" +
        "<div>Poll fallback: " + info.pollMs + " ms</div>" +
        '<div class="text-muted small">' + (info.live ? "Push updates over WebSocket" : "HTTP polling only — socket down") + "</div>";
      document.body.appendChild(pop);
      const r = pin.getBoundingClientRect();
      const pr = pop.getBoundingClientRect();
      const left = Math.max(6, Math.min(r.left, window.innerWidth - pr.width - 6));
      pop.style.left = left + "px";
      pop.style.top = (r.bottom + 6) + "px";
    } catch (err) { /* tooltip is best-effort */ }
  }
  function hide() { if (pop) { pop.remove(); pop = null; } }
  document.addEventListener("mouseover", (e) => {
    if (!(e.target instanceof Element)) return;
    if (e.target.closest("#ws-status")) { clearTimeout(hideTimer); show(e.target.closest("#ws-status")); }
  });
  document.addEventListener("mouseout", (e) => {
    if (!(e.target instanceof Element)) return;
    if (e.target.closest("#ws-status")) {
      clearTimeout(hideTimer);
      hideTimer = setTimeout(hide, 250);
    }
  });
})();

// Last filter the user typed; restored after #mediapool re-renders.
var mediaFilterState = { text: "", type: "all" };
function filterMedia() {
  var text = (document.getElementById("mediaFilterText") || {}).value || "";
  var type = (document.getElementById("mediaFilterType") || {}).value || "all";
  text = text.trim().toLowerCase();
  // Remember the live filter so a pool re-render can restore it (the inputs
  // live inside the swapped #mediapool partial and come back blank).
  mediaFilterState.text = text;
  mediaFilterState.type = type;
  var visible = 0, total = 0;
  document.querySelectorAll("#mediapool .media-tile").forEach(function (tile) {
    total++;
    var name = (tile.getAttribute("data-media-name") || "").toLowerCase();
    var tileType = tile.getAttribute("data-media-type") || "other";
    var matchesText = text === "" || name.indexOf(text) > -1;
    var matchesType = type === "all" || tileType === type;
    var show = matchesText && matchesType;
    tile.style.display = show ? "" : "none";
    if (show) visible++;
  });
  var noMatch = document.getElementById("media-no-match");
  if (noMatch) noMatch.hidden = visible !== 0 || total === 0;
}

// Restores the remembered filter into the freshly-rendered inputs, then
// re-applies it. Wired to the same re-render hook as filterMedia below.
function restoreMediaFilter() {
  var textEl = document.getElementById("mediaFilterText");
  var typeEl = document.getElementById("mediaFilterType");
  if (textEl) textEl.value = mediaFilterState.text;
  if (typeEl) typeEl.value = mediaFilterState.type;
  filterMedia();
}

// Re-apply the media-pool filters whenever htmx processes newly-swapped pool
// content (this covers both htmx swaps and the drag-and-drop path, which calls
// htmx.process()). The old "mediapool-updated" custom event had no dispatcher
// and was removed.
document.addEventListener("htmx:after:init", restoreMediaFilter);
filterMedia();

// --- Media pool side panel: collapse/expand + drag-to-resize ---
// Runs on the control-centre page only (guards on element presence, so it is
// a no-op on the standalone /upload page). The collapse button lives inside
// the re-rendered #mediapool partial, so it is wired via delegation.
(function () {
  const pane = document.getElementById("mediapool-pane");
  const resizer = document.getElementById("mediapool-resizer");
  if (!pane || !resizer) return;

  const STORAGE_WIDTH = "cutepi.mediapool.width";
  const STORAGE_COLLAPSED = "cutepi.mediapool.collapsed";
  const DEFAULT_WIDTH = 320;

  function getWidth() {
    try {
      const w = parseFloat(localStorage.getItem(STORAGE_WIDTH));
      return isNaN(w) || w < 160 ? DEFAULT_WIDTH : w;
    } catch (e) {
      return DEFAULT_WIDTH;
    }
  }
  function setWidth(w) {
    document.documentElement.style.setProperty("--mediapool-width", w + "px");
  }
  function setCollapsed(state) {
    document.body.classList.toggle("mediapool-collapsed", state);
    try {
      localStorage.setItem(STORAGE_COLLAPSED, state ? "1" : "0");
    } catch (e) {}
  }

  // Restore persisted state on load.
  setWidth(getWidth());
  try {
    setCollapsed(localStorage.getItem(STORAGE_COLLAPSED) === "1");
  } catch (e) {
    setCollapsed(false);
  }

  // Delegated: works even after #mediapool is re-rendered by an htmx swap.
  document.addEventListener("click", (e) => {
    if (e.target.closest("#mediapool-collapse-btn")) setCollapsed(true);
    else if (e.target.closest("#mediapool-expand-btn")) setCollapsed(false);
  });

  // Drag-to-resize via the splitter handle.
  resizer.addEventListener("mousedown", (e) => {
    e.preventDefault();
    const startX = e.clientX;
    const startWidth = pane.getBoundingClientRect().width;
    const minW = 160;
    const maxW = Math.max(minW, window.innerWidth * 0.7);

    const onMove = (ev) => {
      setWidth(Math.min(maxW, Math.max(minW, startWidth + (ev.clientX - startX))));
    };
    const onUp = () => {
      document.removeEventListener("mousemove", onMove);
      document.removeEventListener("mouseup", onUp);
      try {
        localStorage.setItem(STORAGE_WIDTH, String(pane.getBoundingClientRect().width));
      } catch (e) {}
    };
    document.addEventListener("mousemove", onMove);
    document.addEventListener("mouseup", onUp);
  });
})();

// Delete is handled outside htmx because the control lives inside a native
// draggable row. Capture the click before row-selection handlers can consume
// it, then replace the authoritative cuesheet returned by the server.
document.addEventListener("click", (e) => {
  const button = e.target instanceof Element ? e.target.closest("[data-cue-delete]") : null;
  if (!button) return;
  // Shift/Ctrl-click is multi-select (§12.4): let the selection handler own
  // it instead of deleting the cue out from under the operator.
  if (e.shiftKey || e.ctrlKey || e.metaKey) return;
  if (!needEditMode()) return;
  e.preventDefault();
  e.stopPropagation();
  button.disabled = true;
  fetch("/api/cue/" + encodeURIComponent(button.dataset.cueDelete), {method: "DELETE"})
    .then((res) => {
      if (!res.ok) throw new Error("server returned " + res.status);
      return res.text();
    })
    .then((html) => {
      const wrapper = document.createElement("div");
      wrapper.innerHTML = html.trim();
      const replacement = wrapper.firstElementChild;
      const current = document.getElementById("cuesheet");
      if (!replacement || replacement.id !== "cuesheet" || !current) {
        throw new Error("unexpected cuesheet response");
      }
      current.replaceWith(replacement);
      if (window.htmx) {
        htmx.process(replacement);
        if (document.getElementById("cueinspector-body")) {
          htmx.ajax("GET", "/api/cue/inspector", {target: "#cueinspector-body", swap: "outerHTML"});
        }
      }
    })
    .catch((err) => {
      button.disabled = false;
      console.error("CuTePi: delete cue failed", err);
      showToast("Delete cue failed: " + err.message);
    });
  }, true);

document.addEventListener("dragstart", (e) => {
    const bar = e.target.closest(".cue-progress-bar");
    if (bar) e.preventDefault(); // drag would conflict with the pointer-drag scrub
    const row = e.target.closest("tr[draggable]");
    if (row && row.querySelector(".cue-inline-edit.htmx-request")) e.preventDefault();
  });
  document.addEventListener("pointerdown", (e) => {
    const bar = e.target.closest(".cue-progress-bar");
    if (!bar) return;
    e.preventDefault();
    // Throttled to ~100ms: a raw seek per pointermove turns one drag into
    // dozens of POSTs, each a QueryDuration+SeekTime on the pipeline (and
    // serialized with everything else server-side).
    let lastSeek = 0;
    const seek = (ev) => {
      const now = Date.now();
      if (now - lastSeek < 100) return;
      lastSeek = now;
      const rect = bar.getBoundingClientRect();
      const ratio = (ev.clientX - rect.left) / rect.width;
      const dur = parseInt(bar.dataset.dur, 10) || 0;
      const position = ratio * dur; // ms
      const params = new URLSearchParams({position: "" + (position / 1000)});
      fetch("/api/seek", {method: "POST", headers: {"Content-Type": "application/x-www-form-urlencoded"}, body: params.toString()});
    };
    seek(e);
    const onMove = (ev) => seek(ev);
    const onUp = (ev) => {
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
      seek(ev); // land exactly where the drag ended, not at the last throttle window
    };
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
  });
  document.addEventListener("dragend", (e) => {
    const bar = e.target.closest(".cue-progress-bar");
    if (bar) e.preventDefault();
  });

// --- Cue context menu (right-click on a cue row) ---
// A lightweight single-instance menu offering per-cue actions: play, loop,
// colour, fade-and-stop-others (duration + scope), autofollow and delete.
// The menu element is created lazily and reused. Actions that mutate a cue
// column call PUT /api/cue/<pos>/edit/<col> and, on success, re-render the
// cuesheet (which the endpoint already returns) plus the inspector.
// The 12 named palette matches the Cue Inspector dropdown (routes/index.go).
// Rainbow order, then the neutral tones, matching the inspector palette.
const CUE_COLOURS = [
  ["Red", "#dc3545"], ["Orange", "#fd7e14"], ["Yellow", "#ffc107"],
  ["Green", "#28a745"], ["Teal", "#20c997"], ["Cyan", "#17a2b8"],
  ["Blue", "#007bff"], ["Indigo", "#6610f2"], ["Purple", "#a855f7"],
  ["Pink", "#e83e8c"], ["Brown", "#795548"], ["Grey", "#6c757d"],
];
function patternOptions() {
  // Each option is tinted with its colour so the dropdown reads as a graded
  // list; dark text keeps it readable on the deeper tints.
  return '<option value="">None</option>' + CUE_COLOURS.map((c) =>
    `<option value="${c[1]}" style="background-color:${c[1]}22">${c[0]}</option>`).join("");
}
(function () {
  let menuEl = null;

  function ensureMenu() {
    if (menuEl) return menuEl;
    menuEl = document.createElement("div");
    menuEl.id = "cue-context-menu";
    menuEl.className = "cue-context-menu";
    menuEl.hidden = true;
    // Play deliberately lives on the transport and the cue row, not here.
    menuEl.innerHTML = `
      <div class="cue-context-item cue-context-color">
        <i class="bi bi-palette"></i> Colour:
        <select title="Cue colour">${patternOptions()}</select>
      </div>
      <div class="cue-context-divider"></div>
      <div class="cue-context-item cue-context-fade">
        <i class="bi bi-volume-off"></i> Fade-stop others
        <select title="Scope"><option value="peers">Peers</option><option value="list">List/Cart</option><option value="all">All</option></select>
        <input type="text" placeholder="0:00" title="Fade/stop time (mm:ss)" value="0:00">
      </div>
      <div class="cue-context-item" data-cue-action="autofollow"><i class="bi bi-arrow-right-circle"></i> <span>Auto-continue: off</span></div>
      <div class="cue-context-divider"></div>
      <div class="cue-context-item" data-cue-action="newgroup"><i class="bi bi-folder-plus"></i> <span>New group</span></div>
      <div class="cue-context-divider"></div>
      <div class="cue-context-item cue-context-danger" data-cue-action="delete"><i class="bi bi-trash3"></i> Delete cue</div>`;
    document.body.appendChild(menuEl);
    return menuEl;
  }

  function hideMenu() {
    if (menuEl) menuEl.hidden = true;
  }

  // Re-render the cuesheet partial from the server after any mutation.
  // Also published on window: the sync/inspector-save handlers in other
  // closures call it (colour picks must reach the sheet immediately).
  window.refreshCuesheet = function () { return refreshCuesheet(); };
  function refreshCuesheet() {
    if (!window.htmx || !document.getElementById("cuesheet")) return;
    htmx.ajax("GET", "/api/cuesheet", {target: "#cuesheet", swap: "outerHTML"});
    if (document.getElementById("cueinspector-body")) {
      htmx.ajax("GET", "/api/cue/inspector", {target: "#cueinspector-body", swap: "outerHTML"});
    }
  }

  // Replace #cuesheet with a rendered partial and re-process its htmx attrs.
  function replaceCuesheet(html) {
    const wrapper = document.createElement("div");
    wrapper.innerHTML = html.trim();
    const replacement = wrapper.firstElementChild;
    const current = document.getElementById("cuesheet");
    if (replacement && replacement.id === "cuesheet" && current) {
      current.replaceWith(replacement);
      if (window.htmx) htmx.process(replacement);
    }
  }

  // PUT a new column value and refresh on success.
  function putCol(cuePos, col, val) {
    const params = new URLSearchParams({col, val});
    return fetch("/api/cue/" + encodeURIComponent(cuePos) + "/edit/" + encodeURIComponent(col), {
      method: "PUT",
      headers: {"Content-Type": "application/x-www-form-urlencoded"},
      body: params.toString(),
    }).then(async (res) => {
      if (!res.ok) {
        const text = await res.text().catch(() => "");
        throw new Error("update failed: " + res.status + " " + text);
      }
      refreshCuesheet();
    });
  }

  // Bulk PUT (§12.4): one operation over the multi-selection; single cue
  // falls back to the per-column PUT — except "group", which has no edit
  // column and always goes through the transactional bulk endpoint (the
  // server normalizes membership after the move).
  // Menu snapshot goes stale when the sheet re-renders (poll/WS) between
  // open and action: drop positions whose rows no longer exist, so a
  // reorder or delete from elsewhere can't retarget the action.
  function livePositions(snapshot) {
    const live = new Set(
      [...document.querySelectorAll('#cuesheet tr.cue[data-cue-pos]')]
        .map((r) => parseInt(r.dataset.cuePos, 10))
    );
    return (snapshot || []).filter((p) => live.has(p));
  }

  function bulkPut(op, value, singlePos, singleCol) {
    let positions = [];
    try { positions = JSON.parse(menuEl.dataset.bulk || "[]"); } catch (err) {}
    positions = livePositions(positions);
    if ((!positions || positions.length < 2) && op !== "group") {
      return putCol(singlePos, singleCol, value);
    }
    if (!positions || positions.length === 0) {
      positions = [parseInt(singlePos, 10)];
    }
    return fetch("/api/cue/bulk", {
      method: "POST",
      headers: {"Content-Type": "application/json"},
      body: JSON.stringify({op, value, positions}),
    }).then((res) => {
      if (!res.ok) throw new Error("server returned " + res.status);
      return res.text();
    }).then((html) => replaceCuesheet(html));
  }

  document.addEventListener("contextmenu", (e) => {
    const row = e.target instanceof Element ? e.target.closest("tr.cue[data-cue-pos]") : null;
    if (!row) return; // default browser menu elsewhere
    if (isShowMode()) return; // sheet locked: fall through to the native menu
    e.preventDefault();
    const m = ensureMenu();
    m.dataset.cuePos = row.dataset.cuePos;
    // Multi-selection (§12.4): remember the selected set so menu actions
    // apply to all of them; the anchor cue is the single-selection fallback.
    m.dataset.bulk = JSON.stringify(
      [...document.querySelectorAll('#cuesheet tr.cue[data-cue-sel="1"], #cuesheet tr.cue[data-cue-anchor="1"]')]
        .map((r) => parseInt(r.dataset.cuePos, 10))
    );
    m.dataset.cueAutoContinue = row.dataset.cueAutoContinue || "false";
    m.dataset.cueFadeAction = row.dataset.cueFadeAction || "peers";
    m.dataset.cueFadeOut = row.dataset.cueFadeOut || "0";
    m.dataset.cueColor = row.dataset.cueColor || "";

    // Auto-continue label
    m.querySelector('[data-cue-action="autofollow"] span').textContent =
      "Auto-continue: " + (m.dataset.cueAutoContinue === "true" ? "on" : "off");
    // "Add to New Group" for a multi-selection, plain "New group" for one.
    let bulkCount = 0;
    try { bulkCount = JSON.parse(m.dataset.bulk || "[]").length; } catch (err) {}
    m.querySelector('[data-cue-action="newgroup"] span').textContent =
      bulkCount > 1 ? "Add to New Group" : "New group";
    // Fade scope + time
    const fadeSel = m.querySelector(".cue-context-fade select");
    fadeSel.value = m.dataset.cueFadeAction;
    const fadeTime = m.querySelector(".cue-context-fade input[type=text]");
    fadeTime.value = msToClock((parseInt(m.dataset.cueFadeOut, 10) || 0));
    // Colour select (12 swatches + None), showing the cue's current colour.
    const col = m.querySelector(".cue-context-color select");
    if (col) col.value = m.dataset.cueColor || "";
    // Wire control persistence here at open time, not inside the click
    // handler below: keyboard-only use (Tab + arrows + Enter) fires change
    // with no preceding click, which previously never saved.
    fadeSel.onchange = () => {
      bulkPut("fadeAction", fadeSel.value, m.dataset.cuePos, "fadeAction").catch((err) =>
        console.error("CuTePi: fadeAction update failed", err));
    };
    fadeTime.onchange = () => {
      const seconds = parseFadeTime(fadeTime.value);
      if (seconds < 0) {
        console.error("CuTePi: invalid fade time", fadeTime.value);
        return;
      }
      const ms = Math.round(seconds * 1000);
      bulkPut("fadeOut", "" + ms, m.dataset.cuePos, "fadeOut").catch((err) =>
        console.error("CuTePi: fadeOut update failed", err));
    };
    if (col) {
      col.onchange = () => {
        bulkPut("color", col.value, m.dataset.cuePos, "color").catch((err) =>
          console.error("CuTePi: colour update failed", err));
      };
    }

    // Position near cursor, clamped to the viewport.
    const rw = m.offsetWidth, rh = m.offsetHeight;
    let x = e.clientX, y = e.clientY;
    if (x + rw > window.innerWidth) x = window.innerWidth - rw;
    if (y + rh > window.innerHeight) y = window.innerHeight - rh;
    m.style.left = x + "px";
    m.style.top = y + "px";
    m.hidden = false;
  });

  document.addEventListener("click", (e) => {
    if (menuEl && !menuEl.contains(e.target)) hideMenu();
  });
  window.addEventListener("blur", hideMenu);

  // Dispatch menu actions. Single handler: a duplicate registration here
  // made the autofollow toggle run twice per click (net zero - the operator's
  // on/off switch silently did nothing).
  document.addEventListener("click", (e) => {
    if (!menuEl || menuEl.hidden) return;
    const pos = menuEl.dataset.cuePos;
    const item = e.target.closest("[data-cue-action]");
    // Form controls inside the menu (colour/scope selects, fade time) handle
    // their own change events - a click on them must not match a parent
    // action row (it used to match the colour row's action and close the
    // menu before the dropdown could open).
    if (item && !e.target.closest("select, input")) {
      const action = item.dataset.cueAction;
      hideMenu();
      if (action === "autofollow") {
        const newVal = menuEl.dataset.cueAutoContinue === "true" ? "false" : "true";
        menuEl.dataset.cueAutoContinue = newVal;
        bulkPut("autoContinue", newVal, pos, "autoContinue");
      } else if (action === "delete") {
        let positions = [];
        try { positions = livePositions(JSON.parse(menuEl.dataset.bulk || "[]")); } catch (err) {}
        if (positions && positions.length > 1) {
          fetch("/api/cue/bulk", {
            method: "POST",
            headers: {"Content-Type": "application/json"},
            body: JSON.stringify({op: "delete", value: "", positions}),
          })
            .then((res) => {
              if (!res.ok) throw new Error("server returned " + res.status);
              return res.text();
            })
            .then((html) => {
              replaceCuesheet(html);
            })
            .catch((err) => {
              console.error("CuTePi: bulk delete failed", err);
              showToast("Bulk delete failed: " + err.message);
            });
        }
        else {
          fetch("/api/cue/" + encodeURIComponent(pos), {method: "DELETE"})
            .then((res) => {
              if (!res.ok) throw new Error("server returned " + res.status);
              return res.text();
            })
            .then((html) => {
              replaceCuesheet(html);
            })
            .catch((err) => {
              console.error("CuTePi: cue delete failed", err);
              showToast("Delete failed: " + err.message);
            });
        }
      } else if (action === "newgroup") {
        let positions = [];
        try { positions = livePositions(JSON.parse(menuEl.dataset.bulk || "[]")); } catch (err) {}
        const at = parseInt(menuEl.dataset.cuePos, 10) || 0;
        if (!positions || positions.length === 0) {
          if (!at) return;
          positions = [at];
        }
        // One path: a new folder at the right-clicked cue's slot holding
        // the selection (one cue or many).
        fetch("/api/cue/bulkgroupnew", {
          method: "POST",
          headers: {"Content-Type": "application/json"},
          body: JSON.stringify({positions, at}),
        })
          .then((res) => {
            if (!res.ok) throw new Error("server returned " + res.status);
            return res.text();
          })
          .then((html) => {
            replaceCuesheet(html);
          })
          .catch((err) => {
            console.error("CuTePi: group create failed", err);
            showToast("Group create failed: " + err.message);
          });
      }
      return;
    }
  });

  // Format milliseconds as mm:ss (dropping minutes when 0). -1 <=> invalid.
  function msToClock(ms) {
    if (!ms || ms < 0) return "0:00";
    const totalSec = Math.round(ms / 1000);
    const min = Math.floor(totalSec / 60);
    const sec = totalSec % 60;
    return min + ":" + (sec < 10 ? "0" : "") + sec;
  }

  // Parse a clock or bare number into seconds; -1 signals invalid.
  function parseFadeTime(raw) {
    const s = (raw || "").trim();
    if (!s) return 0;
    if (s.indexOf(":") !== -1) {
      const parts = s.split(":");
      if (parts.length !== 2) return -1;
      const min = parseInt(parts[0], 10);
      const sec = parseFloat(parts[1]);
      if (isNaN(min) || isNaN(sec) || min < 0 || sec < 0) return -1;
      return min * 60 + sec;
    }
    const v = parseFloat(s);
    return isNaN(v) || v < 0 ? -1 : v;
  }
})();

// --- Group right-click menu (cuesheet group header rows) ---
// Mirrors the cue context menu: right-click a group row for Play,
// Inspector, Collapse/Expand and Delete.
(function () {
  let menuEl = null;

  function ensureMenu() {
    if (menuEl) return menuEl;
    menuEl = document.createElement("div");
    menuEl.id = "group-context-menu";
    menuEl.className = "cue-context-menu";
    menuEl.hidden = true;
    menuEl.innerHTML = `
      <div class="cue-context-item" data-group-action="play"><i class="bi bi-play-fill"></i> Play</div>
      <div class="cue-context-item" data-group-action="inspector"><i class="bi bi-sliders"></i> Inspector</div>
      <div class="cue-context-item" data-group-action="collapse"><i class="bi bi-chevron-down"></i> <span>Collapse</span></div>
      <div class="cue-context-item cue-context-color">
        <i class="bi bi-palette"></i> Colour:
        <select title="Group colour">${patternOptions()}</select>
      </div>
      <div class="cue-context-divider"></div>
      <div class="cue-context-item" data-group-action="newgroup"><i class="bi bi-folder-plus"></i> New group</div>
      <div class="cue-context-divider"></div>
      <div class="cue-context-item cue-context-danger" data-group-action="delete"><i class="bi bi-trash3"></i> Delete group</div>`;
    document.body.appendChild(menuEl);
    return menuEl;
  }

  function hideMenu() {
    if (menuEl) menuEl.hidden = true;
  }

  // POST an endpoint that returns the whole cuesheet partial and splice it in.
  function postCuesheet(url, method, body) {
    fetch(url, {method: method || "POST", body})
      .then((res) => {
        if (!res.ok) throw new Error("server returned " + res.status);
        return res.text();
      })
      .then((html) => {
        const wrapper = document.createElement("div");
        wrapper.innerHTML = html.trim();
        const replacement = wrapper.firstElementChild;
        const current = document.getElementById("cuesheet");
        if (replacement && replacement.id === "cuesheet" && current) {
          current.replaceWith(replacement);
          if (window.htmx) htmx.process(replacement);
        }
      })
      .catch((err) => {
        console.error("CuTePi: group action failed", err);
        showToast("Group action failed: " + err.message);
      });
  }

  document.addEventListener("contextmenu", (e) => {
    const row = e.target instanceof Element ? e.target.closest("tr.cue-group-header") : null;
    if (!row) return;
    if (isShowMode()) return; // sheet locked: fall through to the native menu
    e.preventDefault();
    const m = ensureMenu();
    m.dataset.groupId = row.dataset.groupId || "";
    const collapsed = row.dataset.groupCollapsed === "true";
    m.querySelector('[data-group-action="collapse"] span').textContent =
      collapsed ? "Expand" : "Collapse";
    // Colour select shows the group's stored colour and persists on change.
    const col = m.querySelector(".cue-context-color select");
    if (col) {
      col.value = row.dataset.cueColor || "";
      col.onchange = () => {
        const form = new FormData();
        form.append("color", col.value);
        postCuesheet("/api/group/" + (m.dataset.groupId || "") + "/color", "POST", form);
      };
    }
    const rw = m.offsetWidth, rh = m.offsetHeight;
    let x = e.clientX, y = e.clientY;
    if (x + rw > window.innerWidth) x = window.innerWidth - rw;
    if (y + rh > window.innerHeight) y = window.innerHeight - rh;
    m.style.left = x + "px";
    m.style.top = y + "px";
    m.hidden = false;
  });

  document.addEventListener("click", (e) => {
    if (menuEl && !menuEl.contains(e.target)) hideMenu();
  });
  window.addEventListener("blur", hideMenu);

  document.addEventListener("click", (e) => {
    if (!menuEl || menuEl.hidden) return;
    const id = menuEl.dataset.groupId;
    if (!id) return;
    const item = e.target.closest("[data-group-action]");
    if (!item) return;
    e.stopPropagation();
    const action = item.dataset.groupAction;
    hideMenu();
    if (action === "play") {
      postCuesheet("/api/group/" + id + "/play"); // fire-and-forget; play returns 200
    } else if (action === "inspector") {
      if (window.htmx && document.getElementById("cueinspector-collapse")) {
        htmx.ajax("GET", "/api/group/" + id + "/inspector",
          {target: "#cueinspector-collapse", swap: "innerHTML"});
      }
    } else if (action === "collapse") {
      postCuesheet("/api/group/" + id + "/collapse");
    } else if (action === "newgroup") {
      postCuesheet("/api/group/add");
    } else if (action === "delete") {
      postCuesheet("/api/group/" + id, "DELETE");
      // The group inspector panel was showing the deleted group: reset it to
      // the cue-inspector empty state, or every later auto-refresh keeps
      // refetching /api/group/<gone>/inspector (404 toasts).
      if (window.htmx && document.getElementById("cueinspector-collapse")) {
        htmx.ajax("GET", "/api/cue/inspector", { target: "#cueinspector-collapse", swap: "innerHTML" });
      }
    }
  });
})();

// --- Blank-space context menu: right-click empty cuesheet area offers
// New group (row menus keep their own right-click behaviour) ---
(function () {
  let menuEl = null;
  function ensureMenu() {
    if (menuEl) return menuEl;
    menuEl = document.createElement("div");
    menuEl.id = "sheet-context-menu";
    menuEl.className = "cue-context-menu";
    menuEl.hidden = true;
    menuEl.innerHTML = `
      <div class="cue-context-item" data-sheet-action="newgroup"><i class="bi bi-folder-plus"></i> New group</div>`;
    document.body.appendChild(menuEl);
    // Clicks anywhere else dismiss the menu (same contract as the other menus).
    document.addEventListener("pointerdown", (e) => {
      if (menuEl && !menuEl.contains(e.target)) menuEl.hidden = true;
    });
    menuEl.addEventListener("click", (e) => {
      const item = e.target instanceof Element ? e.target.closest("[data-sheet-action]") : null;
      if (!item) return;
      menuEl.hidden = true;
      if (window.htmx) {
        htmx.ajax("POST", "/api/group/add", {target: "#cuesheet", swap: "outerHTML"});
      }
    });
    return menuEl;
  }
  document.addEventListener("contextmenu", (e) => {
    const t = e.target instanceof Element ? e.target : null;
    if (!t || !t.closest("#cuesheet")) return;
    // Cue rows and group headers own their menus; blank space (table body
    // gaps, the blank-row filler, empty sheet) is ours.
    if (t.closest("tr.cue[data-cue-pos]") || t.closest(".cue-group-header")) return;
    if (isShowMode()) return; // sheet locked: native menu
    e.preventDefault();
    const m = ensureMenu();
    const rw = m.offsetWidth, rh = m.offsetHeight;
    let x = e.clientX, y = e.clientY;
    if (x + rw > window.innerWidth) x = window.innerWidth - rw;
    if (y + rh > window.innerHeight) y = window.innerHeight - rh;
    m.style.left = x + "px";
    m.style.top = y + "px";
    m.hidden = false;
  });
})();

// --- Media pool context menu (right-click or three-dot on a tile) ---
// Single shared element, shown at cursor. Actions call the same
// API endpoints the old Bootstrap dropdown used (Play / Load /
// Add / Delete / Refresh thumbnail / Analyse).
(function () {
  let menuEl = null;

  function ensureMenu() {
    if (menuEl) return menuEl;
    menuEl = document.createElement("div");
    menuEl.id = "media-context-menu";
    menuEl.className = "cue-context-menu";
    menuEl.hidden = true;
    menuEl.innerHTML = `
      <button type="button" class="cue-context-item" data-media-action="play"><i class="bi bi-play-fill"></i> Play</button>
      <button type="button" class="cue-context-item" data-media-action="load"><i class="bi bi-download"></i> Load</button>
      <button type="button" class="cue-context-item" data-media-action="add"><i class="bi bi-plus-circle"></i> Add</button>
      <div class="cue-context-divider"></div>
      <button type="button" class="cue-context-item" data-media-action="refresh"><i class="bi bi-camera"></i> Refresh thumbnail</button>
      <button type="button" class="cue-context-item" data-media-action="analyse"><i class="bi bi-music-note-beamed"></i> Analyse</button>
      <button type="button" class="cue-context-item" data-media-action="panichold"><i class="bi bi-life-preserver"></i> Set as holding image</button>
      <button type="button" class="cue-context-item" data-media-action="testpattern"><i class="bi bi-tv"></i> <span>Add to test patterns</span></button>
      <div class="cue-context-divider"></div>
      <button type="button" class="cue-context-item cue-context-danger" data-media-action="delete"><i class="bi bi-trash3"></i> Delete</button>`;
    document.body.appendChild(menuEl);
    return menuEl;
  }

  function hideMenu() {
    if (menuEl) menuEl.hidden = true;
  }

  function positionMenuAt(x, y) {
    const m = ensureMenu();
    m.hidden = false;
    const rw = m.offsetWidth, rh = m.offsetHeight;
    if (x + rw > window.innerWidth) x = window.innerWidth - rw;
    if (y + rh > window.innerHeight) y = window.innerHeight - rh;
    m.style.left = Math.max(0, x) + "px";
    m.style.top = Math.max(0, y) + "px";
    m.hidden = false;
  }

  // Right-click on a tile.
  document.addEventListener("contextmenu", (e) => {
    const tile = e.target.closest("figure.media-tile");
    if (!tile) return;
    e.preventDefault();
    const m = ensureMenu();
    m.dataset.mediaFilename = tile.dataset.mediaName || "";
    // Reflect the test-pattern pin state on the menu item's label.
    fetch("/api/testpatterns").then((r) => r.json()).then((d) => {
      const pinned = (d.custom || []).indexOf(m.dataset.mediaFilename) > -1;
      const lbl = m.querySelector('[data-media-action="testpattern"] span');
      if (lbl) lbl.textContent = pinned ? "Remove from test patterns" : "Add to test patterns";
    }).catch(() => {});
    positionMenuAt(e.clientX, e.clientY);
  });

  // Three-dot button toggles the menu below/above itself.
  document.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-media-menu]");
    if (!btn) return;
    e.preventDefault();
    e.stopPropagation();
    const m = ensureMenu();
    if (!m.hidden && m.dataset.mediaFilename === btn.dataset.mediaMenu) {
      hideMenu();
      return;
    }
    m.dataset.mediaFilename = btn.dataset.mediaMenu || "";
    const rect = btn.getBoundingClientRect();
    positionMenuAt(rect.left, rect.bottom + 4);
    m.querySelector("button").focus();
  });

  // Click outside or blur hides.
  document.addEventListener("click", (e) => {
    if (menuEl && !menuEl.contains(e.target) && !e.target.closest("[data-media-menu]")) hideMenu();
  });
  window.addEventListener("blur", hideMenu);
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") hideMenu(); });

  // Add a media item to the cuesheet (shared by the context menu and the
  // tile's double-click / Enter gesture).
  function addMediaToCuesheet(filename) {
    if (!needEditMode()) return Promise.reject(new Error("show mode"));
    return fetch("/api/cue/add/" + encodeURIComponent(filename), {method: "POST"})
      .then((res) => {
        if (!res.ok) throw new Error("server returned " + res.status);
        return res.text();
      })
      .then((html) => {
        const wrapper = document.createElement("div");
        wrapper.innerHTML = html.trim();
        const replacement = wrapper.firstElementChild;
        const current = document.getElementById("cuesheet");
        if (replacement && replacement.id === "cuesheet" && current) {
          current.replaceWith(replacement);
          if (window.htmx) htmx.process(replacement);
        }
      })
      .catch((err) => {
        console.error("CuTePi: add cue failed", err);
        showToast("Add cue failed: " + err.message);
      });
  }

  // Double-click or Enter on a tile adds it to the cuesheet — the tile used
  // to have no direct action at all, only drag and the context menu.
  document.addEventListener("dblclick", (e) => {
    const tile = e.target instanceof Element ? e.target.closest("#mediapool .media-tile") : null;
    if (!tile) return;
    addMediaToCuesheet(tile.dataset.mediaName || "");
  });
  document.addEventListener("keydown", (e) => {
    if (e.key !== "Enter") return;
    const tile = e.target instanceof Element ? e.target.closest("#mediapool .media-tile") : null;
    if (!tile) return;
    e.preventDefault();
    addMediaToCuesheet(tile.dataset.mediaName || "");
  });

  // Action dispatch.
  document.addEventListener("click", (e) => {
    if (!menuEl || menuEl.hidden) return;
    const item = e.target.closest("[data-media-action]");
    if (!item) return;
    e.preventDefault();
    e.stopPropagation();
    const action = item.dataset.mediaAction;
    const filename = menuEl.dataset.mediaFilename || "";
    hideMenu();
    if (action === "play") {
      fetch("/api/play/" + encodeURIComponent(filename), {method: "POST"});
    } else if (action === "load") {
      fetch("/api/load/" + encodeURIComponent(filename), {method: "POST"});
    } else if (action === "add") {
      addMediaToCuesheet(filename);
    } else if (action === "refresh") {
      fetch("/api/media/" + encodeURIComponent(filename) + "/refreshThumbnail", {method: "POST"})
        .catch((err) => console.error("CuTePi: refresh thumbnail failed", err));
    } else if (action === "panichold") {
      const form = new URLSearchParams({filename});
      fetch("/api/setting/panichold", {method: "POST", body: form})
        .then((res) => {
          if (!res.ok) throw new Error("server returned " + res.status);
          showToast("Holding image set — PANIC now cuts to " + filename);
        })
        .catch((err) => {
          console.error("CuTePi: holding image failed", err);
          showToast("Holding image failed: " + err.message);
        });
    } else if (action === "testpattern") {
      const pinned = item.querySelector("span").textContent.startsWith("Remove");
      const form = new URLSearchParams({on: pinned ? "0" : "1"});
      fetch("/api/testpattern/" + encodeURIComponent(filename), {method: "POST", body: form})
        .then((res) => {
          if (!res.ok) throw new Error("server returned " + res.status);
          showToast(pinned ? "Removed from test patterns" : "Added to test patterns");
        })
        .catch((err) => {
          console.error("CuTePi: test pattern update failed", err);
          showToast("Test pattern update failed: " + err.message);
        });
    } else if (action === "analyse") {
      fetch("/api/media/" + encodeURIComponent(filename) + "/analyse", {method: "POST"})
        .catch((err) => console.error("CuTePi: analyse failed", err));
    } else if (action === "delete") {
      fetch("/api/media/" + encodeURIComponent(filename), {method: "DELETE"})
        .then((res) => {
          if (!res.ok) throw new Error("server returned " + res.status);
          return res.text();
        })
        .then((html) => {
          const wrapper = document.createElement("div");
          wrapper.innerHTML = html.trim();
          const replacement = wrapper.firstElementChild;
          const current = document.getElementById("mediapool");
          if (replacement && current) {
            current.replaceWith(replacement);
            if (window.htmx) htmx.process(replacement);
          }
        })
        .catch((err) => {
          console.error("CuTePi: delete media failed", err);
          showToast("Delete failed: " + err.message);
        });
    }
  });
})();

// --- Now Playing: change-detection polling ---
// Replaces the old hx-trigger="every 500ms" full re-render of #mediainfo.
// Polls the lightweight GET /api/nowplaying/status endpoint, which returns
// {"changed": bool, "version": N} where version is a server-side monotonic
// counter bumped on real playback state changes and position ticks. The
// widget is re-rendered (via GET /api/nowplaying) only when changed is true
// and the version differs from what this client last saw, so idle/paused
// playback no longer re-renders every 500ms. Each client tracks its own
// last-seen version; the server keeps no per-client state, so concurrent
// clients work. No-op on pages without #mediainfo (e.g. /upload).
(function () {
  if (!document.getElementById("mediainfo")) return;

  let pollMs = 500;
  let lastSeen = 0;
  let fallback = null;

  fetch("/api/settings")
    .then((res) => res.json())
    .then((settings) => {
      if (Number.isFinite(settings.pollInterval) && settings.pollInterval >= 10) {
        pollMs = settings.pollInterval;
      }
    })
    .catch(() => {})
    .finally(() => setFallback(true)); // poll until the socket proves itself

  // HTTP polling is the WS-disconnected fallback ONLY: while the socket is
  // up, every server signal (cutepi-sync) pulls once, version-guarded. The
  // server pushes one sync per displayed second while playing (gsp ticker),
  // so the progress clock advances without any timer-driven requests.
  function setFallback(on) {
    if (on && !fallback) fallback = setInterval(refresh, pollMs);
    if (!on && fallback) { clearInterval(fallback); fallback = null; }
  }
  document.addEventListener("cutepi-ws", (e) => setFallback(!e.detail.connected));

  async function refresh() {
    try {
      const status = await fetch(
        "/api/nowplaying/status?version=" + lastSeen,
        { headers: { "Accept": "application/json" } }
      );
      const body = await status.json();
      lastSeen = body.version;
      if (!body.changed) return;
      // While the user is actively dragging the scrubber, don't replace the
      // widget (the swap would reset the thumb mid-drag). The next signal
      // catches up once the drag ends.
      const scrubber = document.querySelector("#nowplaying-scrubber");
      if (scrubber && scrubber.matches(":active")) return;
      const res = await fetch("/api/nowplaying", { headers: { "Accept": "text/html" } });
      const el = document.getElementById("mediainfo");
      if (el) el.outerHTML = await res.text();
    } catch (e) {
      // Transient network/server error; the fallback timer retries.
    }
  }
  // WebSocket "sync" wakes this poller immediately (single writer for the
  // widget, same rationale as the cuesheet poller).
  document.addEventListener("cutepi-sync", refresh);

})();

// --- Cue Inspector: auto-refresh when the cuesheet re-renders ---
// The inspector is server-rendered for the current selection; every cuesheet
// swap (row click / arrow keys / add/delete/move / poller / WebSocket / DnD)
// may change the selection, so re-fetch the inspector partial to follow it.
// Two entry points feed this: htmx:after:swap (for htmx-driven swaps of
// #cuesheet) and a MutationObserver (for swaps that bypass htmx). They can
// both fire for one render, and GET caches can return a stale inspector for a
// different cue, so the refresh is debounced and cache-busted: exactly one
// fetch per render cycle, always fetching the current server selection.
(function () {
  const bodyEl = document.getElementById("cueinspector-body");
  if (!bodyEl) return;

  let timer = null;
  // Ordering guard: while an inspector save (PUT from #inspector-form) is in
  // flight, a WS signal may schedule an inspector refetch that would swap a
  // PRE-save render (and reset the just-dragged trim handles). Defer any
  // such refresh until the save settles, then fetch once.
  let saveInFlight = false;
  let refreshDirty = false;
  // The auto-follow must mirror what the operator is looking at, not only the
  // persisted selection: a group inspector can be open for a group that was
  // never selected (right-click > Inspector). After a group save swaps the
  // cuesheet, re-fetch the SELECTED entity from the freshly-rendered sheet -
  // and keep a group inspector alive when the sheet selects nothing.
  function inspectorURL() {
    const selected = document.querySelector("#cuesheet tr.table-warning");
    const body = document.getElementById("cueinspector-body");
    const shown = body ? parseInt(body.dataset.groupId || "", 10) : 0;
    if (selected) {
      const selGroup = selected.classList.contains("cue-group-header")
        ? parseInt(selected.dataset.groupId || "", 10)
        : 0;
      if (selGroup > 0) {
        return "/api/group/" + selGroup + "/inspector?_=" + Date.now();
      }
      return "/api/cue/inspector?_=" + Date.now();
    }
    if (shown > 0) {
      return "/api/group/" + shown + "/inspector?_=" + Date.now();
    }
    return "/api/cue/inspector?_=" + Date.now();
  }
  function refresh() {
    if (saveInFlight) { refreshDirty = true; return; }
    clearTimeout(timer);
    timer = setTimeout(() => {
      // Cache-bust: without no-store headers the browser may reuse a cached
      // inspector for the previously selected entity and the wrong panel
      // appears.
      const url = inspectorURL();
      try { htmx.ajax("GET", url, {target: "#cueinspector-body", swap: "outerHTML"}); } catch (e) {}
    }, 0);
  }

  document.body.addEventListener("htmx:after:swap", (e) => {
    const t = e.detail.ctx?.target;
    if (t && t.id === "cuesheet") refresh();
  });

  // Only colour and auto-continue are visible on the cuesheet; rate, trim,
  // volume saves must not churn the sheet (full re-render = hover/scroll churn).
  let sheetField = false;
  document.addEventListener("change", (e) => {
    const t = e.target instanceof Element ? e.target : null;
    sheetField = !!(t && t.closest("#inspector-form") &&
      ["color", "autoContinue"].indexOf(t.name) > -1);
  }, true);
  const inspectorRequest = (e) => {
    const ctx = e.detail?.ctx;
    const action = ctx?.request?.action || "";
    return ctx?.sourceElement?.id === "inspector-form" &&
      action.includes("/api/cue/inspector/");
  };
  document.body.addEventListener("htmx:before:request", (e) => {
    if (inspectorRequest(e)) saveInFlight = true;
  });
  document.body.addEventListener("htmx:after:request", (e) => {
    if (inspectorRequest(e)) {
      saveInFlight = false;
      if (refreshDirty) { refreshDirty = false; refresh(); }
      if (sheetField) { sheetField = false; refreshCuesheet(); }
    }
  });

  // Watch the cuesheet's PERSISTENT container, not the #cuesheet element
  // itself: every swap of any kind (htmx outerHTML, DnD replaceById, the
  // poller's replaceWith) replaces the #cuesheet node, so an observer bound
  // to that node dies on the first swap and never fires again. A new
  // #cuesheet appearing inside the pane means the sheet re-rendered and the
  // selection may have moved — re-fetch the inspector to follow it.
  const pane = document.getElementById("cuesheet-pane") || document.body;
  const observer = new MutationObserver((muts) => {
    for (const m of muts) {
      for (const n of m.addedNodes) {
        if (n.nodeType === Node.ELEMENT_NODE && (n.id === "cuesheet" || n.querySelector("#cuesheet"))) {
          refresh();
          return;
        }
      }
    }
  });
  observer.observe(pane, { childList: true, subtree: true });
})();

// Keep the inspector's read-only dB readout in sync with its vertical slider.
document.addEventListener("input", (e) => {
  if (e.target.id === "insp-volume") {
    const db = document.getElementById("insp-volume-db");
    if (db) db.value = e.target.value;
  }
  if (e.target.id === "insp-volume-db" && e.target.value !== "" && e.target.validity.valid) {
    document.getElementById("insp-volume").value = e.target.value;
  }
  if (e.target.matches("[data-fade-in]")) {
    document.querySelectorAll("[data-fade-in]").forEach(input => { input.value = e.target.value; });
  }
  for (const id of ["insp-rate", "insp-balance"]) {
    if (e.target.id === id) document.getElementById(id + "-value").value = e.target.value + (id === "insp-rate" ? "×" : "");
  }
});

document.addEventListener("click", (e) => {
  const button = e.target.closest("[data-volume-step], [data-volume-reset], [data-rate-reset]");
  if (!button) return;
  const rate = button.hasAttribute("data-rate-reset");
  const input = document.getElementById(rate ? "insp-rate" : "insp-volume");
  input.value = rate ? 1 : button.hasAttribute("data-volume-reset") ? 0 : Number(input.value) + Number(button.dataset.volumeStep);
  input.dispatchEvent(new Event("input", {bubbles:true}));
  input.dispatchEvent(new Event("change", {bubbles:true}));
});

// --- Cue Inspector: docked bottom panel (mirrors the mediapool pane) -----
// Collapse-to-fully-hidden with a grab-bar, drag-to-resize height, tabbed
// content (Time / Audio / Colour) persisted per browser, and a silent
// save-flash on every committed change.
(function () {
  const STORAGE_COLLAPSED = "cutepi.inspector.collapsed";
  const STORAGE_HEIGHT = "cutepi.inspector.height";
  const STORAGE_TAB = "cutepi.inspector.tab";
  const DEFAULT_HEIGHT = 320;
  const MIN_HEIGHT = 96;

  function setHeight(h) {
    document.documentElement.style.setProperty("--cueinspector-height", h + "px");
  }
  function getHeight() {
    try {
      const h = parseFloat(localStorage.getItem(STORAGE_HEIGHT));
      return isNaN(h) || h < MIN_HEIGHT ? DEFAULT_HEIGHT : h;
    } catch (e) { return DEFAULT_HEIGHT; }
  }
  function setCollapsed(state) {
    document.body.classList.toggle("cueinspector-collapsed", state);
    try { localStorage.setItem(STORAGE_COLLAPSED, state ? "1" : "0"); } catch (e) {}
  }
  // Tabs are marked active on the tabstrip AND on the pane container; the
  // container's data attribute is refreshed on every inspector swap, so the
  // choice survives partial re-renders.
  function setTab(tab) {
    if (["audio", "video"].includes(tab) && !document.querySelector(`.inspector-tabpane[data-tabpane="${tab}"]`)) {
      tab = "time";
    }
    const strip = document.getElementById("inspector-tabs");
    if (strip) {
      strip.querySelectorAll("[data-inspector-tab]").forEach((b) => {
        const active = b.dataset.inspectorTab === tab;
        b.classList.toggle("active", active);
        b.setAttribute("aria-selected", active ? "true" : "false");
      });
    }
    document.querySelectorAll(".inspector-tabpanes").forEach((p) => {
      p.dataset.activeTab = tab;
    });
    // The media grid's column count depends on the now-visible pane's height.
    if (window.__fitMediaGrid) window.__fitMediaGrid();
    try { localStorage.setItem(STORAGE_TAB, tab); } catch (e) {}
  }
  function storedTab() {
    const t = localStorage.getItem(STORAGE_TAB);
    return ["time", "video", "audio", "auto", "media", "advanced"].includes(t) ? t : "time";
  }
  // Arrow-key/click selection can land the chosen cue outside the visible
  // part of a full cuesheet; pull the nearest scroller so the row shows.
  function scrollSelectionIntoView() {
    const sel = document.querySelector("#cuesheet .table-warning");
    if (sel) sel.scrollIntoView({ block: "nearest" });
  }
  // Images have no audio playback at all: hide the Audio tab (button + pane)
  // while an image cue is selected, and don't let a stored "audio" tab land
  // on a hidden pane.
  function syncAudioTab() {
    const body = document.querySelector("#cueinspector-body");
    const noAudio = !!body && body.hasAttribute("data-no-audio-tab");
    document.body.classList.toggle("ctp-no-audio-tab", noAudio);
    if (noAudio && ["audio", "video"].includes(storedTab())) setTab("time");
  }
  // A group inspector has no tabs: disable the strip while it's showing.
  function syncGroupView() {
    document.body.classList.toggle("inspector-groupview",
      !!document.querySelector("#cueinspector-collapse .groupinspector-body"));
  }

  setHeight(getHeight());
  try { setCollapsed(localStorage.getItem(STORAGE_COLLAPSED) === "1"); } catch (e) {}
  setTab(storedTab());
  syncGroupView();
  syncAudioTab();

  document.addEventListener("click", (e) => {
    if (!(e.target instanceof Element)) return;
    if (e.target.closest("#cueinspector-collapse-btn")) setCollapsed(true);
    else if (e.target.closest("#cueinspector-expand-btn")) setCollapsed(false);
    const tabBtn = e.target.closest("[data-inspector-tab]");
    if (tabBtn) setTab(tabBtn.dataset.inspectorTab);
  });

  // Drag-to-resize the panel height via the grab-bar above it.
  const resizer = document.getElementById("cueinspector-resizer");
  if (resizer) {
    resizer.addEventListener("mousedown", (e) => {
      e.preventDefault();
      const startY = e.clientY;
      const startH = document.getElementById("cueinspector-panel").getBoundingClientRect().height;
      const maxH = Math.max(MIN_HEIGHT, window.innerHeight * 0.6);
      const onMove = (ev) => setHeight(Math.min(maxH, Math.max(MIN_HEIGHT, startH - (ev.clientY - startY))));
      const onUp = () => {
        document.removeEventListener("mousemove", onMove);
        document.removeEventListener("mouseup", onUp);
        try {
          localStorage.setItem(STORAGE_HEIGHT, document.getElementById("cueinspector-panel").getBoundingClientRect().height + "");
        } catch (err) {}
      };
      document.addEventListener("mousemove", onMove);
      document.addEventListener("mouseup", onUp);
    });
  }

  // Silent save flash: pulse the little square whenever an inspector save
  // commits (see the sequenced-swaps setup for the same request detection).
  document.body.addEventListener("htmx:after:request", (e) => {
    const ctx = e.detail?.ctx;
    const action = ctx?.request?.action || "";
    if (ctx?.sourceElement?.id === "inspector-form" && action.includes("/api/cue/inspector/")) {
      const dot = document.getElementById("inspector-save-flash");
      if (dot) {
        dot.classList.add("saved");
        setTimeout(() => dot.classList.remove("saved"), 500);
      }
    }
  });

  // After any inspector swap: re-apply the stored tab and group-view state.
  document.body.addEventListener("htmx:after:swap", (e) => {
    const t = e.detail?.ctx?.target;
    if (!t) return;
    if (t.id === "cueinspector-body" || t.id === "cueinspector-collapse") {
      syncAudioTab();
      setTab(storedTab());
      syncGroupView();
      return;
    }
    if (t.id === "cuesheet") scrollSelectionIntoView();
  });
})();

// --- Fullscreen toggle (topbar button); icon/title follow the state ---
(function () {
  function sync() {
    const b = document.getElementById("fullscreen-btn");
    if (!b) return;
    const fs = !!document.fullscreenElement;
    b.dataset.fullscreenState = fs ? "on" : "off";
    b.title = fs ? "Exit fullscreen" : "Enter fullscreen";
    b.setAttribute("aria-label", b.title);
    const i = b.querySelector("i");
    if (i) i.className = fs ? "bi bi-fullscreen-exit" : "bi bi-arrows-fullscreen";
  }
  document.addEventListener("click", (e) => {
    if (!(e.target instanceof Element) || !e.target.closest("#fullscreen-btn")) return;
    if (document.fullscreenElement) document.exitFullscreen();
    else document.documentElement.requestFullscreen().catch(() => {});
  });
  document.addEventListener("fullscreenchange", sync);
  sync();
})();

// --- Mediapool thumbnail column stepper (persisted per browser) ---
(function () {
  const KEY = "cutepi.mediaCols";
  const MIN = 1, MAX = 6;
  function current() {
    const stored = parseInt(localStorage.getItem(KEY) || "", 10);
    return stored >= MIN && stored <= MAX ? stored : 2;
  }
  function applyStored() {
    document.querySelectorAll(".media-grid").forEach((g) =>
      g.style.setProperty("--media-cols", String(current())));
  }
  // Wire the + / − steppers in the mediapool header (delegated, survives
  // partial re-renders).
  document.body.addEventListener("click", (e) => {
    if (!(e.target instanceof Element)) return;
    const less = e.target.closest("#media-cols-less");
    const more = e.target.closest("#media-cols-more");
    if (!less && !more) return;
    let cols = current() + (more ? 1 : -1);
    cols = Math.max(MIN, Math.min(MAX, cols));
    try { localStorage.setItem(KEY, String(cols)); } catch (err) {}
    applyStored();
  });
  applyStored();
  // Re-apply when the pool re-renders (htmx swaps or partial replacement).
  document.body.addEventListener("htmx:after:swap", applyStored);
  new MutationObserver(() => applyStored()).observe(document.body, { childList: true, subtree: true });
})();

// --- Cuesheet: change-detection poller (WS-disconnected fallback) ---
// Server is source of truth (selection + order persisted in DB). While the
// WebSocket is up, signals drive this; the timer here is the fallback that
// keeps every open control tab in sync within ~1s when the socket is down.
(function () {
  if (!document.getElementById("cuesheet")) return;
  const POLL_MS = 800;
  let lastSeen = 0;
  // Seed lastSeen by fetching current version once; avoids an immediate
  // redundant full render on load.
  fetch("/api/cuesheet/status?version=0", {headers: {"Accept": "application/json"}})
    .then((r) => r.json()).then((b) => { lastSeen = b.version; }).catch(() => {});
  async function refresh() {
    try {
      const status = await fetch("/api/cuesheet/status?version=" + lastSeen, {headers: {"Accept": "application/json"}});
      const body = await status.json();
      // Always advance lastSeen to server version to avoid tight loop
      lastSeen = body.version;
      if (!body.changed) return;
      const res = await fetch("/api/cuesheet", {headers: {"Accept": "text/html"}});
      const html = await res.text();
      const current = document.getElementById("cuesheet");
      if (!current) return;
      const wrapper = document.createElement("div");
      wrapper.innerHTML = html.trim();
      const replacement = wrapper.firstElementChild;
      if (!replacement) return;
      current.replaceWith(replacement);
      if (window.htmx) htmx.process(replacement);
    } catch (e) {
      // transient
    }
  }
  // HTTP polling is the WS-disconnected fallback ONLY (see the Now Playing
  // poller); while connected, every server signal pulls once, version-guarded.
  let fallback = null;
  function setFallback(on) {
    if (on && !fallback) fallback = setInterval(refresh, POLL_MS);
    if (!on && fallback) { clearInterval(fallback); fallback = null; }
  }
  setFallback(true); // poll until the socket proves itself
  document.addEventListener("cutepi-ws", (e) => setFallback(!e.detail.connected));
  // WebSocket "sync" wakes this poller immediately (single writer: the
  // poller is the only thing that swaps the cuesheet, so a WS-triggered
  // swap and a poll tick can no longer race each other's re-render).
  document.addEventListener("cutepi-sync", refresh);
})();

// Prefer server push: while the socket is up the server signals every
// change (including one sync per displayed second while playing) and the
// version-guarded pollers pull exactly then. The pollers' HTTP timers run
// only as the fallback while the socket is disconnected.
(function () {
  if (!window.WebSocket || !document.getElementById("cuesheet")) return;
  let socket;
  let retry;
  function connect() {
    const scheme = location.protocol === "https:" ? "wss" : "ws";
    socket = new WebSocket(scheme + "://" + location.host + "/api/ws");
    socket.onmessage = (event) => {
      let type;
      try {
        type = JSON.parse(event.data).type;
      } catch (e) {
        return;
      }
      if (type === "media") {
        // Skip while a manual swap owns the pool (the YouTube downloader and
        // rename flows fetch it themselves); otherwise refetch like before.
        if (window.__poolBusy) return;
        if (document.getElementById("mediapool")) {
          htmx.ajax("GET", "/mediapool", {target: "#mediapool", swap: "outerHTML"});
        }
        return;
      }
      if (type !== "sync") return;
      // Wake the authoritative pollers; they are the only writers for the
      // cuesheet and now-playing widget, so no parallel htmx swap here (it
      // used to race the poller's replaceWith: the htmx response could land
      // in a node the poller had just detached, losing that update until
      // the next tick - and the double re-render flickered).
      document.dispatchEvent(new Event("cutepi-sync"));
    };
    socket.onopen = () => {
      document.dispatchEvent(new CustomEvent("cutepi-ws", {detail: {connected: true}}));
    };
    socket.onclose = () => {
      // Fallback pollers resume while the socket is down; reconnect in 2s.
      document.dispatchEvent(new CustomEvent("cutepi-ws", {detail: {connected: false}}));
      clearTimeout(retry);
      retry = setTimeout(connect, 2000);
    };
    socket.onerror = () => socket.close();
  }
  connect();
})();

// Topbar wall clock: locale HH:MM:SS, one client-side tick, no server
// round trip. Tabular numerals so digit changes never reflow the bar.
(function () {
  const el = document.getElementById("topbar-clock");
  if (!el) return;
  const tick = () => {
    const now = new Date();
    const label = now.toLocaleTimeString(undefined, {hour12: false});
    el.textContent = label;
    el.setAttribute("aria-label", "Current time " + label);
  };
  tick();
  setInterval(tick, 1000);
})();

// Header live-status dot: mirrors the WebSocket connection state. Hidden on
// pages without the cuesheet (the WS client only runs there, so the dot
// would otherwise sit red forever on e.g. the upload page). A disconnect
// only turns the dot red after 1s: sub-second blips (server hiccup, tab
// suspension) reconnect instantly and would otherwise flash red.
(function () {
  const el = document.getElementById("ws-status");
  if (!el) return;
  if (!document.getElementById("cuesheet")) { el.hidden = true; return; }
  el.hidden = true; // stay dark until the first connection event
  let offTimer;
  const setOn = () => {
    clearTimeout(offTimer);
    el.hidden = false;
    el.classList.add("ws-status-on");
    el.setAttribute("aria-label", "Live updates connected");
  };
  const setOff = () => {
    el.hidden = false;
    el.classList.remove("ws-status-on");
    el.setAttribute("aria-label", "Live updates offline");
  };
  document.addEventListener("cutepi-ws", (e) => {
    const on = !!(e.detail && e.detail.connected);
    if (on) { setOn(); return; }
    clearTimeout(offTimer);
    offTimer = setTimeout(setOff, 1000);
  });
})();

// --- YouTube downloader: live progress + post-download rename ---
// POST /youtube streams newline-delimited JSON events ({"stage":...,
// "pct":...,"speed":...,"eta":...} then {"done":true,"filename":...} or
// {"error":"..."}). A plain streaming fetch renders yt-dlp's own progress
// live; on done the pool refreshes (its WS "media" broadcast is suppressed
// while a manual swap owns it) and a rename field appears.
(function () {
  var form = document.getElementById("youtube-dl");
  if (!form) return;
  var btn = document.getElementById("download");
  var status = document.getElementById("ytdl-status");
  var stageEl = document.getElementById("ytdl-stage");
  var bar = document.getElementById("ytdl-progress");
  var pctEl = document.getElementById("ytdl-percent");
  var spdEl = document.getElementById("ytdl-speed");
  var etaEl = document.getElementById("ytdl-eta");
  var renameEl = document.getElementById("ytdl-rename");
  var renameInput = document.getElementById("ytdl-rename-input");
  var renameBtn = document.getElementById("ytdl-rename-btn");
  var errEl = document.getElementById("ytdl-error");
  var finished = false;

  function showError(msg) {
    finished = true;
    errEl.textContent = msg;
    errEl.classList.remove("d-none");
    if (btn) btn.disabled = false;
  }

  function refreshPool() {
    if (!window.htmx || !document.getElementById("mediapool")) return;
    window.__poolBusy = true;
    htmx.ajax("GET", "/mediapool", {target: "#mediapool", swap: "outerHTML"});
    setTimeout(function () { window.__poolBusy = false; }, 2000);
  }

  function swapPool(html) {
    var el = document.getElementById("mediapool");
    if (!el) return;
    var wrap = document.createElement("div");
    wrap.innerHTML = html.trim();
    var replacement = wrap.firstElementChild;
    if (replacement && replacement.id === "mediapool") {
      window.__poolBusy = true;
      el.replaceWith(replacement);
      if (window.htmx) htmx.process(replacement);
      setTimeout(function () { window.__poolBusy = false; }, 2000);
    }
  }

  function onLine(msg) {
    if (msg.error) { showError(msg.error); return; }
    if (msg.done) {
      finished = true;
      stageEl.textContent = "Downloaded and imported \u2713";
      pctEl.textContent = "100%";
      bar.style.width = "100%";
      if (btn) btn.disabled = false;
      if (renameInput) {
        renameInput.value = msg.filename;
        renameInput.dataset.old = msg.filename;
      }
      if (renameEl) renameEl.classList.remove("d-none");
      refreshPool();
    } else if (msg.stage === "resolving") {
      stageEl.textContent = "Resolving video\u2026";
    } else if (msg.stage === "resolved") {
      stageEl.textContent = "Resolved: " + msg.title;
    } else if (msg.stage === "download") {
      var pct = Math.round(msg.pct * 10) / 10;
      stageEl.textContent = "Downloading\u2026";
      bar.style.width = Math.min(100, pct) + "%";
      pctEl.textContent = pct + "%";
      if (msg.speed) spdEl.textContent = msg.speed;
      if (msg.eta) etaEl.textContent = "ETA " + msg.eta;
    } else if (msg.stage === "processing") {
      stageEl.textContent = "Post-processing\u2026";
    } else if (msg.stage === "importing") {
      stageEl.textContent = "Importing to media pool\u2026";
      bar.style.width = "100%";
    }
  }

  form.addEventListener("submit", function (ev) {
    ev.preventDefault();
    errEl.classList.add("d-none"); errEl.textContent = "";
    if (renameEl) renameEl.classList.add("d-none");
    if (btn) btn.disabled = true;
    status.classList.remove("d-none");
    stageEl.textContent = "Preparing\u2026";
    bar.style.width = "0%";
    pctEl.textContent = "0%";
    spdEl.textContent = "";
    etaEl.textContent = "";
    finished = false;

    fetch("/youtube", {method: "POST", body: new URLSearchParams(new FormData(form))})
      .then(function (res) {
        if (!res.ok) return res.text().then(function (t) { throw new Error(stripHtml(t) || "download failed"); });
        var reader = res.body.getReader();
        var dec = new TextDecoder();
        var buf = "";
        function pump() {
          return reader.read().then(function (r) {
            if (r.done) {
              if (!finished) showError("Download ended unexpectedly.");
              return;
            }
            buf += dec.decode(r.value, {stream: true});
            var nl;
            while ((nl = buf.indexOf("\n")) !== -1) {
              var line = buf.slice(0, nl).trim();
              buf = buf.slice(nl + 1);
              if (!line) continue;
              try { onLine(JSON.parse(line)); } catch (e) {}
            }
            return pump();
          });
        }
        return pump();
      })
      .catch(function (err) { showError(err.message); });
  });

  // Post-download rename (the button is type=button; Enter still triggers it).
  if (renameBtn && renameInput) {
    renameBtn.addEventListener("click", function () {
      var name = renameInput.value.trim();
      var old = renameInput.dataset.old || "";
      if (!name || !old) return;
      renameBtn.disabled = true;
      fetch("/youtube/rename", {
        method: "POST",
        headers: {"Content-Type": "application/x-www-form-urlencoded"},
        body: new URLSearchParams({old: old, name: name}),
      })
        .then(function (res) {
          if (!res.ok) return res.text().then(function (t) { throw new Error(stripHtml(t) || "rename failed"); });
          return res.text();
        })
        .then(function (html) {
          swapPool(html);
          stageEl.textContent = "Renamed \u2713";
          renameInput.dataset.old = "";
          renameBtn.disabled = false;
        })
        .catch(function (err) { showError(err.message); renameBtn.disabled = false; });
    });
    renameInput.addEventListener("keydown", function (e) {
      if (e.key === "Enter") { e.preventDefault(); renameBtn.click(); }
    });
  }

  // Back to a clean state next time the modal opens.
  var modal = document.getElementById("ytdlModal");
  if (modal) modal.addEventListener("hidden.bs.modal", function () {
    if (status) status.classList.add("d-none");
    if (renameEl) renameEl.classList.add("d-none");
    if (errEl) errEl.classList.add("d-none");
    if (renameInput) renameInput.dataset.old = "";
    if (btn) btn.disabled = false;
  });

  function stripHtml(html) {
    var d = document.createElement("div");
    d.innerHTML = (html || "").trim();
    return (d.textContent || "").trim();
  }
})();

// In-cell editor helper: a double-click on a cue/group field opens an inline
// editor, but those rows also re-render the whole sheet on a single click
// (select). The row's "click delay:250ms[!justEdited()]" trigger consults
// this timestamp so the delayed select - which would land AFTER the editor
// swapped in and wipe it - is suppressed when a double-click just happened.
let inlineEditAt = 0;
document.addEventListener("dblclick", (e) => {
  const t = e.target;
  if (t instanceof Element && t.closest(".cue-inline-edit")) inlineEditAt = Date.now();
});
// The second click of a double-click (detail === 2) lands well before the
// first click's 250ms delayed select fires: stamp the guard now so that
// pending select can't wipe the editor the dblclick just opened. (The
// dblclick listener above fires too late for this — the editor doesn't exist
// yet on the second click, and the first click's timer is already armed.)
document.addEventListener("click", (e) => {
  if (!(e.target instanceof Element)) return;
  if (e.detail >= 2 && e.target.closest("#cuesheet .cue-inline-edit")) {
    inlineEditAt = Date.now();
  }
}, true);

// Shift/Ctrl-click on cue AND group-header rows drives multi-selection
// (§12.4): the row's own hx click trigger would single-select, so intercept
// first and route to the selection modifiers.
document.addEventListener("click", (e) => {
  if (!(e.target instanceof Element)) return;
  const groupRow = e.target.closest("#cuesheet tr.cue-group-header[data-group-id]");
  const row = e.target.closest("#cuesheet tr.cue[data-cue-pos]");
  if (!row && !groupRow) return;
  if (e.shiftKey || e.ctrlKey || e.metaKey) {
    e.preventDefault();
    e.stopImmediatePropagation();
    const mode = e.shiftKey ? "extend=1" : "toggle=1";
    const url = groupRow
      ? "/api/group/" + encodeURIComponent(groupRow.dataset.groupId) + "/select?" + mode
      : "/api/cue/" + encodeURIComponent(row.dataset.cuePos) + "?" + mode;
    htmx.ajax("POST", url, {target: "#cuesheet", swap: "outerHTML"});
  }
}, true);
window.justEdited = () => Date.now() - inlineEditAt < 350;
