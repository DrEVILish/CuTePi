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
// Surface every 4xx/5xx as a dismissible toast. htmx requests get the
// server's message as plain text; an HTML body (error.html) keeps it in <pre>.
htmx.on("htmx:response:error", (e) => {
  const ctx = e.detail?.ctx || {};
  // A group-inspector 404 is self-healing (the handler below falls back to
  // the cue inspector): no toast for it.
  if (ctx.response?.status === 404 && isInspectorTarget(ctx.target) &&
      /\/api\/group\/\d+\/inspector/.test(ctx.request?.action || "")) return;
  let detail = "";
  const body = (ctx.text || "").trim();
  if (body && !body.startsWith("<")) detail = body.replace(/\s+/g, " ");
  else try {
    const doc = new DOMParser().parseFromString(body, "text/html");
    detail = (doc.querySelector("pre") || doc.body).textContent.replace(/\s+/g, " ").trim();
  } catch (err) { /* body may be empty or non-HTML; status alone still shows */ }
  if (detail.length > 160) detail = detail.slice(0, 159) + "…";
  const status = ctx.response?.raw?.status ?? "?";
  showToast("Request failed (" + status + ")" + (detail ? ": " + detail + "" : ""));
  // Mirror the failure into the server log viewer so transient 404s/500s are
  // visible from the logs page, not just the browser console.
  try {
    const path = ctx.request?.action || ctx.sourceElement?.getAttribute?.("hx-get") ||
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
htmx.on("htmx:response:error", (e) => {
  const ctx = e.detail?.ctx;
  if (!isInspectorTarget(ctx?.target)) return;
  const path = ctx.request?.action || ctx.sourceElement?.getAttribute?.("hx-get") || "";
  if (/\/api\/group\/\d+\/inspector/.test(path) && window.htmx) {
    htmx.ajax("GET", "/api/cue/inspector?_=" + Date.now(), {target: "#cueinspector-body", swap: "outerHTML"});
  }
});

// Tests toggle button state: pressed while a pattern is on the wall.
// Synced from toggle responses, modal Hide, and once at load (a test may
// already run from another client).
function setTestPressed(on) {
  const btn = document.getElementById("showTestBtn");
  if (!btn) return;
  btn.setAttribute("aria-pressed", on ? "true" : "false");
  btn.classList.toggle("is-active", on);
  // Off: a quiet grey button. On: red, pulsing, "TEST ON" — unmissable.
  for (const b of [btn, document.getElementById("showTestPicker")]) {
    if (!b) continue;
    b.classList.toggle("btn-danger", on);
    b.classList.toggle("btn-secondary", !on);
  }
  btn.classList.toggle("test-live", on);
  const label = document.getElementById("showTestLabel");
  if (label) label.textContent = on ? "TEST ON" : "Tests";
  btn.title = on ? "A test pattern is on the output — click to take it off" : "Put the last test pattern on the output";
}
window.setTestPressed = setTestPressed;
fetch("/api/testpatterns", { headers: { Accept: "application/json" } })
  .then((r) => (r.ok ? r.json() : null))
  .then((d) => { if (d) setTestPressed(!!d.showing); })
  .catch(() => {});
htmx.on("htmx:after:request", (e) => {
  const el = e.detail.ctx?.sourceElement;
  const ok = (e.detail.ctx?.response?.status ?? 500) < 400;
  const ui = () => window.bootstrap;
  if (el && el.id === "youtube-dl") {
    const info = document.getElementById("info");
    if (info) info.textContent = ok ? "Download complete" : "Download failed. Check the server log.";
    if (ok) hideModal("ytdlModal");
    return;
  }
  if (el && el.id === "live-cue-form") {
    const err = document.getElementById("live-error");
    if (ok) {
      el.reset();
      if (err) { err.textContent = ""; err.classList.add("d-none"); }
      hideModal("liveModal");
    } else if (err) {
      err.textContent = (e.detail.ctx?.text || "").trim() || "Could not add the live page.";
      err.classList.remove("d-none");
    }
    return;
  }
  if (el && el.id === "dropform" && ok) hideModal("uploadModal");
  if (el && el.id === "testHideBtn" && ok) { hideModal("testModal"); setTestPressed(false); }
  if (el && el.id === "deleteConfirmBtn" && ok) hideModal("deleteModal");
  if (el && el.id === "showTestBtn" && ok) {
    try {
      setTestPressed(!!JSON.parse(e.detail.ctx.text || "{}").showing);
    } catch (_) {}
  }
  if (el && el.id === "settingsForm") {
    const status = document.getElementById("settingsStatus");
    if (!status) return;
    if (ok) {
      try {
        const body = JSON.parse(e.detail.ctx.text || "{}");
        const sPort = document.getElementById("settingsPort");
        if (sPort) sPort.value = body.port;
        const authState = document.getElementById("settingsAuthState");
        if (authState && typeof body.authEnabled === "boolean") {
          authState.textContent = body.authEnabled
            ? "A password is currently set."
            : "No password set (open access).";
        }
        if (body.remoteStatus && window.renderRemoteStatus) window.renderRemoteStatus(body.remoteStatus);
        status.textContent = body.message || "Saved.";
        status.className = "settings-status text-success";
      } catch (err) {
        status.textContent = "Saved (could not read response).";
        status.className = "settings-status text-success";
      }
    } else {
      status.textContent = "Save failed - check the values on each tab.";
      status.className = "settings-status text-danger";
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

// Swap #id for a rendered partial whose root is #id again and re-process its
// htmx attributes. Returns the new element, or null when either is missing.
function replaceById(id, html) {
  const current = document.getElementById(id);
  if (!current) return null;
  const wrapper = document.createElement("div");
  wrapper.innerHTML = html.trim();
  const replacement = wrapper.firstElementChild;
  if (!replacement || replacement.id !== id) {
    console.error("CuTePi: unexpected partial response for #" + id);
    return null;
  }
  current.replaceWith(replacement);
  if (window.htmx) htmx.process(replacement);
  return replacement;
}

// --- Upload helpers (§5.7) -------------------------------------------------
// Every upload path (modal, pool drop, /upload page, show import) checks the
// batch against the media volume's free space first, and reports live
// progress: bytes sent, then the server-side import/validation phase.
function fmtBytes(n) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (i ? n.toFixed(1) : n) + " " + units[i];
}
// Resolves true to proceed: enough space, space unknown, or the operator
// chose to upload anyway after the warning.
function cutepiCheckSpace(files) {
  let total = 0;
  for (const f of files || []) total += f.size || 0;
  return fetch("/api/disk", { headers: { Accept: "application/json" } })
    .then((r) => (r.ok ? r.json() : null))
    .then((d) => {
      if (!d || !d.known || total <= d.freeBytes) return true;
      return window.confirm("These files total " + fmtBytes(total) + " but only " +
        fmtBytes(d.freeBytes) + " is free on the media disk. Upload anyway?");
    })
    .catch(() => true);
}
// Floating progress card for uploads that have no modal of their own (pool
// drag-and-drop). set(pct, text) updates it; done(text, ok) fades it out.
function cutepiProgress(label) {
  const el = document.createElement("div");
  el.className = "upload-progress-card";
  el.setAttribute("role", "status");
  el.setAttribute("aria-live", "polite");
  el.innerHTML = '<div class="small upload-progress-text"></div><progress max="100" value="0"></progress>';
  const text = el.querySelector(".upload-progress-text");
  const bar = el.querySelector("progress");
  text.textContent = label;
  document.body.appendChild(el);
  return {
    set(pct, t) {
      if (pct === null) bar.removeAttribute("value"); else bar.value = pct;
      if (t) text.textContent = t;
    },
    done(t, ok) {
      bar.value = 100;
      text.textContent = t;
      el.classList.add(ok ? "is-ok" : "is-error");
      setTimeout(() => el.remove(), ok ? 2500 : 6000);
    },
  };
}
// XHR POST with upload progress; onProgress(pct|null, phaseText). Resolves
// {status, text}. pct null = indeterminate (the server is importing).
function cutepiUpload(url, formData, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", url);
    xhr.setRequestHeader("HX-Request", "true");
    xhr.upload.addEventListener("progress", (ev) => {
      if (!ev.lengthComputable) return;
      const pct = Math.round((ev.loaded / ev.total) * 100);
      onProgress(pct, "Uploading… " + pct + "% (" + fmtBytes(ev.loaded) + " of " + fmtBytes(ev.total) + ")");
    });
    xhr.upload.addEventListener("load", () => onProgress(null, "Importing… validating media on the server"));
    xhr.addEventListener("load", () => resolve({ status: xhr.status, text: xhr.responseText || "", result: xhr.getResponseHeader("X-Upload-Result") }));
    xhr.addEventListener("error", () => reject(new Error("network error")));
    xhr.send(formData);
  });
}

// Server refusals are plain text (htmx requests), an error page (reason in
// its <pre>) or, for a name clash, JSON with an "error" field.
function uploadErrorText(status, text) {
  try {
    const j = JSON.parse(text);
    if (j && j.error) return j.error;
  } catch (err) { /* not JSON */ }
  const body = (text || "").trim();
  if (body && !body.startsWith("<")) return body.replace(/\s+/g, " ").slice(0, 300);
  const doc = new DOMParser().parseFromString(body, "text/html");
  return (doc.querySelector("pre") || doc.body).textContent.replace(/\s+/g, " ").trim().slice(0, 300) || ("server returned " + status);
}

// Name clashes are settled before any bytes are sent: ask the server which
// names already exist (or repeat in the batch), then let the operator pick.
// Resolves {go: false} on cancel, else {go: true, onConflict: "" | choice}.
async function cutepiUploadChoice(files) {
  let conflicts = [];
  try {
    const r = await fetch("/upload/check", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ names: Array.from(files, (f) => f.name) }),
    });
    if (r.ok) conflicts = (await r.json()).conflicts || [];
  } catch (err) { /* the upload itself still refuses an unresolved clash */ }
  if (!conflicts.length) return { go: true, onConflict: "" };
  const choice = await askUploadConflict(conflicts);
  return choice ? { go: true, onConflict: choice } : { go: false };
}

function askUploadConflict(names) {
  return new Promise((resolve) => {
    const back = document.createElement("div");
    back.className = "upload-conflict-backdrop";
    const box = document.createElement("div");
    box.className = "upload-conflict card bg-dark border-secondary";
    box.setAttribute("role", "alertdialog");
    box.setAttribute("aria-modal", "true");
    box.setAttribute("aria-labelledby", "upload-conflict-title");
    const title = document.createElement("h2");
    title.id = "upload-conflict-title";
    title.className = "h6";
    title.textContent = names.length === 1
      ? "A file with this name is already in the media pool"
      : names.length + " files with these names are already in the media pool";
    const list = document.createElement("ul");
    list.className = "small";
    names.forEach((n) => { const li = document.createElement("li"); li.textContent = n; list.appendChild(li); });
    const actions = document.createElement("div");
    actions.className = "upload-conflict-actions";
    const done = (v) => { document.removeEventListener("keydown", onKey, true); back.remove(); resolve(v); };
    const onKey = (e) => { if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); done(null); } };
    [["Cancel", null, "btn-ghost"], ["Skip", "skip", "btn-secondary"],
     ["Keep both", "rename", "btn-secondary"], ["Replace", "replace", "btn-danger"]].forEach(([label, v, cls]) => {
      const b = document.createElement("button");
      b.type = "button";
      b.className = "btn " + cls;
      b.textContent = label;
      b.dataset.conflictChoice = v || "cancel";
      b.title = { skip: "Leave the pool files as they are and don't upload these", rename: "Upload these as new files named \"name (2)\"", replace: "Overwrite the pool files with these uploads" }[v] || "Don't upload anything";
      b.addEventListener("click", () => done(v));
      actions.appendChild(b);
    });
    box.append(title, list, actions);
    back.appendChild(box);
    document.body.appendChild(back);
    document.addEventListener("keydown", onKey, true);
    actions.querySelector('[data-conflict-choice="rename"]').focus();
  });
}

// Summary line for a finished upload from the server's X-Upload-Result.
function cutepiUploadSummary(header) {
  try {
    const r = JSON.parse(decodeURIComponent(header || ""));
    const n = (r.imported || []).length, k = (r.skipped || []).length;
    if (!k) return n === 1 ? "Uploaded " + r.imported[0] : "Uploaded " + n + " files";
    if (!n) return "Nothing uploaded: " + (k === 1 ? r.skipped[0] + " is" : k + " files are") + " already in the pool";
    return "Uploaded " + n + ", skipped " + k + " already in the pool";
  } catch (err) {
    return "Upload complete";
  }
}

// One dismissible bootstrap toast per error; the container is created lazily
// so every page that loads ui.js gets feedback with no markup changes.
function showToast(message, kind) {
  let holder = document.getElementById("ctp-toasts");
  if (!holder) {
    holder = document.createElement("div");
    holder.id = "ctp-toasts";
    holder.className = "toast-container position-fixed bottom-0 end-0 p-3";
    holder.style.zIndex = 2000; // above modals (1055)
    document.body.appendChild(holder);
  }
  const el = document.createElement("div");
  el.className = "toast align-items-center border-0 " + (kind === "success" ? "text-bg-success" : "text-bg-danger");
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

// Theme registry: header.html's boot script already rendered the id ->
// {name, href} map into the page, so start from that (it is what picked the
// stylesheet before first paint) and refresh from /api/themes — same source,
// routes.Themes — so a theme file added while the page is open validates too.
const DEFAULT_THEME_ID = document.documentElement.dataset.themeId || "ftl:xbmc";
const appThemeMap = {};
try {
  const boot = document.getElementById("cutepi-theme-css");
  if (boot && boot.href) {
    appThemeMap[DEFAULT_THEME_ID] = {
      name: document.documentElement.dataset.theme,
      href: boot.getAttribute("href"),
      version: "",
    };
  }
} catch (e) {}
fetch("/api/themes", { headers: { Accept: "application/json" } })
  .then((resp) => (resp.ok ? resp.json() : []))
  .then((list) => {
    if (Array.isArray(list)) {
      list.forEach((t) => {
        if (t && t.id && t.name && t.href) {
          appThemeMap[t.id] = { name: t.name, href: t.href, scheme: t.scheme, version: t.version || "", variants: t.variants || [], tint: t.tint || null };
        }
      });
    }
  })
  .catch(() => {});
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
  // Test output is an Edit-mode tool (the server 403s it in Show mode,
  // which locks the sheet): enabled while editing, disabled on Show.
  const tests = document.getElementById("showTestBtn");
  if (tests) tests.disabled = show;
  // The Cue Inspector is unavailable in Show mode (see layout.css): keep
  // the footer toggle button disabled too so it cannot be reopened.
  const inspExpand = document.getElementById("cueinspector-toggle");
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
// No polling: the scheduler pushes a sync when a schedule enters or leaves
// its final minute, and every sync re-reads the state here.
function refreshScheduleFlash() {
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
refreshScheduleFlash();
document.addEventListener("cutepi-sync", refreshScheduleFlash);

// Themes all come from ftl-themes and are identified as "ftl:<slug>". The
// id -> {name, href, scheme} map is rendered into the page by header.html and
// refreshed from /api/themes.
// Mark every .icon's <use> with the theme's sprite: each theme ships
// dist/icons/<slug>.svg (the generic set plus that theme's redraws).
function applyIconSprite(id) {
  const srcs = id.indexOf("ftl:") === 0
    ? "/ftl/themes/icons/" + id.slice(4) + ".svg"
    : "/ftl/assets/icons/icons.svg";
  document.querySelectorAll('.icon use[href]').forEach((u) => {
    const h = u.getAttribute("href") || "";
    const hash = h.indexOf("#");
    if (hash < 0) return;
    u.setAttribute("href", srcs + h.slice(hash));
  });
}

function applyAppTheme(id) {
  // Older saved values ("lcars", "app:blue-future") map to the ftl-themes
  // theme of the same name, or the default when there is none.
  id = "ftl:" + String(id || "").split(":").pop();
  if (!appThemeMap[id]) id = DEFAULT_THEME_ID;
  applyIconSprite(id);
  const link = document.getElementById("cutepi-theme-css");
  if (link) {
    const version = appThemeMap[id].version || document.documentElement.dataset.assetStamp || "";
    link.href = appThemeMap[id].href + "?v=" + encodeURIComponent(version);
  }
  // The outgoing theme's tint token must not linger on <html>.
  const prev = appThemeMap[document.documentElement.dataset.themeId];
  if (prev && prev.tint) document.documentElement.style.removeProperty(prev.tint.token);
  document.documentElement.dataset.theme = appThemeMap[id].name;
  document.documentElement.dataset.bsTheme = appThemeMap[id].scheme || "dark";
  document.documentElement.dataset.themeId = id;
  try {
    localStorage.setItem("cutepi.theme", id);
  } catch (e) {}
  applyThemeVariant(themeStore("variant"));
  applyThemeTint(themeStore("tint"));
  themeControls();
}

// Sub-themes and tint (ftl-themes contract "Palette variants" and "Theme
// tint"), saved per theme slug in this browser like the theme itself.
function currentTheme() {
  return appThemeMap[document.documentElement.dataset.themeId] || null;
}
function themeStore(kind, value) {
  const key = "cutepi.theme." + kind + "." + document.documentElement.dataset.theme;
  try {
    if (value === undefined) return localStorage.getItem(key) || "";
    if (value) localStorage.setItem(key, value);
    else localStorage.removeItem(key);
  } catch (e) {}
  return "";
}
function applyThemeVariant(v) {
  const t = currentTheme();
  const ok = t && v && (t.variants || []).some((x) => x.id === v);
  if (ok) document.documentElement.dataset.variant = v;
  else delete document.documentElement.dataset.variant;
}
function applyThemeTint(c) {
  const t = currentTheme();
  if (!t || !t.tint) return;
  if (/^#[0-9a-fA-F]{6}$/.test(c || "")) document.documentElement.style.setProperty(t.tint.token, c);
  else document.documentElement.style.removeProperty(t.tint.token);
}
// themeControls fills the Style and colour fields for the current theme and
// hides each one the theme does not have.
function themeControls() {
  const t = currentTheme();
  const vf = document.getElementById("settingsThemeVariantField");
  const vs = document.getElementById("settingsThemeVariant");
  if (vf && vs) {
    const vars = (t && t.variants) || [];
    vs.replaceChildren();
    const std = document.createElement("option");
    std.value = "";
    std.textContent = "Standard";
    vs.append(std);
    vars.forEach((x) => {
      const o = document.createElement("option");
      o.value = x.id;
      o.textContent = x.label;
      vs.append(o);
    });
    vs.value = document.documentElement.dataset.variant || "";
    vf.hidden = vars.length === 0;
  }
  const tf = document.getElementById("settingsThemeTintField");
  const ti = document.getElementById("settingsThemeTint");
  if (tf && ti) {
    const tint = t && t.tint;
    tf.hidden = !tint;
    if (tint) {
      const lbl = document.getElementById("settingsThemeTintLabel");
      if (lbl) lbl.textContent = tint.label || "Colour";
      ti.value = themeStore("tint") || tint.default;
    }
  }
}
window.cutepiThemeControls = themeControls;

document.addEventListener("change", (e) => {
  if (e.target.id === "settingsTheme") {
    applyAppTheme(e.target.value);
  } else if (e.target.id === "settingsThemeVariant") {
    themeStore("variant", e.target.value);
    applyThemeVariant(e.target.value);
    // A variant may be a tint preset: choosing one clears the custom colour
    // so the preset shows (contract "Theme tint", rule 3).
    themeStore("tint", "");
    applyThemeTint("");
    themeControls();
  } else if (e.target.id === "settingsThemeTint") {
    themeStore("tint", e.target.value);
    applyThemeTint(e.target.value);
  }
});
// Live preview while the colour is dragged; saved on change (above).
document.addEventListener("input", (e) => {
  if (e.target.id === "settingsThemeTint") applyThemeTint(e.target.value);
});
document.addEventListener("click", (e) => {
  if (!(e.target instanceof Element) || !e.target.closest("#settingsThemeTintReset")) return;
  themeStore("tint", "");
  applyThemeTint("");
  themeControls();
});

// Boot-time sprite + every swapped-in partial (the server renders the
// generic sprite; under an ftl: theme the override shapes re-point after swap).
applyIconSprite(document.documentElement.dataset.themeId || DEFAULT_THEME_ID);
document.addEventListener("htmx:after:swap", () => {
  applyIconSprite(document.documentElement.dataset.themeId || DEFAULT_THEME_ID);
});

// Column resizing was removed; clear the old persisted widths.
try { localStorage.removeItem("cutepi.cuesheet.colWidths"); } catch (e) {}

let lastEscPress = 0;
window.addEventListener("keydown", (e) => {
  const active = document.activeElement;
  const tag = active && active.nodeName ? active.nodeName.toLowerCase() : "";
  const itype = tag === "input" ? String(active.type || "").toLowerCase() : "";
  // Arrow keys always drive row selection except where the user is actively
  // editing text or driving a control that owns its arrows (text inputs,
  // selects, textareas, buttons). Radios (the colour swatches) and
  // checkboxes give up their native arrow behaviour so arrows always move
  // the cue row selection.
  const plain = !active || (tag !== "input" && tag !== "textarea" && tag !== "select" && tag !== "button");
  const arrowTarget = plain || (tag === "input" && (itype === "radio" || itype === "checkbox"));
  const arrows = ["ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight"];
  // Space (GO) and Escape (fade out / panic) work from anywhere except while
  // typing into a text field: a focused button, checkbox, select, slider or
  // open dialog must not swallow them. Space never re-presses the focused
  // control. Held keys do not repeat GO or panic.
  const typing = active && (active.isContentEditable || tag === "textarea" ||
    (tag === "input" && ["", "text", "search", "email", "url", "tel", "password", "number",
      "date", "time", "datetime-local", "month", "week"].indexOf(itype) > -1));
  if (!typing && e.code === "Space") {
    e.preventDefault();
  }
  if (plain && arrows.indexOf(e.code) > -1) {
    e.preventDefault();
  } else if (arrowTarget && arrows.indexOf(e.code) > -1) {
    e.preventDefault(); // stop native radio/checkbox arrow navigation
  }
  // Space (not Enter) is GO: Enter is reserved for menu/inline-edit commit
  // and must never fire the selected cue.
  if (!typing && e.code === "Space" && !e.repeat) {
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
  // Escape = fade out and stop; a second Escape within a second is a hard
  // PANIC (no fade, holding image if configured) — but only when no
  // context menu holds the gesture: Escape closes an open menu instead, and
  // closing a menu must not also kill playback.
  const openMenus = e.code === "Escape" ? document.querySelectorAll(".cue-context-menu:not([hidden])") : [];
  if (openMenus.length) {
    openMenus.forEach((m) => { m.hidden = true; });
  } else if (!typing && e.code === "Escape" && !e.repeat) {
    const now = Date.now();
    if (now - lastEscPress < 1000) {
      lastEscPress = 0;
      htmx.trigger("#panichard", "panichard");
    } else {
      lastEscPress = now;
      htmx.trigger("#esc", "esc");
    }
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
      .then((html) => replaceById("cuesheet", html))
      .catch((err) => console.error("CuTePi: delete failed", err));
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
// styled card with live connection facts. While it is open the facts keep
// updating: uptime ticks locally from the server's base, live/offline
// follows this page's socket, and the client count re-reads on every
// server push or connection change (no polling).
(function () {
  let pop = null, pin = null, hideTimer = null, tick = null;
  let wsLive = false;
  let base = null; // {clients, uptimeS, at}
  function fmtUptime(sec) {
    sec = Math.max(0, Math.floor(sec));
    if (sec >= 3600) return Math.floor(sec / 3600) + "h " + Math.floor((sec % 3600) / 60) + "m";
    return Math.floor(sec / 60) + "m " + (sec % 60) + "s";
  }
  function render() {
    if (!pop || !base) return;
    const up = base.uptimeS + (Date.now() - base.at) / 1000;
    const clients = wsLive ? base.clients : "—";
    pop.innerHTML =
      "<div><strong>" + (wsLive ? "Live" : "Offline") + "</strong> — " + clients +
      " client" + (base.clients === 1 ? "" : "s") + " connected</div>" +
      "<div>Server uptime: " + (wsLive ? fmtUptime(up) : "—") + "</div>" +
      '<div class="text-muted small">' + (wsLive ? "Push updates over WebSocket" : "Socket down — reconnecting") + "</div>";
    const r = pin.getBoundingClientRect();
    const pr = pop.getBoundingClientRect();
    pop.style.left = Math.max(6, Math.min(r.left, window.innerWidth - pr.width - 6)) + "px";
    pop.style.top = (r.bottom + 6) + "px";
  }
  async function refetch() {
    if (!pop) return;
    try {
      const info = await (await fetch("/api/serverinfo")).json();
      base = {clients: info.clients, uptimeS: info.uptimeS, at: Date.now()};
    } catch (err) { /* best-effort: keep the last facts */ }
    render();
  }
  function show(el) {
    pin = el;
    if (!pop) {
      pop = document.createElement("div");
      pop.className = "hover-popout";
      document.body.appendChild(pop);
      tick = setInterval(render, 1000);
    }
    refetch();
  }
  function hide() {
    clearInterval(tick);
    tick = null;
    if (pop) { pop.remove(); pop = null; }
  }
  document.addEventListener("cutepi-ws", (e) => { wsLive = !!e.detail.connected; refetch(); });
  document.addEventListener("cutepi-sync", refetch);
  document.addEventListener("mouseover", (e) => {
    if (!(e.target instanceof Element)) return;
    const el = e.target.closest("#ws-status");
    if (el) { clearTimeout(hideTimer); if (!pop) show(el); }
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
    const t = document.getElementById("mediapool-toggle");
    if (t) {
      t.setAttribute("aria-pressed", state ? "false" : "true");
      t.classList.toggle("active", !state);
    }
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
    if (e.target.closest("#mediapool-toggle")) {
      setCollapsed(!document.body.classList.contains("mediapool-collapsed"));
    }
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

// --- Active Cues pane (§6.1.2): the media pool's mirror on the right ---
// Collapse/expand from the footer button and drag-to-resize, both kept per
// browser. The list re-renders on every WebSocket sync (one a second while
// anything plays) and on reconnect; the footer button shows the count, so
// it is useful collapsed too.
(function () {
  const pane = document.getElementById("activecues-pane");
  const resizer = document.getElementById("activecues-resizer");
  if (!pane || !resizer) return;

  const STORAGE_WIDTH = "cutepi.activecues.width";
  const STORAGE_COLLAPSED = "cutepi.activecues.collapsed";
  const DEFAULT_WIDTH = 280;

  function getWidth() {
    try {
      const w = parseFloat(localStorage.getItem(STORAGE_WIDTH));
      return isNaN(w) || w < 160 ? DEFAULT_WIDTH : w;
    } catch (e) {
      return DEFAULT_WIDTH;
    }
  }
  function setWidth(w) {
    document.documentElement.style.setProperty("--activecues-width", w + "px");
  }
  function setCollapsed(state) {
    document.body.classList.toggle("activecues-collapsed", state);
    const t = document.getElementById("activecues-toggle");
    if (t) {
      t.setAttribute("aria-pressed", state ? "false" : "true");
      t.classList.toggle("active", !state);
    }
    try {
      localStorage.setItem(STORAGE_COLLAPSED, state ? "1" : "0");
    } catch (e) {}
  }
  setWidth(getWidth());
  try {
    setCollapsed(localStorage.getItem(STORAGE_COLLAPSED) === "1");
  } catch (e) {
    setCollapsed(false);
  }
  document.addEventListener("click", (e) => {
    if (e.target.closest("#activecues-toggle")) {
      setCollapsed(!document.body.classList.contains("activecues-collapsed"));
    }
  });

  // The splitter sits left of the pane: dragging left widens it.
  resizer.addEventListener("mousedown", (e) => {
    e.preventDefault();
    const startX = e.clientX;
    const startWidth = pane.getBoundingClientRect().width;
    const minW = 160;
    const maxW = Math.max(minW, window.innerWidth * 0.5);
    const onMove = (ev) => {
      setWidth(Math.min(maxW, Math.max(minW, startWidth - (ev.clientX - startX))));
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

  function showCount(el) {
    const badge = document.getElementById("activecues-count");
    if (!badge || !el) return;
    const n = parseInt(el.dataset.count || "0", 10);
    badge.textContent = n > 0 ? String(n) : "";
  }
  showCount(document.getElementById("activecues"));
  // Its own Stop/Fade buttons swap the list through htmx.
  document.addEventListener("htmx:after:swap", (e) => {
    if (e.detail?.ctx?.target?.id !== "activecues") return;
    showCount(document.getElementById("activecues"));
    document.dispatchEvent(new Event("cutepi-activecues"));
  });

  // A press on Stop / Fade out must not straddle a re-render (the browser
  // drops a click whose press and release land on different elements).
  let pressing = false, missed = false;
  document.addEventListener("pointerdown", (e) => {
    if (e.target instanceof Element && e.target.closest("#activecues")) pressing = true;
  }, true);
  const release = () => {
    if (!pressing) return;
    pressing = false;
    if (missed) { missed = false; setTimeout(refresh, 0); }
  };
  document.addEventListener("pointerup", release, true);
  document.addEventListener("pointercancel", release, true);

  let busy = false, again = false;
  async function refresh() {
    if (pressing) { missed = true; return; }
    if (busy) { again = true; return; }
    busy = true;
    try {
      const res = await fetch("/api/activecues", { headers: { "Accept": "text/html" } });
      if (!res.ok) return;
      const html = (await res.text()).trim();
      const el = document.getElementById("activecues");
      if (!el || pressing) { missed = pressing; return; }
      const wrapper = document.createElement("div");
      wrapper.innerHTML = html;
      const next = wrapper.querySelector("#activecues");
      if (!next) return;
      showCount(next);
      if (next.outerHTML !== el.outerHTML.replace(/ data-htmx-powered="[^"]*"/g, "")) {
        el.replaceWith(next);
        if (window.htmx) htmx.process(next);
      }
      // New positions from the server (the trim timeline's playhead).
      document.dispatchEvent(new Event("cutepi-activecues"));
    } catch (e) {
      // Transient; the next sync retries.
    } finally {
      busy = false;
      if (again) { again = false; refresh(); }
    }
  }
  document.addEventListener("cutepi-sync", refresh);
  document.addEventListener("cutepi-ws", (e) => { if (e.detail.connected) refresh(); });
})();

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
        <svg class="icon -palette"><use href="/ftl/assets/icons/icons.svg#icon-palette"/></svg> Colour:
        <select title="Cue colour">${patternOptions()}</select>
      </div>
      <div class="cue-context-divider"></div>
      <div class="cue-context-item cue-context-fade">
        <svg class="icon -volume-off"><use href="/ftl/assets/icons/icons.svg#icon-volume-mute"/></svg> Fade-stop others
        <select title="Scope"><option value="peers">Peers</option><option value="list">List/Cart</option><option value="all">All</option></select>
        <input type="text" placeholder="0:00" title="Fade/stop time (mm:ss)" value="0:00">
      </div>
      <div class="cue-context-item" data-cue-action="autofollow"><svg class="icon -arrow-right-circle"><use href="/ftl/assets/icons/icons.svg#icon-arrow-right"/></svg> <span>Auto-continue: off</span></div>
      <div class="cue-context-divider"></div>
      <div class="cue-context-item" data-cue-action="newgroup"><svg class="icon -folder-plus"><use href="/ftl/assets/icons/icons.svg#icon-plus"/></svg> <span>New group</span></div>
      <div class="cue-context-divider"></div>
      <div class="cue-context-item" data-sheet-order="sort"><svg class="icon -sort"><use href="/ftl/assets/icons/icons.svg#icon-sort"/></svg> Sort by cue number</div>
      <div class="cue-context-item" data-sheet-order="renumber"><svg class="icon -hash"><use href="/ftl/assets/icons/icons.svg#icon-hash"/></svg> Renumber cues</div>
      <div class="cue-context-divider"></div>
      <div class="cue-context-item cue-context-danger" data-cue-action="delete"><svg class="icon -trash3"><use href="/ftl/assets/icons/icons.svg#icon-trash"/></svg> Delete cue</div>`;
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
    }).then((html) => replaceById("cuesheet", html));
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
              replaceById("cuesheet", html);
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
              replaceById("cuesheet", html);
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
            replaceById("cuesheet", html);
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
      <div class="cue-context-item" data-group-action="inspector"><svg class="icon -sliders"><use href="/ftl/assets/icons/icons.svg#icon-settings"/></svg> Inspector</div>
      <div class="cue-context-item" data-group-action="collapse"><svg class="icon -chevron-down"><use href="/ftl/assets/icons/icons.svg#icon-chevron-down"/></svg> <span>Collapse</span></div>
      <div class="cue-context-item cue-context-color">
        <svg class="icon -palette"><use href="/ftl/assets/icons/icons.svg#icon-palette"/></svg> Colour:
        <select title="Group colour">${patternOptions()}</select>
      </div>
      <div class="cue-context-divider"></div>
      <div class="cue-context-item" data-group-action="newgroup"><svg class="icon -folder-plus"><use href="/ftl/assets/icons/icons.svg#icon-plus"/></svg> New group</div>
      <div class="cue-context-divider"></div>
      <div class="cue-context-item" data-sheet-order="sort"><svg class="icon -sort"><use href="/ftl/assets/icons/icons.svg#icon-sort"/></svg> Sort by cue number</div>
      <div class="cue-context-item" data-sheet-order="renumber"><svg class="icon -hash"><use href="/ftl/assets/icons/icons.svg#icon-hash"/></svg> Renumber cues</div>
      <div class="cue-context-divider"></div>
      <div class="cue-context-item cue-context-danger" data-group-action="delete"><svg class="icon -trash3"><use href="/ftl/assets/icons/icons.svg#icon-trash"/></svg> Delete group</div>`;
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
      .then((html) => replaceById("cuesheet", html))
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
    if (action === "inspector") {
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
      <div class="cue-context-item" data-sheet-action="newgroup"><svg class="icon -folder-plus"><use href="/ftl/assets/icons/icons.svg#icon-plus"/></svg> New group</div>
      <div class="cue-context-divider"></div>
      <div class="cue-context-item" data-sheet-order="sort"><svg class="icon -sort"><use href="/ftl/assets/icons/icons.svg#icon-sort"/></svg> Sort by cue number</div>
      <div class="cue-context-item" data-sheet-order="renumber"><svg class="icon -hash"><use href="/ftl/assets/icons/icons.svg#icon-hash"/></svg> Renumber cues</div>`;
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
// API endpoints the old Bootstrap dropdown used. Items per §5.3: Add (as
// cue), Refresh thumbnail, Analyse, holding image, test pattern, Delete —
// pool media reaches the output through cues, never directly.
(function () {
  let menuEl = null;

  function ensureMenu() {
    if (menuEl) return menuEl;
    menuEl = document.createElement("div");
    menuEl.id = "media-context-menu";
    menuEl.className = "cue-context-menu";
    menuEl.hidden = true;
    menuEl.innerHTML = `
      <button type="button" class="cue-context-item" data-media-action="add"><svg class="icon -plus-circle"><use href="/ftl/assets/icons/icons.svg#icon-plus"/></svg> Add</button>
      <div class="cue-context-divider"></div>
      <button type="button" class="cue-context-item" data-media-action="refresh"><svg class="icon -camera"><use href="/ftl/assets/icons/icons.svg#icon-camera"/></svg> Refresh thumbnail</button>
      <button type="button" class="cue-context-item" data-media-action="analyse"><svg class="icon -music-note-beamed"><use href="/ftl/assets/icons/icons.svg#icon-music-note"/></svg> Analyse</button>
      <button type="button" class="cue-context-item" data-media-action="panichold"><svg class="icon -life-preserver"><use href="/ftl/assets/icons/icons.svg#icon-help-circle"/></svg> Set as holding image</button>
      <button type="button" class="cue-context-item" data-media-action="testpattern"><svg class="icon -tv"><use href="/ftl/assets/icons/icons.svg#icon-video"/></svg> <span>Add to test patterns</span></button>
      <div class="cue-context-divider"></div>
      <button type="button" class="cue-context-item cue-context-danger" data-media-action="delete"><svg class="icon -trash3"><use href="/ftl/assets/icons/icons.svg#icon-trash"/></svg> Delete</button>`;
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

  // Reflect the test-pattern pin state on the menu item's label (both the
  // right-click and the "..." entry points must do this, or a pinned item
  // can never be unpinned from the button menu).
  function refreshPinLabel(m) {
    const name = m.dataset.mediaFilename;
    fetch("/api/testpatterns").then((r) => r.json()).then((d) => {
      if (m.dataset.mediaFilename !== name) return;
      const pinned = (d.custom || []).indexOf(name) > -1;
      const lbl = m.querySelector('[data-media-action="testpattern"] span');
      if (lbl) lbl.textContent = pinned ? "Remove from test patterns" : "Add to test patterns";
    }).catch(() => {});
  }

  // Right-click on a tile.
  document.addEventListener("contextmenu", (e) => {
    const tile = e.target.closest("figure.media-tile");
    if (!tile) return;
    e.preventDefault();
    const m = ensureMenu();
    m.dataset.mediaFilename = tile.dataset.mediaName || "";
    refreshPinLabel(m);
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
    refreshPinLabel(m);
    const rect = btn.getBoundingClientRect();
    positionMenuAt(rect.left, rect.bottom + 4);
    m.querySelector("button").focus();
  });

  // Click outside or blur hides.
  document.addEventListener("click", (e) => {
    if (menuEl && !menuEl.contains(e.target) && !e.target.closest("[data-media-menu]")) hideMenu();
  });
  window.addEventListener("blur", hideMenu);

  // Add a media item to the cuesheet (shared by the context menu and the
  // tile's double-click / Enter gesture).
  function addMediaToCuesheet(filename) {
    if (!needEditMode()) return Promise.reject(new Error("show mode"));
    return fetch("/api/cue/add/" + encodeURIComponent(filename), {method: "POST"})
      .then((res) => {
        if (!res.ok) throw new Error("server returned " + res.status);
        return res.text();
      })
      .then((html) => replaceById("cuesheet", html))
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
    if (action === "add") {
      addMediaToCuesheet(filename);
    } else if (action === "refresh") {
      fetch("/api/media/" + encodeURIComponent(filename) + "/refreshThumbnail", {method: "POST"})
        .catch((err) => console.error("CuTePi: refresh thumbnail failed", err));
    } else if (action === "panichold") {
      const form = new URLSearchParams({filename});
      fetch("/api/setting/panichold", {method: "POST", body: form})
        .then((res) => {
          if (!res.ok) throw new Error("server returned " + res.status);
          showToast("Holding image set — PANIC now cuts to " + filename, "success");
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
          showToast(pinned ? "Removed from test patterns" : "Added to test patterns", "success");
        })
        .catch((err) => {
          console.error("CuTePi: test pattern update failed", err);
          showToast("Test pattern update failed: " + err.message);
        });
    } else if (action === "analyse") {
      fetch("/api/media/" + encodeURIComponent(filename) + "/analyse", {method: "POST"})
        .catch((err) => console.error("CuTePi: analyse failed", err));
    } else if (action === "delete") {
      // §5.3: Delete asks first — it removes the file and every cue using it.
      const nameEl = document.getElementById("deleteFilename");
      const confirmBtn = document.getElementById("deleteConfirmBtn");
      if (!nameEl || !confirmBtn) return;
      nameEl.textContent = filename;
      confirmBtn.setAttribute("hx-delete", "/api/media/" + encodeURIComponent(filename));
      if (window.htmx) htmx.process(confirmBtn);
      showModal("deleteModal");
    }
  });
})();

// --- Now Playing: scrubber, dead-reckoned clock, remaining-time pulse ---
// Lives here rather than in a <script> inside mediainfo.html: the widget is
// re-rendered by DOM swap, and scripts inserted that way never execute, so
// the old inline script only ever bound the FIRST render (later widgets had
// a dead scrubber while the original 500ms timer kept writing to detached
// nodes forever). initNowPlaying runs on load and after every swap, and a
// single module-level timer is reset each time.
let nowPlayingTimer = null;
let clockShown = { v: 0, t: 0 }; // last dead-reckoned position shown
function initNowPlaying() {
  clearInterval(nowPlayingTimer);
  nowPlayingTimer = null;
  const root = document.getElementById("nowplaying-scrubber");
  if (!root) return;
  const remEl = document.getElementById("now-remaining");
  const timeEl = document.getElementById("now-time");
  const duration = parseFloat(root.max) || 0;

  // Dead reckoning: advance the displayed clock from the last server
  // position while playing, so the timer tracks real time instead of
  // lagging a server round trip behind. The next server render resets the
  // base. Skipped while scrubbing, while paused, and when the duration is
  // unknown (held stills stay frozen, correctly).
  let basePos = parseFloat(root.value) || 0;
  const baseMs = Date.now();
  const playing = root.getAttribute("data-playing") === "1";
  // A fresh server sample a hair behind what is already shown (request
  // latency) must not step the clock backwards: keep the shown value.
  if (playing && clockShown.v > basePos && clockShown.v - basePos < 0.5 && baseMs - clockShown.t < 1500) {
    basePos = clockShown.v;
  }
  const fmtClock = (sec) => {
    sec = Math.max(0, sec);
    const m = Math.floor(sec / 60), s = Math.floor(sec % 60);
    return (m < 10 ? "0" : "") + m + ":" + (s < 10 ? "0" : "") + s;
  };
  const tick = () => {
    if (!remEl || !timeEl) return;
    if (playing && duration > 0 && document.activeElement !== root) {
      const live = Math.min(duration, basePos + (Date.now() - baseMs) / 1000);
      root.value = live;
      clockShown = { v: live, t: Date.now() };
      if (timeEl.firstChild) timeEl.firstChild.textContent = fmtClock(live) + " / -";
    }
    const remSec = Math.max(0, duration - (parseFloat(root.value) || 0));
    remEl.textContent = fmtClock(remSec);
    // The playing cue's row bar follows the same clock, so it glides with
    // the NOW bar instead of stepping once per cuesheet render.
    const bar = document.querySelector("#cuesheet tr.cue-playing .cue-progress-bar");
    const fill = bar && bar.querySelector(".cue-progress-fill");
    const rowDur = bar ? parseFloat(bar.dataset.dur) / 1000 : 0;
    if (fill && rowDur > 0) {
      fill.style.width = Math.min(100, (parseFloat(root.value) || 0) / rowDur * 100) + "%";
    }
    remEl.classList.toggle("remaining-fast", remSec <= 10 && remSec > 0);
    remEl.classList.toggle("remaining-slow", remSec > 10 && remSec <= 30);
  };
  tick();
  nowPlayingTimer = setInterval(tick, 100); // smooth bar; the clock text still changes once a second

  if (root.dataset.scrubBound) return;
  root.dataset.scrubBound = "1";
  let scrubTimer = null;
  root.addEventListener("input", () => {
    const pos = parseFloat(root.value) || 0;
    clearTimeout(scrubTimer);
    scrubTimer = setTimeout(() => {
      fetch("/api/seek", {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded", "HX-Request": "true" },
        body: "position=" + pos,
      }).catch(() => {});
    }, 120);
  });
}
initNowPlaying();

// --- Now Playing: change-detection refresh (WebSocket-driven) ---
// Every WebSocket sync pulls GET /api/nowplaying/status, which returns
// {"changed": bool, "version": "<state>.<cuesheet>"}; the widget is
// re-rendered (via GET /api/nowplaying) only when the version differs from
// what this client last saw. Each client tracks its own last-seen version;
// the server keeps no per-client state. No-op on pages without #mediainfo
// (e.g. /upload).
(function () {
  if (!document.getElementById("mediainfo")) return;

  let lastSeen = 0;

  // No polling (§6.7): every server signal (cutepi-sync) pulls once,
  // version-guarded. The server pushes one sync per displayed second while
  // playing (gsp ticker), so the progress clock advances without any
  // timer-driven requests. A (re)connect pulls once to catch up on anything
  // missed while the socket was down.
  document.addEventListener("cutepi-ws", (e) => { if (e.detail.connected) refresh(); });

  // A press (mouse or touch) on GO / Pause must not straddle a re-render: the
  // browser drops a click whose press and release hit different elements.
  // Hold refreshes while a pointer is down in the bar; catch up on release.
  let pressing = false, missed = false;
  document.addEventListener("pointerdown", (e) => {
    if (e.target instanceof Element && e.target.closest("#mediainfo")) pressing = true;
  }, true);
  const release = () => {
    if (!pressing) return;
    pressing = false;
    if (missed) { missed = false; setTimeout(refresh, 0); }
  };
  document.addEventListener("pointerup", release, true);
  document.addEventListener("pointercancel", release, true);

  // Same clip, same controls: only the clock moved, so update the text and
  // the scrubber in place instead of replacing the buttons under the pointer.
  function shape(root) {
    // htmx marks processed nodes (data-htmx-powered) and the GO flash is
    // client-side; neither is a change in what the server rendered.
    const norm = (b) => {
      const c = b.cloneNode(true);
      c.removeAttribute("data-htmx-powered");
      c.classList.remove("go-imminent");
      return c.outerHTML;
    };
    return Array.from(root.querySelectorAll("button, [data-shape]"), norm).join("|") +
      "|" + ((root.querySelector(".now-title") || {}).textContent || "");
  }
  function patchInPlace(el, next) {
    if (shape(el) !== shape(next)) return false;
    for (const id of ["now-time", "now-remaining"]) {
      const a = el.querySelector("#" + id), b = next.querySelector("#" + id);
      if (!a || !b) return false;
      a.innerHTML = b.innerHTML;
      a.title = b.title;
    }
    const a = el.querySelector("#nowplaying-scrubber"), b = next.querySelector("#nowplaying-scrubber");
    if (a && b) {
      for (const k of ["min", "max", "data-playing"]) a.setAttribute(k, b.getAttribute(k));
      a.value = b.value;
      a.setAttribute("value", b.getAttribute("value"));
    }
    return true;
  }

  async function refresh() {
    if (pressing) { missed = true; return; }
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
      const html = await res.text();
      const el = document.getElementById("mediainfo");
      if (!el) return;
      // Parse and swap the node (not el.outerHTML = …): the new widget's
      // hx-post buttons (GO, pause) must go through htmx.process or they are
      // dead after the first refresh, and the scrubber is re-bound below.
      const wrapper = document.createElement("div");
      wrapper.innerHTML = html.trim();
      const replacement = wrapper.querySelector("#mediainfo");
      if (!replacement) return;
      // The GO "imminent" flash is client-applied; carry it across the swap.
      const oldGo = el.querySelector("#go-btn");
      const newGo = replacement.querySelector("#go-btn");
      if (oldGo && newGo) newGo.classList.toggle("go-imminent", oldGo.classList.contains("go-imminent"));
      if (pressing) { missed = true; return; }
      setTestPressed(replacement.dataset.testShowing === "1");
      if (patchInPlace(el, replacement)) { initNowPlaying(); return; }
      el.replaceWith(replacement);
      if (window.htmx) htmx.process(replacement);
      initNowPlaying();
    } catch (e) {
      // Transient network/server error; the next sync retries.
    }
  }
  // WebSocket "sync" wakes this refresher immediately (single writer for the
  // widget, same rationale as the cuesheet refresher).
  document.addEventListener("cutepi-sync", refresh);

})();

// --- Cue Inspector: auto-refresh when the cuesheet re-renders ---
// The inspector is server-rendered for the current selection; every cuesheet
// swap (row click / arrow keys / add/delete/move / refresher / WebSocket / DnD)
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
  // A trim-timeline drag would die with the panel it is in: wait for the
  // release (trimline.js signals it), then follow.
  document.addEventListener("ctp-trim-released", () => {
    if (refreshDirty && !saveInFlight) { refreshDirty = false; refresh(); }
  });
  function refresh() {
    if (saveInFlight || window.ctpTrim?.pressing()) { refreshDirty = true; return; }
    clearTimeout(timer);
    timer = setTimeout(() => {
      // Cache-bust: without no-store headers the browser may reuse a cached
      // inspector for the previously selected entity and the wrong panel
      // appears.
      const url = inspectorURL();
      try { htmx.ajax("GET", url, {target: "#cueinspector-body", swap: "outerHTML"}); } catch (e) {}
    }, 0);
  }

  // Both entry points below see the same render: follow each new #cuesheet
  // node once (two fetches per click before).
  let followed = null;
  function sheetRendered() {
    const sheet = document.getElementById("cuesheet");
    if (!sheet || sheet === followed) return;
    followed = sheet;
    refresh();
  }
  document.body.addEventListener("htmx:after:swap", (e) => {
    const t = e.detail.ctx?.target;
    if (t && t.id === "cuesheet") sheetRendered();
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
  // refresher's replaceWith) replaces the #cuesheet node, so an observer bound
  // to that node dies on the first swap and never fires again. A new
  // #cuesheet appearing inside the pane means the sheet re-rendered and the
  // selection may have moved — re-fetch the inspector to follow it.
  const pane = document.getElementById("cuesheet-pane") || document.body;
  const observer = new MutationObserver((muts) => {
    for (const m of muts) {
      for (const n of m.addedNodes) {
        if (n.nodeType === Node.ELEMENT_NODE && (n.id === "cuesheet" || n.querySelector("#cuesheet"))) {
          sheetRendered();
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
  if (e.target.matches("[data-fade-out]")) {
    document.querySelectorAll("[data-fade-out]").forEach(input => { input.value = e.target.value; });
  }
  for (const id of ["insp-rate", "insp-balance"]) {
    if (e.target.id === id) document.getElementById(id + "-value").value = e.target.value + (id === "insp-rate" ? "×" : "");
  }
});

// Double-click on an inspector slider resets it to its default (volume
// 0 dB, rate 1×, balance centre); the rate slider also carries an explicit
// 1× reset button (§5.5).
function resetSlider(slider) {
  slider.value = slider.dataset.default;
  slider.dispatchEvent(new Event("input", {bubbles: true}));
  slider.dispatchEvent(new Event("change", {bubbles: true}));
}
document.addEventListener("dblclick", (e) => {
  if (!(e.target instanceof Element)) return;
  const slider = e.target.closest('input[type="range"][data-default]');
  if (slider) resetSlider(slider);
});
// Explicit reset buttons (the rate slider's "1×") share the same path.
document.addEventListener("click", (e) => {
  if (!(e.target instanceof Element)) return;
  const btn = e.target.closest("[data-reset-slider]");
  if (!btn) return;
  const slider = document.getElementById(btn.dataset.resetSlider);
  if (slider && slider.dataset.default !== undefined) resetSlider(slider);
});

// Inspector Audio tab output-device picker (§5.5): options come from the
// same enumeration as Settings > Audio (fetched once per page), and a
// change saves the system-wide device directly — it is not a cue field.
(function () {
  let devices = null;
  function fill() {
    const sel = document.getElementById("insp-audio-device");
    if (!sel || sel.dataset.filled) return;
    sel.dataset.filled = "1";
    if (!devices) {
      devices = fetch("/api/audio/devices", { headers: { Accept: "application/json" } })
        .then((r) => r.json()).then((b) => (b && b.devices) || []).catch(() => []);
    }
    devices.then((list) => {
      const current = sel.dataset.audioDevice || "";
      sel.innerHTML = "";
      const add = (value, label) => {
        const o = document.createElement("option");
        o.value = value;
        o.textContent = label;
        sel.appendChild(o);
      };
      add("", "HDMI embedded (default)");
      list.forEach((d) => add(d.id, d.label));
      if (current && !list.some((d) => d.id === current)) add(current, current);
      sel.value = current;
    });
  }
  document.addEventListener("change", (e) => {
    if (!(e.target instanceof Element) || e.target.id !== "insp-audio-device") return;
    const sel = e.target;
    fetch("/api/setting/audiodevice", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({ device: sel.value }),
    }).then((r) => {
      if (!r.ok) throw new Error("server returned " + r.status);
      sel.dataset.audioDevice = sel.value;
    }).catch((err) => console.error("CuTePi: audio device save failed", err));
  });
  document.addEventListener("DOMContentLoaded", fill);
  document.body && document.body.addEventListener("htmx:after:swap", () => Promise.resolve().then(fill));
  fill();
})();

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
    const t = document.getElementById("cueinspector-toggle");
    if (t) {
      t.setAttribute("aria-pressed", state ? "false" : "true");
      t.classList.toggle("active", !state);
    }
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
        b.classList.toggle("is-active", active);
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
    if (e.target.closest("#cueinspector-toggle")) {
      setCollapsed(!document.body.classList.contains("cueinspector-collapsed"));
    }
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
    const i = b.querySelector(".icon use");
    if (i) i.setAttribute("href", "/ftl/assets/icons/icons.svg#icon-" + (fs ? "minimize" : "maximize"));
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

// --- Cuesheet: change-detection refresh, driven by WebSocket signals ---
// Server is source of truth (selection + order persisted in DB). No polling
// (§6.7): each server signal pulls once, version-guarded; a (re)connect
// pulls once to catch up.
(function () {
  if (!document.getElementById("cuesheet")) return;
  let lastSeen = 0;
  // Seed lastSeen by fetching current version once; avoids an immediate
  // redundant full render on load.
  fetch("/api/cuesheet/status?version=0", {headers: {"Accept": "application/json"}})
    .then((r) => r.json()).then((b) => { lastSeen = b.version; }).catch(() => {});
  // A sheet that arrived some other way (an action's response: a row
  // click, GO, an edit) carries its version: no need to fetch it again when
  // the sync for that same change arrives.
  function domVersion() {
    const el = document.getElementById("cuesheet");
    const v = el ? parseInt(el.dataset.version, 10) : NaN;
    return isNaN(v) ? 0 : v;
  }
  // The server signals a change as soon as it is made, often before the
  // response of the request that made it (which carries the new sheet) has
  // landed. While such a request is in flight, wait for it, then check.
  let inFlight = 0, deferred = false;
  const sheetRequest = (e) => e.detail?.ctx?.target?.id === "cuesheet";
  document.body.addEventListener("htmx:before:request", (e) => { if (sheetRequest(e)) inFlight++; });
  document.body.addEventListener("htmx:finally:request", (e) => {
    if (!sheetRequest(e)) return;
    inFlight = Math.max(0, inFlight - 1);
    if (inFlight === 0 && deferred) { deferred = false; setTimeout(refresh, 0); }
  });
  async function refresh() {
    // Nor over an open inline editor: its save (or Escape) re-renders the
    // sheet, and that request's end runs the deferred check.
    if (inFlight > 0 || window.inlineEditing?.()) {
      deferred = true;
      // A double-click that opened no editor ends nothing: look again.
      if (inFlight === 0 && !document.querySelector("#cuesheet #updateValue")) setTimeout(refresh, 400);
      return;
    }
    try {
      if (domVersion() > lastSeen) lastSeen = domVersion();
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
  document.addEventListener("cutepi-ws", (e) => { if (e.detail.connected) refresh(); });
  // WebSocket "sync" wakes this refresher (single writer: it is the only
  // thing that swaps the cuesheet on a server signal, so two signals can't
  // race each other's re-render).
  document.addEventListener("cutepi-sync", refresh);
})();

// Server push is the only status channel (§6.7): the server signals every
// change (including one sync per displayed second while playing) and the
// version-guarded refreshers pull exactly then. While the socket is down
// the status dot goes red; the reconnect catches everything up.
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
      // Wake the authoritative refreshers; they are the only writers for the
      // cuesheet and now-playing widget, so no parallel htmx swap here (it
      // used to race the refresher's replaceWith: the htmx response could land
      // in a node the refresher had just detached, losing that update until
      // the next tick - and the double re-render flickered).
      document.dispatchEvent(new Event("cutepi-sync"));
    };
    socket.onopen = () => {
      document.dispatchEvent(new CustomEvent("cutepi-ws", {detail: {connected: true}}));
    };
    socket.onclose = () => {
      // Status dot goes red while down; reconnect in 2s (onopen catches up).
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
    } else if (msg.stage === "encoding") {
      // Downloads that aren't HEVC are re-encoded on the Pi (about 6x the video's length).
      var epct = Math.round((msg.pct || 0) * 10) / 10;
      stageEl.textContent = "Converting " + (msg.from ? msg.from.toUpperCase() + " " : "") + "to HEVC\u2026";
      bar.style.width = Math.min(100, epct) + "%";
      pctEl.textContent = epct + "%";
      spdEl.textContent = "";
      etaEl.textContent = msg.eta ? "ETA " + msg.eta : "";
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

  // DOMParser builds an inert document: unlike innerHTML on a live-document
  // element, an <img onerror> echoed back in an error message never runs.
  function stripHtml(html) {
    var doc = new DOMParser().parseFromString((html || "").trim(), "text/html");
    return (doc.body.textContent || "").trim();
  }
})();

// In-cell editor helper: a double-click on a cue/group field opens an inline
// editor, and those rows also select on a single click, which re-renders the
// whole sheet. The select is sent at once (no delay: a click selects as fast
// as an arrow key). Its response can land after a double-click has opened the
// editor (or while its request is out), and swapping the sheet then would
// wipe the editor: that swap is skipped. The selection is saved all the same,
// and the editor's own save (or Escape) brings the new sheet.
let inlineEditAt = 0;
document.addEventListener("dblclick", (e) => {
  const t = e.target;
  if (t instanceof Element && t.closest(".cue-inline-edit")) inlineEditAt = Date.now();
});
// The second click of a double-click (detail === 2) comes before the
// dblclick event: stamp the guard now, so a select response landing between
// the two is held back too.
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
// An inline editor is open in the sheet (or a double-click just asked for one).
window.inlineEditing = () => window.justEdited() || !!document.querySelector("#cuesheet #updateValue");
document.body.addEventListener("htmx:before:swap", (e) => {
  const src = e.detail?.ctx?.sourceElement;
  if (src instanceof Element && src.matches("tr.cue, tr.cuegroup") && window.inlineEditing()) e.preventDefault();
});

// QA harness: ?settings=1 opens the Settings modal directly (headless
// screenshot testing of the tabbed layout); harmless in normal use.
// QA harness (?settings=1 | ?settings=display|audio|...): renders the modal
// and the named pane with the state classes applied directly, no fade —
// deterministic headless screenshots of the tabbed sheet (the virtual-time
// screenshot harness races the browser's transition).
const qaSettings = new URLSearchParams(location.search).get("settings");
if (qaSettings) {
  const modal = document.getElementById("settingsModal");
  const pane = document.querySelector(qaSettings === "1"
    ? ".settings-panes .tab-pane:first-child"
    : '.settings-panes .tab-pane[id="' + (qaSettings === "appearance" ? "settingsPane" : "settingsTab") + qaSettings.charAt(0).toUpperCase() + qaSettings.slice(1) + '"]');
  if (modal && pane) {
    document.querySelectorAll(".settings-panes .tab-pane").forEach(p => p.classList.remove("show", "active"));
    modal.classList.add("show");
    modal.style.display = "block";
    modal.removeAttribute("aria-hidden");
    pane.classList.add("show", "active");
    document.body.insertAdjacentHTML("beforeend", '<div class="modal-backdrop fade show"></div>');
    pane.scrollIntoView({block: "start"});
    const rail = modal.querySelector('.settings-tabs .tab[data-bs-target="#' + pane.id + '"]');
    if (rail) rail.classList.add("is-active");
  }
}

// QA harness 2: ?modal=<id> force-shows any modal (uploadModal, ytdlModal,
// qrModal, testModal, logsModal, showModal, deleteModal) with its state
// classes applied directly — the same deterministic no-fade trick the
// settings harness uses. ?modal=<id>:<pane> also selects a pane.
const qaModal = new URLSearchParams(location.search).get("modal");
if (qaModal) {
  const m = document.getElementById(qaModal);
  if (m) {
    m.classList.add("show");
    m.style.display = "block";
    m.removeAttribute("aria-hidden");
    document.body.insertAdjacentHTML("beforeend", '<div class="modal-backdrop fade show"></div>');
  }
}

// A refused cue number (already used by another cue or group): the server
// re-rendered the old value; point at the field with the reason for a moment.
document.addEventListener("cueNumRejected", (e) => {
  const d = e.detail || {};
  const el = d.anchor && document.querySelector(d.anchor);
  if (!el || !window.bootstrap) return;
  if ("value" in el && el.tagName === "INPUT") el.value = d.value || "";
  const tip = new bootstrap.Tooltip(el, {title: d.message, trigger: "manual", placement: "bottom", container: "body", customClass: "cue-num-tooltip"});
  tip.show();
  setTimeout(() => tip.dispose(), 3500);
});

// --- Validated text fields (data-validate = time | int | number) ---
// Number-like settings are plain text inputs so every form the operator
// types is accepted: times as "1:05.000", "1m5s" or "65" (all 65 s, same
// grammar as ctp.ParseTime), whole numbers and decimals with optional
// data-min / data-max. An invalid entry is put back and explained on the
// field instead of reaching the server.
function cutepiParseTime(raw) {
  const s = String(raw || "").trim().toLowerCase().replace(/\s+/g, "");
  if (!s) return null;
  if (/[a-z]/.test(s)) {
    const units = { h: 3600000, hr: 3600000, m: 60000, min: 60000, s: 1000, sec: 1000, ms: 1 };
    const re = /(\d+(?:\.\d+)?)([a-z]+)/y;
    let total = 0, last = Infinity, m;
    re.lastIndex = 0;
    while (re.lastIndex < s.length) {
      if (!(m = re.exec(s))) return null;
      const u = units[m[2]];
      if (!u || u >= last) return null;
      last = u;
      total += parseFloat(m[1]) * u;
    }
    return Math.round(total);
  }
  const parts = s.split(":");
  if (parts.length > 3 || parts.some((p) => p === "")) return null;
  const sec = parts.pop();
  if (!/^\d+(\.\d+)?$/.test(sec) || parts.some((p) => !/^\d+$/.test(p))) return null;
  let total = parseFloat(sec);
  if (parts.length) total += parseInt(parts.pop(), 10) * 60;
  if (parts.length) total += parseInt(parts.pop(), 10) * 3600;
  return Math.round(total * 1000);
}
window.cutepiParseTime = cutepiParseTime;

function cutepiFieldProblem(el) {
  const kind = el.dataset.validate, v = el.value.trim();
  if (!kind || v === "") return "";
  if (kind === "time") return cutepiParseTime(v) === null ? "Enter a time like 1:05, 1m5s or 65 (seconds)" : "";
  if (kind === "geom") return /^-?\d+(\.\d+)?(px|%)?$/i.test(v) ? "" : "Enter pixels (960) or a percentage (50%)";
  const re = kind === "int" ? /^\d+$/ : /^-?\d+(\.\d+)?$/;
  if (!re.test(v)) return kind === "int" ? "Enter a whole number" : "Enter a number";
  const n = parseFloat(v), lo = el.dataset.min, hi = el.dataset.max;
  if (lo !== undefined && n < parseFloat(lo)) return "Must be at least " + lo;
  if (hi !== undefined && n > parseFloat(hi)) return "Must be at most " + hi;
  return "";
}

// A short error tip pinned under a field (or where it was, if it is re-rendered).
function cutepiFieldTip(el, message) {
  const r = el.getBoundingClientRect();
  const tip = document.createElement("div");
  tip.className = "tooltip bs-tooltip-bottom show cutepi-field-tip";
  tip.setAttribute("role", "alert");
  tip.innerHTML = '<div class="tooltip-inner"></div>';
  tip.firstChild.textContent = message;
  tip.style.cssText = "position:fixed;z-index:2100;left:" + Math.max(8, r.left) + "px;top:" + (r.bottom + 4) + "px";
  document.body.appendChild(tip);
  setTimeout(() => tip.remove(), 3500);
}
window.cutepiFieldTip = cutepiFieldTip;

// true = valid (or not a validated field). Invalid: value restored + tip.
function cutepiFieldOK(el) {
  const problem = cutepiFieldProblem(el);
  if (!problem) return true;
  cutepiFieldTip(el, problem);
  if (el.dataset.prev !== undefined) el.value = el.dataset.prev;
  return false;
}
window.cutepiFieldOK = cutepiFieldOK;

document.addEventListener("focusin", (e) => {
  const el = e.target;
  if (el instanceof HTMLInputElement && el.dataset.validate) el.dataset.prev = el.value;
}, true);
// Capture phase: runs before the forms' own change/submit handlers (htmx
// autosave), so a bad value never gets posted.
document.addEventListener("change", (e) => {
  const el = e.target;
  if (!(el instanceof HTMLInputElement) || !el.dataset.validate) return;
  if (!cutepiFieldOK(el)) { e.stopImmediatePropagation(); e.preventDefault(); }
  else el.dataset.prev = el.value;
}, true);
document.addEventListener("submit", (e) => {
  const bad = e.target.querySelector && Array.from(e.target.querySelectorAll("input[data-validate]")).find((el) => cutepiFieldProblem(el));
  if (bad) { e.preventDefault(); e.stopImmediatePropagation(); cutepiFieldOK(bad); bad.focus(); }
}, true);

// Sheet ordering actions (right-click menus and the top-bar menu): sort the
// sheet by cue number, or renumber every cue in sheet order (§12.5).
document.addEventListener("click", (e) => {
  const item = e.target instanceof Element ? e.target.closest("[data-sheet-order]") : null;
  if (!item) return;
  e.preventDefault();
  e.stopImmediatePropagation();
  document.querySelectorAll(".cue-context-menu").forEach((m) => { m.hidden = true; });
  if (isShowMode()) { showToast("Switch to EDIT mode to change the sheet"); return; }
  const renumber = item.dataset.sheetOrder === "renumber";
  if (renumber && !window.confirm("Renumber every cue in sheet order (1, 2, 3…)? Hand-set numbers are replaced.")) return;
  htmx.ajax("POST", renumber ? "/api/cue/renumber" : "/api/cue/sort", {target: "#cuesheet", swap: "outerHTML"});
}, true);

// Dialog dismiss buttons. Bootstrap's own handler finds the dialog with
// closest(".modal"), which now hits the themed window inside (ftl-themes v4
// styles .modal as the window; the overlay is .app-modal), so close the
// overlay here, ahead of Bootstrap.
document.addEventListener("click", (e) => {
  const btn = e.target instanceof Element ? e.target.closest('[data-bs-dismiss="modal"]') : null;
  const overlay = btn && btn.closest(".app-modal");
  if (!overlay || !window.bootstrap) return;
  e.preventDefault();
  e.stopImmediatePropagation();
  bootstrap.Modal.getOrCreateInstance(overlay).hide();
}, true);
