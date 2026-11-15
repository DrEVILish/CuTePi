// UI review: drives the control UI like an operator and reports every console
// error, uncaught exception, failed request, HTTP >= 400 and layout overflow.
// node review.js <base> <outdir> [default]   (dev server / CT 110 only, AGENTS.md)
// "default" skips the every-theme sweep and runs the flows plus the
// default-theme width/accessibility pass only.
const { chromium } = require("playwright");
const fs = require("fs");
const base = process.argv[2] || "http://localhost:3001";
const out = process.argv[3] || "/root/uicheck/out";
fs.mkdirSync(out, { recursive: true });
const problems = [];
const note = (where, what) => { problems.push(`${where}: ${what}`); };

function watch(page, where) {
  page.on("pageerror", (e) => note(where(), "exception: " + e.message));
  page.on("console", (m) => { if (m.type() === "error") note(where(), "console: " + m.text().slice(0, 300)); });
  page.on("requestfailed", (r) => {
    // Aborts from a reload, and fire-and-forget POSTs whose body is never read.
    if ((r.failure() || {}).errorText === "net::ERR_ABORTED" && (where() === "flow/clear" || /showmode/.test(r.url()))) return;
    if (!/ws$|\/ws\b/.test(r.url())) note(where(), "request failed: " + r.method() + " " + r.url() + " " + (r.failure() || {}).errorText); });
  page.on("response", (r) => { if (r.status() >= 400) note(where(), `HTTP ${r.status()} ${r.request().method()} ${r.url()}`); });
}

async function overflow(page, where) {
  const o = await page.evaluate(() => {
    const d = document.documentElement;
    const res = [];
    if (d.scrollWidth > d.clientWidth + 1) res.push(`page scrolls horizontally (${d.scrollWidth} > ${d.clientWidth})`);
    // The working panes must get real height (a theme's shell grid fighting the
    // app's layout once squashed them to 44px under liquid-glass).
    const cs = document.getElementById("cuesheet-pane");
    if (cs && cs.offsetParent && cs.getBoundingClientRect().height < innerHeight * 0.2)
      res.push(`cuesheet pane only ${cs.getBoundingClientRect().height | 0}px high`);
    const nm = document.querySelector("#cuesheet thead th:nth-child(3)");
    if (nm && nm.offsetParent && nm.getBoundingClientRect().width < 100) res.push(`cue Name column only ${nm.getBoundingClientRect().width | 0}px wide`);
    const top = document.querySelector(".app-bar"), main = document.querySelector(".app-content");
    if (top && main && main.getBoundingClientRect().top - top.getBoundingClientRect().bottom > 40)
      res.push(`${main.getBoundingClientRect().top - top.getBoundingClientRect().bottom | 0}px gap between top bar and panes`);
    const ft = document.getElementById("app-footer");
    const scrolls = getComputedStyle(document.body).overflowY !== "hidden" && getComputedStyle(d).overflowY !== "hidden";
    if (ft && !scrolls && ft.getBoundingClientRect().bottom > innerHeight + 1) res.push("footer below the viewport, page cannot scroll to it");
    return res;
  });
  o.forEach((x) => note(where, x));
}

const MODALS = ["uploadModal", "ytdlModal", "liveModal", "showModal", "logsModal", "qrModal", "settingsModal", "testModal"];
async function openModal(page, m) {
  const opener = page.locator(`[data-bs-target="#${m}"]`).first();
  if (await opener.count()) {
    if (!(await opener.isVisible())) { await page.locator('#app-menu-btn, [data-bs-toggle="dropdown"]').first().click(); }
    await opener.click();
  } else if (m === "settingsModal") await page.locator("#app-footer [data-settings-open]").click();
  else if (m === "testModal") await page.locator("[data-test-open]").first().click();
  else await page.evaluate((id) => bootstrap.Modal.getOrCreateInstance(document.getElementById(id)).show(), m);
}

// Accessibility and fit of whatever is on screen (scope = a selector, the
// open modal or the page): controls without a name, text below WCAG AA
// contrast, controls under 24px on touch widths, content cut off sideways.
async function a11y(page, where, scope) {
  const r = await page.evaluate((scope) => {
    const root = document.querySelector(scope) || document.body, res = [];
    const vis = (e) => { const b = e.getBoundingClientRect(), s = getComputedStyle(e); return b.width > 0 && b.height > 0 && s.visibility !== "hidden" && +s.opacity > 0.05 && e.offsetParent !== null && b.bottom > 0 && b.top < innerHeight; };
    const id = (e) => e.tagName.toLowerCase() + (e.id ? "#" + e.id : "") + (typeof e.className === "string" && e.className ? "." + e.className.trim().split(/\s+/).slice(0, 3).join(".") : "");
    for (const e of root.querySelectorAll("button, a[href], input:not([type=hidden]), select, textarea, [role=button], [role=tab], [role=switch]")) {
      if (!vis(e)) continue;
      const lab = e.getAttribute("aria-label") || (e.getAttribute("aria-labelledby") && document.getElementById(e.getAttribute("aria-labelledby").split(" ")[0])) || (e.id && document.querySelector(`label[for="${CSS.escape(e.id)}"]`)) || e.closest("label") || e.textContent.trim() || e.title || (e.querySelector("img[alt]") || {}).alt;
      if (!lab) res.push("no accessible name: " + id(e));
      const b = e.getBoundingClientRect();
      if (innerWidth <= 768 && (b.width < 24 || b.height < 24) && !/^(checkbox|radio)$/.test(e.type) && !e.closest("label")) res.push(`tap target ${b.width | 0}x${b.height | 0}: ` + id(e));
    }
    // rgb()/rgba(), and color(srgb r g b / a) whose channels are 0..1.
    const parse = (c) => { const m = c.match(/[\d.]+(?:e-?\d+)?/g); if (!m) return null; const v = m.map(Number); return /^color\(/.test(c) ? [v[0] * 255, v[1] * 255, v[2] * 255, v[3] === undefined ? 1 : v[3]] : v; };
    const lum = ([r, g, b]) => { const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }; return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b); };
    // Composite translucent backgrounds up the tree; give up on images/gradients.
    const bgOf = (e) => { const st = []; for (; e; e = e.parentElement) { const s = getComputedStyle(e); if (s.backgroundImage !== "none") return null; const c = parse(s.backgroundColor); if (c && (c[3] === undefined || c[3] > 0)) { st.push(c); if (c[3] === undefined || c[3] >= 1) break; } } let out = [255, 255, 255]; if (st.length && (st[st.length - 1][3] === undefined || st[st.length - 1][3] >= 1)) out = st.pop().slice(0, 3); while (st.length) { const c = st.pop(); out = out.map((v, i) => c[i] * c[3] + v * (1 - c[3])); } return out; };
    const seen = {};
    for (const e of root.querySelectorAll("*")) {
      if (![...e.childNodes].some((n) => n.nodeType === 3 && n.textContent.trim().length > 1) || !vis(e) || e.closest("[disabled], .row-disabled, [aria-disabled=true]")) continue;
      const s = getComputedStyle(e), fg = parse(s.color), bg = bgOf(e);
      if (!fg || !bg) continue;
      const a = fg[3] === undefined ? 1 : fg[3], mix = fg.slice(0, 3).map((v, i) => v * a + bg[i] * (1 - a));
      const L1 = lum(mix), L2 = lum(bg), cr = (Math.max(L1, L2) + 0.05) / (Math.min(L1, L2) + 0.05);
      const px = parseFloat(s.fontSize), big = px >= 24 || (px >= 18.66 && +s.fontWeight >= 700);
      if (cr < (big ? 3 : 4.5) && !seen[id(e)]) { seen[id(e)] = 1; res.push(`contrast ${cr.toFixed(2)}:1 ${id(e)} "${e.textContent.trim().slice(0, 24)}"`); }
    }
    for (const e of root.querySelectorAll(".modal-content, .modal-content *, .app-bar > * > *, #app-footer > *")) {
      if (!vis(e)) continue;
      const b = e.getBoundingClientRect();
      // Content of a sideways scroller (a tab strip on a phone) is reachable.
      let sc = e.parentElement;
      while (sc && !(/auto|scroll/.test(getComputedStyle(sc).overflowX) && sc.scrollWidth > sc.clientWidth + 1)) sc = sc.parentElement;
      if (sc && sc !== document.documentElement && sc !== document.body && !sc.classList.contains("app-modal")) continue;
      if (b.right > innerWidth + 1 || b.left < -1) { res.push(`cut off sideways (${b.left | 0}..${b.right | 0} of ${innerWidth}): ` + id(e)); break; }
    }
    return res;
  }, scope || "body");
  r.forEach((x) => note(where, x));
}

// Keyboard: Tab through the page; every stop must be visible and show a
// focus indicator (outline or box-shadow).
async function focusWalk(page, where, stops) {
  for (let i = 0; i < stops; i++) {
    await page.keyboard.press("Tab");
    const bad = await page.evaluate(() => {
      let e = document.activeElement;
      if (!e || e === document.body) return null;
      // A visually hidden input inside a styled label (switch, swatch,
      // toggle button): the label carries the focus ring.
      if (e.matches("input") && e.closest("label") && e.getBoundingClientRect().width < 4) {
        e = e.closest("label");
        if ([e, ...e.querySelectorAll("*")].some((x) => { const t = getComputedStyle(x); return (t.outlineStyle !== "none" && parseFloat(t.outlineWidth) > 0) || t.boxShadow !== "none"; })) return null;
      }
      const s = getComputedStyle(e), b = e.getBoundingClientRect();
      const name = e.tagName.toLowerCase() + (e.id ? "#" + e.id : "") + (typeof e.className === "string" && e.className ? "." + e.className.trim().split(/\s+/)[0] : "");
      if (!b.width || !b.height) return "focus lands on an invisible element: " + name;
      const ring = (s.outlineStyle !== "none" && parseFloat(s.outlineWidth) > 0) || s.boxShadow !== "none";
      return ring ? null : "no focus indicator: " + name;
    });
    if (bad) note(where, bad);
  }
}

(async () => {
  const browser = await chromium.launch();
  const themes = await (await fetch(base + "/api/themes")).json();
  let ctxName = "";
  // 1. Every theme: load, screenshot, overflow at desktop and phone width.
  for (const vp of process.argv[4] === "default" ? [] : [{ width: 1600, height: 900, n: "desktop" }, { width: 1024, height: 768, n: "small" }, { width: 800, height: 1280, n: "tablet" }, { width: 390, height: 844, n: "phone" }]) {
    for (const t of themes) {
      const page = await browser.newPage({ viewport: vp });
      ctxName = `${t.name}/${vp.n}`;
      watch(page, () => ctxName);
      await page.goto(`${base}/?theme=${t.name}`, { waitUntil: "networkidle" });
      await overflow(page, ctxName);
      if (vp.n === "desktop" || ["aqua", "lcars", "msdos", "material"].includes(t.name))
        await page.screenshot({ path: `${out}/theme-${t.name}-${vp.n}.png` });
      await page.close();
    }
  }
  // 2. Workflows on the default theme.
  const page = await browser.newPage({ viewport: { width: 1600, height: 900 } });
  watch(page, () => ctxName);
  const step = async (name, fn) => {
    ctxName = "flow/" + name;
    try { await fn(); } catch (e) { note(ctxName, "step failed: " + e.message.split("\n")[0]); }
    await page.waitForTimeout(400);
    await page.screenshot({ path: `${out}/flow-${name}.png` });
  };
  await page.goto(base + "/", { waitUntil: "networkidle" });
  await step("clear", async () => { await fetch(base + "/api/clear", { method: "POST", headers: { Origin: base } }); await page.reload({ waitUntil: "networkidle" }); });
  await step("add-media-dblclick", async () => {
    const tiles = page.locator("#mediapool .media-tile");
    const n = await tiles.count();
    if (!n) throw new Error("no media tiles");
    for (let i = 0; i < n; i++) { await tiles.nth(i).dblclick(); await page.waitForTimeout(300); }
    const rows = await page.locator("#cuesheet [data-cue-pos], #cuesheet tr.cue-row").count();
    if (rows < n) note(ctxName, `added ${n} tiles, sheet shows ${rows} cue rows`);
  });
  await step("select-cue", async () => { await page.locator("#cuesheet [data-cue-pos]").first().click(); });
  await step("inspector", async () => {
    const v = await page.locator("#cueinspector-panel").isVisible();
    if (!v) note(ctxName, "inspector not visible after selecting a cue");
  });
  await step("go", async () => { await page.keyboard.press("Space"); await page.waitForTimeout(1500); });
  await step("pause", async () => { await page.locator("#pause-btn").click(); });
  await step("esc", async () => { await page.keyboard.press("Escape"); await page.waitForTimeout(1200); });
  for (const m of MODALS) {
    await step("modal-" + m, async () => {
      await openModal(page, m);
      await page.waitForSelector(`#${m}.show`, { timeout: 3000 });
      await page.waitForTimeout(500);
      if (m === "settingsModal") {
        for (const tab of await page.locator("#settingsModal [data-bs-toggle='tab'], #settingsModal [data-bs-toggle='pill']").all()) {
          await tab.click(); await page.waitForTimeout(300);
          await page.screenshot({ path: `${out}/settings-${(await tab.innerText()).trim().replace(/\W+/g, "_")}.png` });
        }
      }
      await page.screenshot({ path: `${out}/modal-${m}.png` });
      await page.keyboard.press("Escape");
      await page.waitForTimeout(400);
      if (await page.locator(`#${m}.show`).count()) note(ctxName, "modal did not close on Escape");
    });
  }
  await step("media-menu", async () => { await page.locator(".media-menu-toggle").first().click(); await page.waitForTimeout(300); await page.keyboard.press("Escape"); });
  await step("cue-context-menu", async () => { await page.locator("#cuesheet [data-cue-pos]").first().click({ button: "right" }); await page.waitForTimeout(300); await page.keyboard.press("Escape"); });
  await step("mode-toggle", async () => { await page.locator("#mode-toggle").click(); await page.waitForTimeout(300); await page.locator("#mode-toggle").click(); });
  await step("mediapool-filter", async () => { await page.fill("#mediaFilterText", "zzz-none"); await page.waitForTimeout(300); if (!(await page.locator("#media-no-match").isVisible())) note(ctxName, "no-match message not shown"); await page.fill("#mediaFilterText", ""); });
  // 3. Default theme at the review widths: main page, inspector tabs, every
  // modal (settings: every tab), menus, the upload page and the error page.
  await page.locator("#cuesheet [data-cue-pos]").first().click().catch(() => {});
  await page.close();
  for (const w of [1920, 1280, 1024, 768, 390]) {
    const p = await browser.newPage({ viewport: { width: w, height: { 1920: 1080, 1280: 720, 1024: 768, 768: 1024, 390: 844 }[w] }, hasTouch: w <= 768 });
    const at = (n) => `w${w}/${n}`;
    watch(p, () => ctxName);
    const shot = (n) => p.screenshot({ path: `${out}/w${w}-${n}.png`, fullPage: w <= 900 && /main|page|not-found/.test(n) });
    const stage = async (n, fn) => { ctxName = at(n); try { await fn(); } catch (e) { note(ctxName, "step failed: " + e.message.split("\n")[0]); } };
    await stage("main", async () => {
      await p.goto(base + "/", { waitUntil: "networkidle" });
      await overflow(p, ctxName); await a11y(p, ctxName); await shot("main");
      await focusWalk(p, at("keyboard"), 45);
      await p.locator("body").click({ position: { x: 2, y: 2 } }).catch(() => {});
    });
    for (const tab of await p.locator("#inspector-tabs [data-inspector-tab]").all()) {
      const n = "inspector-" + (await tab.innerText().catch(() => "x")).trim();
      await stage(n, async () => {
        if (!(await tab.isVisible())) return;
        await tab.click(); await p.waitForTimeout(250);
        await a11y(p, ctxName, "#cueinspector-panel"); await shot(n);
      });
    }
    for (const m of MODALS) {
      await stage(m, async () => {
        await openModal(p, m);
        await p.waitForSelector(`#${m}.show`, { timeout: 3000 });
        await p.waitForTimeout(450);
        const tabs = m === "settingsModal" ? await p.locator("#settingsModal .settings-tabs .tab").all() : [null];
        for (const tab of tabs) {
          let n = m;
          if (tab) { await tab.click(); await p.waitForTimeout(250); n = "settings-" + (await tab.innerText()).trim().replace(/\W+/g, "_"); ctxName = at(n); }
          await a11y(p, ctxName, `#${m}`); await shot(n);
        }
        await p.keyboard.press("Escape"); await p.waitForTimeout(400);
      });
    }
    await stage("menu", async () => { await p.locator('#app-menu-btn, [data-bs-toggle="dropdown"]').first().click(); await p.waitForTimeout(250); await a11y(p, ctxName, "#app-menu, .dropdown-menu.show"); await shot("menu"); await p.keyboard.press("Escape"); });
    await stage("cue-menu", async () => { await p.locator("#cuesheet [data-cue-pos]").first().click({ button: "right" }); await p.waitForTimeout(250); await a11y(p, ctxName, ".cue-context-menu"); await shot("cue-menu"); await p.keyboard.press("Escape"); });
    await stage("upload-page", async () => { await p.goto(base + "/upload", { waitUntil: "networkidle" }); await overflow(p, ctxName); await a11y(p, ctxName); await shot("upload-page"); });
    await stage("not-found", async () => {
      ctxName = "expected-404"; // the 404 itself is the point of this page
      await p.goto(base + "/no-such-page", { waitUntil: "networkidle" }); ctxName = at("not-found");
      await p.waitForTimeout(400); // button colour transitions in
      if (!(await p.locator("a[href='/']").count())) note(ctxName, "error page has no way back to the control centre");
      await a11y(p, ctxName); await shot("not-found");
    });
    await p.close();
  }
  await browser.close();
  for (let i = problems.length; i--;) if (problems[i].startsWith("expected-404:")) problems.splice(i, 1);
  fs.writeFileSync(`${out}/problems.txt`, problems.join("\n") + "\n");
  console.log(problems.length + " problem(s)");
  process.exitCode = problems.length ? 1 : 0;
  const seen = {};
  for (const p of problems) { const k = p.replace(/^[^:]+: /, ""); seen[k] = (seen[k] || []).concat(p.split(":")[0]); }
  for (const [k, w] of Object.entries(seen)) console.log(`[${w.length}x e.g. ${w.slice(0, 3).join(", ")}] ${k}`);
})();
