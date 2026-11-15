// Two-window lock-step check: after each action in window A (or a plain HTTP
// request, as curl would send), windows A and B must show the same panes
// (sheet, selection, now playing, active cues, inspector, media pool, mode)
// as the server's own render of a fresh page load (JavaScript off), and
// stay there: a match that later goes back is a stale render landing late.
// FAIL = never converged or went back (exit 1); "slow" = converged, but more
// than LIMIT ms after the server render first showed the final state.
// Dev stand-in only (AGENTS.md):
//   NODE_PATH=/root/uicheck/node_modules node sync.js http://localhost:3001 [rounds]
// Needs at least two cues in the sheet and an h264 .mp4, a .wav and a .png in
// the pool. Playback needs a sink: run the server with CUTEPI_WALL_SINK=fakesink.
const { chromium } = require("playwright");
const base = process.argv[2] || "http://localhost:3001";
const rounds = parseInt(process.argv[3] || "1", 10);
const LIMIT = 250; // latency goal, reported as "slow"
const FAIL_AFTER = 6000; // never converged
const SETTLED = 1500; // the server render held this long = final state

// Pane state as the user sees it: time-varying clocks are dropped.
function snapshot() {
  const norm = (s) => (s || "").replace(/\d+:\d{2}(:\d{2})?(\.\d+)?/g, "T").replace(/\s+/g, " ").trim();
  const keep = ["is-selected", "cue-playing", "cue-running", "cue-gcollapsed"];
  const rows = Array.from(document.querySelectorAll("#cuesheet tr.sheet-row"), (r) =>
    [keep.filter((k) => r.classList.contains(k)).join("."), r.dataset.cuePos || "g" + (r.dataset.groupId || ""),
      r.dataset.cueColor || "", r.dataset.cueAutoContinue || "", r.dataset.cueResult || "", norm(r.textContent)].join("|"));
  const np = document.getElementById("mediainfo");
  const ac = document.getElementById("activecues");
  const insp = document.getElementById("cueinspector-body");
  const form = insp && insp.querySelector("form");
  const fields = form ? Array.from(form.querySelectorAll("input[name], select[name], textarea[name]"),
    (f) => f.name + "=" + (f.type === "checkbox" || f.type === "radio" ? f.checked : f.value)).join("&") : "";
  const tiles = Array.from(document.querySelectorAll("#mediapool .media-tile"), (t) => t.dataset.mediaName + ":" + norm(t.textContent));
  const mode = document.getElementById("mode-toggle");
  return {
    sheet: rows,
    nowplaying: np ? norm(np.querySelector(".go-now")?.textContent) + " / " + norm(np.querySelector(".now-cluster")?.textContent) : "",
    activecues: ac ? ac.dataset.count + ": " + Array.from(ac.querySelectorAll("[data-cue-pos]"), (c) => c.dataset.cuePos + " " + norm(c.textContent)).join(" ; ") : "",
    inspector: insp ? (insp.dataset.groupId ? "group " + insp.dataset.groupId : (form?.getAttribute("hx-put") || "")) + " " + fields : "",
    mediapool: tiles,
    mode: document.body.classList.contains("show-mode") + " " + (mode ? mode.getAttribute("aria-checked") : ""),
  };
}

const diff = (a, b) => Object.keys(a).filter((k) => JSON.stringify(a[k]) !== JSON.stringify(b[k]));

async function api(method, path, body, type) {
  const r = await fetch(base + path, { method, body, headers: type ? { "Content-Type": type } : {} });
  if (!r.ok) throw new Error(`${method} ${path}: HTTP ${r.status}`);
  return r;
}

(async () => {
  const browser = await chromium.launch();
  const ctxA = await browser.newContext({ viewport: { width: 1600, height: 1000 } });
  const ctxB = await browser.newContext({ viewport: { width: 1600, height: 1000 } });
  // Keep a handle on B's socket so the check can drop it (offline mode alone
  // leaves an open WebSocket up).
  await ctxB.addInitScript(() => {
    const W = window.WebSocket;
    window.WebSocket = function (...a) { const s = new W(...a); window.__ws = s; return s; };
    Object.assign(window.WebSocket, W); window.WebSocket.prototype = W.prototype;
  });
  const A = await ctxA.newPage(), B = await ctxB.newPage();
  const errs = [];
  for (const [n, p] of [["A", A], ["B", B]]) {
    p.on("pageerror", (e) => errs.push(n + " exception: " + e.message));
    p.on("response", (r) => r.status() >= 400 && errs.push(`${n} HTTP ${r.status()} ${r.request().method()} ${r.url()}`));
  }
  await A.goto(base + "/"); await B.goto(base + "/");
  await A.waitForTimeout(800);
  // Clean start: Edit mode, nothing playing.
  await api("POST", "/api/setting/showmode", "", "application/x-www-form-urlencoded");
  await api("POST", "/api/stop");
  await A.waitForTimeout(800);

  const results = [];
  // Run one action, then watch both windows against the settled server state.
  async function step(name, act) {
    await act();
    const tAct = Date.now();
    const seen = { A: [], B: [] };
    const watchUntil = tAct + FAIL_AFTER;
    let ref = null, refAt = 0;
    const poll = async () => {
      while (Date.now() < watchUntil) {
        const t = Date.now();
        const [sa, sb] = await Promise.all([A.evaluate(snapshot), B.evaluate(snapshot)]);
        seen.A.push([t, sa]); seen.B.push([t, sb]);
        await new Promise((r) => setTimeout(r, 20));
        if (ref && Date.now() - refAt > 400) break;
      }
    };
    // The server's own render, sampled continuously: the state can settle
    // well after the request returns (a pipeline prerolls, a fade runs).
    const server = [];
    const refLoop = async () => {
      const c = await browser.newPage({ viewport: { width: 1600, height: 1000 }, javaScriptEnabled: false });
      while (Date.now() < watchUntil) {
        const t = Date.now();
        await c.goto(base + "/", { waitUntil: "domcontentloaded" });
        server.push([t, await c.evaluate(snapshot)]);
        const n = server.length;
        if (t - tAct > 600 && n > 1 && !diff(server[n - 1][1], server[n - 2][1]).length) {
          let k = n - 1;
          while (k > 0 && !diff(server[k - 1][1], server[n - 1][1]).length) k--;
          if (t - server[k][0] > SETTLED) break;
        }
      }
      await c.close();
      ref = server[server.length - 1][1]; refAt = Date.now();
    };
    await Promise.all([poll(), refLoop()]);
    const res = { name, ms: {}, fail: [], slow: [] };
    for (const w of ["A", "B"]) {
      const list = seen[w];
      // latency = first sample after which every later sample matches ref
      let firstOk = null;
      for (let i = list.length - 1; i >= 0; i--) {
        if (diff(list[i][1], ref).length) break;
        firstOk = list[i][0];
      }
      const last = list[list.length - 1][1];
      const d = diff(last, ref);
      if (d.length || firstOk === null) {
        res.fail.push(`${w} never matched (${d.join(",")}): ` + d.map((k) => `\n      ${k}: got ${JSON.stringify(last[k]).slice(0, 300)}\n      ${k}: ref ${JSON.stringify(ref[k]).slice(0, 300)}`).join(""));
        continue;
      }
      // a matching sample followed by a mismatch = an older render overwrote a newer one
      let regress = null;
      for (let i = 1; i < list.length; i++) {
        if (!diff(list[i - 1][1], ref).length && diff(list[i][1], ref).length && list[i][0] < firstOk) regress = diff(list[i][1], ref);
      }
      if (regress) res.fail.push(`${w} matched then regressed (${regress.join(",")})`);
      // when the server first rendered the final state
      let serverAt = server[server.length - 1][0];
      for (let i = server.length - 1; i >= 0 && !diff(server[i][1], ref).length; i--) serverAt = server[i][0];
      res.ms[w] = Math.max(0, firstOk - Math.max(tAct, serverAt));
      if (res.ms[w] > LIMIT) {
        // which pane was last to arrive
        const slow = Object.keys(ref).map((k) => {
          let at = null;
          for (let i = list.length - 1; i >= 0; i--) { if (JSON.stringify(list[i][1][k]) !== JSON.stringify(ref[k])) break; at = list[i][0]; }
          let sAt = server[server.length - 1][0];
          for (let i = server.length - 1; i >= 0 && JSON.stringify(server[i][1][k]) === JSON.stringify(ref[k]); i--) sAt = server[i][0];
          return [k, at - Math.max(tAct, sAt)];
        }).filter(([, ms]) => ms > LIMIT).map(([k, ms]) => `${k} ${ms}ms`);
        res.slow.push(`${w} took ${res.ms[w]}ms (> ${LIMIT}): ${slow.join(", ")}`);
      }
    }
    results.push(res);
    const tag = res.fail.length ? "FAIL" : res.slow.length ? "slow" : "ok  ";
    const why = [...res.fail, ...res.slow];
    console.log(`${tag} ${name.padEnd(34)} A ${String(res.ms.A ?? "-").padStart(5)}ms  B ${String(res.ms.B ?? "-").padStart(5)}ms` +
      (why.length ? "\n    " + why.join("\n    ") : ""));
  }

  const firstPos = async () => (await B.evaluate(() => Array.from(document.querySelectorAll("#cuesheet tr.cue[data-cue-pos]"), (r) => +r.dataset.cuePos)));
  const media = async () => B.evaluate(() => Array.from(document.querySelectorAll("#mediapool .media-tile"), (t) => t.dataset.mediaName));
  const inA = (fn, arg) => A.evaluate(fn, arg);

  for (let r = 0; r < rounds; r++) {
    if (rounds > 1) console.log(`--- round ${r + 1}`);
    let pos = await firstPos();
    const pool = await media();
    // Light media only: the stand-in decodes in software on two cores.
    const clip = pool.find((m) => /h264.*\.mp4$/.test(m)) || pool[0];
    const byMedia = async (re) => B.evaluate((src) => { const re = new RegExp(src);
      return Array.from(document.querySelectorAll("#cuesheet tr.cue[data-cue-pos]")).filter((r) => re.test(r.textContent) || re.test(r.querySelector(".cue-type")?.title || "")).map((r) => +r.dataset.cuePos); }, re.source);

    await step("A: click row (select)", () => A.locator(`#cuesheet tr.cue[data-cue-pos="${pos[1]}"] td.cue-type`).click());
    await step("A: arrow down", () => inA(() => htmx.ajax("POST", "/api/cue/next", { target: "#cuesheet", swap: "outerHTML" })));
    await step("A: inline edit title", async () => {
      await A.locator(`#cuesheet tr.cue[data-cue-pos="${pos[0]}"] td.cue-name .cue-inline-edit`).dblclick();
      const input = A.locator("#cuesheet #updateValue input");
      await input.waitFor();
      await input.fill("Sync " + r + " " + Date.now() % 1000);
      const saved = A.waitForResponse((res) => /\/edit\/title/.test(res.url()) && res.request().method() === "PUT", { timeout: 5000 });
      await input.press("Enter");
      await saved;
    });
    await step("A: inspector volume", async () => {
      await A.locator(`#cuesheet tr.cue[data-cue-pos="${pos[0]}"] td.cue-type`).click();
      await A.waitForTimeout(400);
      await inA((v) => { const s = document.getElementById("insp-volume"); s.value = v; s.dispatchEvent(new Event("input", { bubbles: true })); s.dispatchEvent(new Event("change", { bubbles: true })); }, String(-3 - r));
    });
    await step("A: inspector colour", async () => {
      await inA(() => { const s = document.querySelector('#inspector-form [name="color"]'); if (!s) throw new Error("no colour field"); s.value = s.value === "#ff0000" ? "#00ff00" : "#ff0000"; s.dispatchEvent(new Event("change", { bubbles: true })); });
    });
    await step("A: add cue", () => inA((f) => htmx.ajax("POST", "/api/cue/add/" + encodeURIComponent(f), { target: "#cuesheet", swap: "outerHTML" }), clip));
    pos = await firstPos();
    await step("A: move last cue to top", () => inA((p) => fetch("/api/sheet/drop", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ cues: [p.at(-1)], beforeKind: "cue", beforeId: p[0] }) }).then((x) => x.text()).then((h) => { const d = document.createElement("div"); d.innerHTML = h.trim(); const n = d.firstElementChild; if (n && n.id === "cuesheet") { document.getElementById("cuesheet").replaceWith(n); htmx.process(n); } }), pos));
    pos = await firstPos();
    await step("A: delete first cue", () => inA((p) => htmx.ajax("DELETE", "/api/cue/" + p, { target: "#cuesheet", swap: "outerHTML" }), pos[0]));
    pos = await firstPos();
    const clipPos = (await byMedia(/clip-h264/))[0], tonePos = (await byMedia(/^audio\//))[0] || clipPos;
    const stillPos = (await byMedia(/^image\/png/))[0] || clipPos;
    await step("A: select clip cue + GO", async () => {
      await A.locator(`#cuesheet tr.cue[data-cue-pos="${clipPos}"] td.cue-type`).click();
      await A.waitForTimeout(300);
      await A.locator("#go-btn").click();
    });
    await step("A: fade active cue", async () => {
      await A.locator("#activecues [hx-post$='/fade']").first().click();
    });
    await step("curl: play cue", () => api("POST", `/api/cue/${tonePos}/play`));
    await step("A: stop active cue", () => A.locator("#activecues [hx-post$='/stop']").first().click());
    await step("curl: play cue 2", () => api("POST", `/api/cue/${stillPos}/play`));
    await step("curl: stop all", () => api("POST", "/api/stop"));
    await step("curl: next cue", () => api("POST", "/api/cue/next"));
    await step("curl: rapid next x3", async () => { for (let i = 0; i < 3; i++) await api("POST", "/api/cue/next"); });
    await step("curl: rapid prev/next burst", () => Promise.all([api("POST", "/api/cue/prev"), api("POST", "/api/cue/next"), api("POST", "/api/cue/prev")]));
    await step("A: show mode on", () => A.locator("#mode-toggle").click());
    await step("A: show mode off", () => A.locator("#mode-toggle").click());
    await step("curl: go-advance toggle", () => api("POST", "/api/setting/goadvance", r % 2 ? "" : "advance=1", "application/x-www-form-urlencoded"));
    await step("A: upload media", () => inA(() => { const fd = new FormData(); fd.append("media", new File([new Uint8Array(atob("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==").split("").map((c) => c.charCodeAt(0)))], "synccheck.png", { type: "image/png" })); fd.append("onConflict", "replace"); return fetch("/upload", { method: "POST", body: fd, headers: { "HX-Request": "true" } }).then((x) => { if (!x.ok) throw new Error("upload " + x.status); }); }));
    await step("A: delete media", () => inA(() => fetch("/api/media/synccheck.png", { method: "DELETE" }).then((x) => { if (!x.ok) throw new Error("delete media " + x.status); })));
    // Reconnect: B's socket drops, server changes meanwhile, B must catch up.
    await step("B offline, curl next, reconnect", async () => {
      await ctxB.setOffline(true);
      await B.evaluate(() => window.__ws && window.__ws.close());
      await api("POST", "/api/cue/next");
      await api("POST", "/api/cue/prev");
      await api("POST", "/api/cue/next");
      await B.waitForTimeout(300);
      await ctxB.setOffline(false);
    });
  }
  await api("POST", "/api/stop");
  const bad = results.filter((r) => r.fail.length).length;
  const slow = results.filter((r) => !r.fail.length && r.slow.length).length;
  const all = results.flatMap((r) => Object.values(r.ms)).sort((a, b) => a - b);
  const pct = (q) => all[Math.min(all.length - 1, Math.floor(all.length * q))];
  console.log(`${results.length - bad}/${results.length} steps converged (${bad} failed, ${slow} slow > ${LIMIT}ms); ` +
    `latency median ${pct(0.5)}ms, p90 ${pct(0.9)}ms, max ${all.at(-1)}ms`);
  for (const e of errs) console.log("page error: " + e);
  await browser.close();
  process.exit(bad || errs.length ? 1 : 0);
})();
