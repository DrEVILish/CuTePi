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
  showToast("Request failed (" + status + ")" + (detail ? ": " + detail : ""));
});

// Error responses are deliberately not swapped into application panels. Keep
// the originating YouTube form useful by showing its sanitized server error
// in the modal instead of failing silently.
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

const appThemes = new Set(["lcars", "qlab", "blue-future", "custom"]);
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
  if (!active || !["input", "textarea", "select", "button"].includes(active.nodeName.toLowerCase())) {
    if(["Space","ArrowUp","ArrowDown"].indexOf(e.code) > -1) {
      e.preventDefault();
    }
    if (["Space"].indexOf(e.code) > -1) {
      htmx.trigger("#spaceBar", "spaceBar")
    }
    if (["ArrowUp"].indexOf(e.code) > -1) {
      htmx.trigger("#ArrowUp", "ArrowUp")
    }
    if (["ArrowDown"].indexOf(e.code) > -1) {
      htmx.trigger("#ArrowDown", "ArrowDown")
    }
    if (["Escape"].indexOf(e.code) > -1) {
      htmx.trigger("#esc", "esc")
    }
  }
}, false);

function filterMedia() {
  var text = (document.getElementById("mediaFilterText") || {}).value || "";
  var type = (document.getElementById("mediaFilterType") || {}).value || "all";
  text = text.trim().toLowerCase();
  document.querySelectorAll("#mediapool .media-tile").forEach(function (tile) {
    var name = (tile.getAttribute("data-media-name") || "").toLowerCase();
    var tileType = tile.getAttribute("data-media-type") || "other";
    var matchesText = text === "" || name.indexOf(text) > -1;
    var matchesType = type === "all" || tileType === type;
    tile.style.display = matchesText && matchesType ? "" : "none";
  });
}

// Re-apply the media-pool filters whenever htmx processes newly-swapped pool
// content (this covers both htmx swaps and the drag-and-drop path, which calls
// htmx.process()). The old "mediapool-updated" custom event had no dispatcher
// and was removed.
document.addEventListener("htmx:load", filterMedia);
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

// --- CueList column widths: drag a header's right edge to resize ---
// Widths persist in localStorage and are restored on load and after every
// re-render. The cuesheet is re-rendered both by htmx (innerHTML swap of
// #cuesheet) and by the file drag-and-drop (which replaces the element
// outright), so mousedown is delegated and a MutationObserver re-applies
// widths whenever a fresh header appears. Runs on the control-centre page
// only (guards on element presence, so it is a no-op on /upload).
(function () {
  const sheet = document.getElementById("cuesheet");
  if (!sheet) return;

  const STORAGE_KEY = "cutepi.cuesheet.colWidths";
  const MIN_WIDTH = 40;
  const MAX_WIDTH = 600;
  const DEFAULTS = {
    cueName: 240,
    preWait: 110,
    cueDur: 115,
    postWait: 115,
  };

  let dragging = false;

  function loadWidths() {
    try {
      return JSON.parse(localStorage.getItem(STORAGE_KEY)) || {};
    } catch (e) {
      return {};
    }
  }
  function saveWidths(w) {
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify(w));
    } catch (e) {}
  }
  function clamp(w) {
    return Math.min(MAX_WIDTH, Math.max(MIN_WIDTH, w));
  }

  function applyWidths() {
    if (dragging) return;
    const stored = loadWidths();
    document.querySelectorAll("#cuesheet th[data-column-resize]").forEach((th) => {
      const key = th.getAttribute("data-column-resize");
      const saved = parseInt(stored[key], 10);
      th.style.width = clamp(isNaN(saved) ? DEFAULTS[key] || 120 : saved) + "px";
    });
  }

  // Delegated: the header is recreated on every cuesheet re-render.
  document.addEventListener("mousedown", (e) => {
    const handle = e.target.closest("#cuesheet .col-resize-handle");
    if (!handle) return;
    e.preventDefault();
    const th = handle.parentElement;
    const key = th.getAttribute("data-column-resize");
    const startX = e.clientX;
    const startWidth = th.getBoundingClientRect().width;
    let lastX = startX;

    dragging = true;
    handle.classList.add("dragging");

    const onMove = (ev) => {
      lastX = ev.clientX;
      th.style.width = clamp(startWidth + (ev.clientX - startX)) + "px";
    };
    const onUp = () => {
      dragging = false;
      handle.classList.remove("dragging");
      document.removeEventListener("mousemove", onMove);
      document.removeEventListener("mouseup", onUp);
      const stored = loadWidths();
      stored[key] = clamp(startWidth + (lastX - startX));
      saveWidths(stored);
    };
    document.addEventListener("mousemove", onMove);
    document.addEventListener("mouseup", onUp);
  });

  // Restore persisted widths on load.
  applyWidths();

  // Re-apply after any re-render (htmx innerHTML swap or the drag-and-drop's
  // full node replacement), which always creates the header fresh.
  new MutationObserver((mutations) => {
    for (const m of mutations) {
      for (const node of m.addedNodes) {
        if (node.nodeType !== Node.ELEMENT_NODE) continue;
        if (
          (node.matches("thead") || node.querySelector("thead")) &&
          node.closest("#cuesheet")
        ) {
          applyWidths();
          return;
        }
      }
    }
  }).observe(document.body, { childList: true, subtree: true });
})();

// Delete is handled outside htmx because the control lives inside a native
// draggable row. Capture the click before row-selection handlers can consume
// it, then replace the authoritative cuesheet returned by the server.
document.addEventListener("click", (e) => {
  const button = e.target instanceof Element ? e.target.closest("[data-cue-delete]") : null;
  if (!button) return;
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
  });
  document.addEventListener("pointerdown", (e) => {
    const bar = e.target.closest(".cue-progress-bar");
    if (!bar) return;
    e.preventDefault();
    const seek = (ev) => {
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
const CUE_COLOURS = [
  ["Red", "#dc3545"], ["Orange", "#fd7e14"], ["Yellow", "#ffc107"],
  ["Green", "#28a745"], ["Teal", "#20c997"], ["Cyan", "#17a2b8"],
  ["Blue", "#007bff"], ["Indigo", "#6610f2"], ["Purple", "#a855f7"],
  ["Pink", "#e83e8c"], ["Brown", "#795548"], ["Grey", "#6c757d"],
];
function patternOptions() {
  return '<option value="">None</option>' + CUE_COLOURS.map((c) =>
    `<option value="${c[1]}">${c[0]}</option>`).join("");
}
(function () {
  let menuEl = null;

  function ensureMenu() {
    if (menuEl) return menuEl;
    menuEl = document.createElement("div");
    menuEl.id = "cue-context-menu";
    menuEl.className = "cue-context-menu";
    menuEl.hidden = true;
    menuEl.innerHTML = `
      <div class="cue-context-item" data-cue-action="play"><i class="bi bi-play-fill"></i> Play</div>
      <div class="cue-context-item cue-context-color" data-cue-action="colour">
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
      <div class="cue-context-item cue-context-danger" data-cue-action="delete"><i class="bi bi-trash3"></i> Delete cue</div>`;
    document.body.appendChild(menuEl);
    return menuEl;
  }

  function hideMenu() {
    if (menuEl) menuEl.hidden = true;
  }

  // Re-render the cuesheet partial from the server after any mutation.
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

  document.addEventListener("contextmenu", (e) => {
    const row = e.target instanceof Element ? e.target.closest("tr.cue[data-cue-pos]") : null;
    if (!row) return; // default browser menu elsewhere
    e.preventDefault();
    const m = ensureMenu();
    m.dataset.cuePos = row.dataset.cuePos;
    m.dataset.cueAutoContinue = row.dataset.cueAutoContinue || "false";
    m.dataset.cueFadeAction = row.dataset.cueFadeAction || "peers";
    m.dataset.cueFadeOut = row.dataset.cueFadeOut || "0";
    m.dataset.cueColor = row.dataset.cueColor || "";

    // Auto-continue label
    m.querySelector('[data-cue-action="autofollow"] span').textContent =
      "Auto-continue: " + (m.dataset.cueAutoContinue === "true" ? "on" : "off");
    // Fade scope + time
    const fadeSel = m.querySelector(".cue-context-fade select");
    fadeSel.value = m.dataset.cueFadeAction;
    const fadeTime = m.querySelector(".cue-context-fade input[type=text]");
    fadeTime.value = msToClock((parseInt(m.dataset.cueFadeOut, 10) || 0));
    // Colour select (12 swatches + None), showing the cue's current colour.
    const col = m.querySelector(".cue-context-color select");
    if (col) col.value = m.dataset.cueColor || "";

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
    if (item) {
      const action = item.dataset.cueAction;
      hideMenu();
      if (action === "play") {
        fetch("/api/cue/" + encodeURIComponent(pos) + "/play", {method: "POST"})
          .then((res) => {
            if (!res.ok) showToast("Play cue failed (server returned " + res.status + ")");
          });
      } else if (action === "loop") {
        const newVal = menuEl.dataset.cueLoop === "true" ? "false" : "true";
        menuEl.dataset.cueLoop = newVal;
        putCol(pos, "loop", newVal);
      } else if (action === "autofollow") {
        const newVal = menuEl.dataset.cueAutoContinue === "true" ? "false" : "true";
        menuEl.dataset.cueAutoContinue = newVal;
        putCol(pos, "autoContinue", newVal);
      } else if (action === "delete") {
        fetch("/api/cue/" + encodeURIComponent(pos), {method: "DELETE"})
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
            console.error("CuTePi: delete cue failed", err);
            showToast("Delete cue failed: " + err.message);
          });
      }
      return;
    }
    // Fade-stop-others: persist scope on change.
    const fadeSel = menuEl.querySelector(".cue-context-fade select");
    fadeSel.onchange = () => {
      putCol(menuEl.dataset.cuePos, "fadeAction", fadeSel.value).catch((err) =>
        console.error("CuTePi: fadeAction update failed", err));
    };
    // Fade-stop-others: persist duration on change (parse mm:ss or seconds).
    const fadeTime = menuEl.querySelector(".cue-context-fade input[type=text]");
    fadeTime.onchange = () => {
      const seconds = parseFadeTime(fadeTime.value);
      if (seconds < 0) {
        console.error("CuTePi: invalid fade time", fadeTime.value);
        return;
      }
      const ms = Math.round(seconds * 1000);
      putCol(menuEl.dataset.cuePos, "fadeOut", "" + ms).catch((err) =>
        console.error("CuTePi: fadeOut update failed", err));
    };
    // Colour select: persist on change (empty = clear colour).
    const colourSelect = menuEl.querySelector(".cue-context-color select");
    colourSelect.onchange = () => {
      putCol(menuEl.dataset.cuePos, "color", colourSelect.value).catch((err) =>
        console.error("CuTePi: colour update failed", err));
    };
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

  fetch("/api/settings")
    .then((res) => res.json())
    .then((settings) => {
      if (Number.isFinite(settings.pollInterval) && settings.pollInterval >= 10) {
        pollMs = settings.pollInterval;
      }
    })
    .catch(() => {})
    .finally(refreshAndSchedule);

  async function refreshAndSchedule() {
    await refresh();
    setTimeout(refreshAndSchedule, pollMs);
  }
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
      // widget (the swap would reset the thumb mid-drag). The next poll tick
      // catches up once the drag ends.
      const scrubber = document.querySelector("#nowplaying-scrubber");
      if (scrubber && scrubber.matches(":active")) return;
      const res = await fetch("/api/nowplaying", { headers: { "Accept": "text/html" } });
      const el = document.getElementById("mediainfo");
      if (el) el.outerHTML = await res.text();
    } catch (e) {
      // Transient network/server error; the next poll tick will retry.
    }
  }

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
  const sheetEl = document.getElementById("cuesheet");
  if (!bodyEl || !sheetEl) return;

  let timer = null;
  function refresh() {
    clearTimeout(timer);
    timer = setTimeout(() => {
      // Cache-bust: without no-store headers the browser may reuse a cached
      // inspector for the previously selected cue and the wrong cue appears.
      const url = "/api/cue/inspector?_=" + Date.now();
      try { htmx.ajax("GET", url, {target: "#cueinspector-body", swap: "outerHTML"}); } catch (e) {}
    }, 0);
  }

  document.body.addEventListener("htmx:after:swap", (e) => {
    const t = e.detail.ctx?.target;
    if (t && t.id === "cuesheet") refresh();
  });

  const observer = new MutationObserver((muts) => {
    for (const m of muts) {
      for (const n of m.addedNodes) {
        if (n.nodeType === Node.ELEMENT_NODE && (n.matches("tbody") || n.querySelector("tbody"))) {
          refresh();
          return;
        }
      }
    }
  });
  observer.observe(sheetEl, { childList: true, subtree: true });
})();

// Keep the inspector's read-only dB readout in sync with its vertical slider.
document.addEventListener("input", (e) => {
  if (e.target.id === "insp-volume") {
    const db = document.getElementById("insp-volume-db");
    if (db) db.value = e.target.value;
  }
});

// --- Cuesheet: change-detection polling for multi-browser sync ---
// Server is source of truth (selection + order persisted in DB). This poller
// keeps every open control tab in sync within ~1s without WebSockets: it
// polls GET /api/cuesheet/status and re-renders the cuesheet (and via the
// afterSwap hook, the inspector) only when the version changes.
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
  setInterval(refresh, POLL_MS);
})();

// Prefer server push for cross-browser updates. The existing pollers remain
// active as the reconnect/fallback path when WebSockets are unavailable.
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
        if (document.getElementById("mediapool")) {
          htmx.ajax("GET", "/mediapool", {target: "#mediapool", swap: "outerHTML"});
        }
        return;
      }
      if (type !== "sync") return;
      // Force the normal authoritative pollers to fetch fresh state.
      document.dispatchEvent(new Event("cutepi-sync"));
      const cuesheet = document.getElementById("cuesheet");
      if (cuesheet) {
        htmx.ajax("GET", "/api/cuesheet", {target: "#cuesheet", swap: "outerHTML"});
      }
      const info = document.getElementById("mediainfo");
      if (info) {
        htmx.ajax("GET", "/api/nowplaying", {target: "#mediainfo", swap: "outerHTML"});
      }
    };
    socket.onclose = () => {
      clearTimeout(retry);
      retry = setTimeout(connect, 2000);
    };
    socket.onerror = () => socket.close();
  }
  connect();
})();
