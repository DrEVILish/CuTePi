// Linked units (DESIGN §6.8c): the Settings › Network section, the warning
// strip, and the failover page (phase 2): in Active/active backup the page
// also holds a WebSocket to each paired unit, and when the unit it was loaded
// from drops, every request goes to a paired unit instead (with a token the
// page's own unit signed for this origin) and the panes follow that unit.
(function () {
  "use strict";
  const origFetch = window.fetch.bind(window);

  function swapIn(id, html) {
    const cur = document.getElementById(id);
    if (!cur) return;
    const wrap = document.createElement("div");
    wrap.innerHTML = html.trim();
    const next = wrap.firstElementChild;
    if (!next || next.id !== id) return;
    cur.replaceWith(next);
    if (window.htmx) window.htmx.process(next);
  }

  // --- Settings section: loaded when the dialog opens, refreshed every 2 s
  // while it is open (not while the operator is in one of its controls).
  const modal = document.getElementById("settingsModal");
  if (modal) {
    let timer;
    const load = () => {
      const sec = document.getElementById("linkSection");
      if (!sec || sec.contains(document.activeElement)) return;
      fetch("/api/link/section", {headers: {Accept: "text/html"}})
        .then((r) => (r.ok ? r.text() : null)).then((t) => t && swapIn("linkSection", t)).catch(() => {});
    };
    modal.addEventListener("shown.bs.modal", () => { load(); clearInterval(timer); timer = setInterval(load, 2000); });
    modal.addEventListener("hidden.bs.modal", () => clearInterval(timer));
  }

  // --- Warning strip: every 3 s while linked, every 30 s otherwise.
  if (document.getElementById("linkBanner")) {
    const banner = () => {
      fetch("/api/link/banner", {headers: {Accept: "text/html"}})
        .then((r) => (r.ok ? r.text() : null))
        .then((t) => {
          if (t) swapIn("linkBanner", t);
          const mode = document.getElementById("linkBanner")?.dataset.mode;
          setTimeout(banner, mode ? 3000 : 30000);
        })
        .catch(() => setTimeout(banner, 3000));
    };
    banner();
  }

  // --- Failover (Active/active backup only).
  if (!document.getElementById("cuesheet") || !window.WebSocket) return;
  let peers = [];          // [{id, name, url, token, ws, up}]
  let selfUp = true;
  let active = null;       // the peer serving this page, null = its own unit
  const note = document.getElementById("linkFailover");

  function pick() {
    const next = selfUp ? null : peers.find((p) => p.up) || null;
    if (next === active) return;
    active = next;
    // The panes now follow another unit, whose versions count separately:
    // let any render replace what is on screen, then pull everything.
    const sheet = document.getElementById("cuesheet");
    if (sheet) sheet.dataset.version = "0";
    if (note) {
      note.hidden = !active;
      note.textContent = active ? "Lost the connection to " + location.host + ": working through the linked unit " +
        active.name + " (" + active.url + "). Everything keeps working; reload the page from " + active.url + " when convenient." : "";
    }
    document.dispatchEvent(new CustomEvent("cutepi-ws", {detail: {connected: true, link: true}}));
  }

  document.addEventListener("cutepi-ws", (e) => {
    if (e.detail?.link) return;
    selfUp = !!e.detail?.connected;
    if (selfUp) loadPeers();
    pick();
  });

  function connect(p) {
    // The token rides as a subprotocol: a URL would land in the request log.
    const ws = new WebSocket(p.url.replace(/^http/, "ws") + "/api/ws", ["cutepi-link", p.token]);
    p.ws = ws;
    ws.onopen = () => { p.up = true; pick(); };
    ws.onmessage = (ev) => {
      if (active !== p) return;
      let msg;
      try { msg = JSON.parse(ev.data); } catch (err) { return; }
      if (msg.type === "sync") document.dispatchEvent(new CustomEvent("cutepi-sync", {detail: msg.v || {}}));
    };
    ws.onclose = () => {
      p.up = false;
      pick();
      if (peers.includes(p)) setTimeout(() => peers.includes(p) && connect(p), 2000);
    };
    ws.onerror = () => ws.close();
  }

  function loadPeers() {
    origFetch("/api/link/page", {headers: {Accept: "application/json"}})
      .then((r) => (r.ok ? r.json() : null))
      .then((d) => {
        if (!d) return;
        const keep = [];
        for (const np of d.peers || []) {
          const old = peers.find((p) => p.id === np.id && p.url === np.url);
          if (old) { old.token = np.token; keep.push(old); continue; }
          const p = Object.assign({up: false}, np);
          keep.push(p);
          connect(p);
        }
        for (const p of peers) if (!keep.includes(p)) { peers = peers.filter((x) => x !== p); p.ws?.close(); }
        peers = keep;
        pick();
      })
      .catch(() => {});
  }
  loadPeers();
  setInterval(() => selfUp && loadPeers(), 10 * 60 * 1000);

  // Every request of the page (htmx and ui.js alike) goes through fetch:
  // while failed over, a same-origin request goes to the active peer. A 503
  // while the units choose a primary (2-2.5 s) is retried.
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  window.fetch = async function (input, init) {
    let url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    if (url.startsWith(location.origin + "/")) url = url.slice(location.origin.length);
    if (!url.startsWith("/") || url.startsWith("//") || peers.length === 0) return origFetch(input, init);
    for (let attempt = 0; ; attempt++) {
      let res;
      const via = active;
      try {
        if (!via) {
          res = await origFetch(input, init);
        } else {
          const opts = Object.assign({}, init || {});
          const headers = new Headers(opts.headers || (input instanceof Request ? input.headers : undefined));
          headers.set("X-CuTePi-Link", via.token);
          opts.headers = headers;
          opts.mode = "cors";
          opts.credentials = "omit";
          res = await origFetch(via.url + url, opts);
        }
      } catch (err) {
        // This page's unit went away mid-request: try a paired unit.
        if (!via && peers.some((p) => p.up) && attempt < 8) { selfUp = false; pick(); continue; }
        throw err;
      }
      if (res.status !== 503 || !res.headers.get("X-CuTePi-Link-Election") || attempt >= 8) return res;
      await sleep(400);
    }
  };
})();
