const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const shared = { id: "relay#shared", provider: "relay", name: "Relay", who: "Shared key", kind: "key", model: "model-one" };
const alternate = { id: "alternate", provider: "alternate", name: "Alternate", kind: "provider", model: "model-two" };
const spare = { id: "spare", provider: "spare", name: "Spare", kind: "provider", model: "model-three" };
const sub = { id: "inner", name: "Inner", ref: "group/inner", routing: "order" };
const member = (source) => `${source.provider}/${source.model}`;
const route = (id, agent, name, order, source, time, nested = false) => ({
  id, seq: id, agent, provider: source.provider, model: `group/${name.toLowerCase()}`, time,
  group: { id: name.toLowerCase(), name, routing: "order", members: order.map(member), ...(nested ? { subs: [sub] } : {}) },
  order, tries: [{ id: source.id, model: source.model, start: time }], done: false,
});

function serve(lang, routes, next) {
  const state = { agents: [{ id: "codex", name: "Codex", fields: [] }, { id: "claude", name: "Claude Code", fields: [] }], profiles: [], settings: { lang, theme: "light" } };
  return async (request) => {
    const url = new URL(request.request().url());
    if (url.hostname !== "magpie.test") return request.abort();
    const json = (data) => request.fulfill({ json: data });
    if (url.pathname === "/boot.js") return request.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:true};` });
    if (url.pathname === "/wails/runtime.js") return request.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json(state);
    if (url.pathname === "/api/gateway/trace") {
      const awaiting = url.searchParams.has("wait");
      if (awaiting && Number(url.searchParams.get("after")) >= 3) return;
      const batch = awaiting ? [await next] : routes;
      return json({ mine: true, now: new Date().toISOString(), seq: awaiting ? 3 : 2, totals: { requests: 2 }, routes: batch });
    }
    if (url.pathname === "/api/gateway/history") return json({ cut: false, days: [], routes: [] });
    if (url.pathname === "/api/groups") return json({ groups: [], pools: [] });
    if (url.pathname === "/api/providers") return json({ providers: [], gateway: { running: true, window: true } });
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    return request.fulfill({ contentType, body: await fs.readFile(file) });
  };
}

function seats() {
  let group = "";
  return [...document.querySelectorAll(".rt-accts > li")].flatMap((row) => {
    if (row.classList.contains("rt-route")) group = row.querySelector(".mdl").textContent;
    if (row.title.split("\n")[0] !== "relay#shared") return [];
    return [{ group, depth: row.style.getPropertyValue("--depth"), answering: row.classList.contains("now") }];
  });
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
    test(`${engine} ${lang}: the live packet follows the group that used the shared key`, async (t) => {
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
      t.after(() => browser.close());
      const page = await browser.newPage({ viewport: { width: 1100, height: 900 }, reducedMotion: "reduce" });
      page.setDefaultTimeout(8000);
      page.setDefaultNavigationTimeout(60000);
      const errors = [];
      page.on("pageerror", (error) => errors.push(error.message));
      const time = new Date(Date.now() - 3000).toISOString();
      const alpha = route(1, "codex", "Alpha", [alternate, shared], alternate, time);
      const betaKey = { ...shared, via: [sub.id] };
      const beta = route(2, "claude", "Beta", [betaKey, spare], betaKey, time, true);
      let send;
      const change = new Promise((resolve) => { send = resolve; });
      await page.route("**/*", serve(lang, [alpha, beta], change));
      await page.goto("http://magpie.test/?view=routing");
      await page.locator(".rt-accts > li.rt-route").nth(1).waitFor();
      await page.waitForFunction(() => document.querySelectorAll(".rt-sky .pkt.held").length === 2);
      const expected = [{ group: "group/alpha", depth: "1", answering: false }, { group: "group/beta", depth: "2", answering: true }];
      assert.deepEqual(await page.evaluate(seats), expected, "the shared key must light in Beta while Alpha uses another source");
      assert.equal(await page.locator('.rt-accts > li[title="relay#shared"].now').evaluate((row) => {
        const box = row.getBoundingClientRect();
        return [...document.querySelectorAll(".rt-sky .pkt.held")].some((dot) => {
          const bounds = dot.getBoundingClientRect();
          const x = bounds.left + bounds.width / 2, y = bounds.top + bounds.height / 2;
          return x <= box.left && x >= box.left - 10 && y >= box.top && y <= box.bottom;
        });
      }), true, "a live packet must be held beside Beta's own shared-key row");

      send({ ...beta, seq: 3, order: [betaKey, alternate], group: { ...beta.group, members: [member(betaKey), member(alternate)] } });
      await page.locator('.rt-accts > li[title="spare"]').waitFor({ state: "detached" });
      assert.deepEqual(await page.evaluate(seats), expected, "groups with identical seat sets must keep their own rows");
      assert.deepEqual(errors, []);
    });
  }
}
