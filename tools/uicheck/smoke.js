// Frontend smoke check: load each page under each theme and fail on any
// console error, uncaught exception or failed same-origin request.
// Dev server only (AGENTS.md): node smoke.js http://192.168.10.162:3001 [theme ...]
// shortcut: loads pages only, no modal clicks; add openers when a template breaks.
const { chromium } = require("playwright");
const base = process.argv[2] || "http://192.168.10.162:3001";
const themes = process.argv.length > 3 ? process.argv.slice(3) : ["", "aqua", "lcars", "msdos"];
const pages = ["/", "/mediapool"];
(async () => {
  const browser = await chromium.launch();
  let bad = 0;
  for (const t of themes) for (const p of pages) {
    const page = await browser.newPage();
    const errs = [];
    page.on("pageerror", (e) => errs.push("exception: " + e.message));
    page.on("console", (m) => m.type() === "error" && errs.push("console: " + m.text()));
    page.on("requestfailed", (r) => errs.push("request failed: " + r.url()));
    await page.goto(base + p + (t ? "?theme=" + t : ""), { waitUntil: "networkidle" });
    for (const e of errs) console.log(`FAIL ${t || "default"} ${p}: ${e}`);
    bad += errs.length;
    await page.close();
  }
  await browser.close();
  console.log(bad ? `${bad} problem(s)` : "ok");
  process.exit(bad ? 1 : 0);
})();
