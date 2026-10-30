const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test, after } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = process.env.MAGPIE_QUOTA_ASSETS || path.resolve(__dirname, "../assets/quota");
const browsers = new Map();
async function browserFor(engine) {
  if (!browsers.has(engine)) browsers.set(engine, engine === "webkit" ? webkit.launch() : chromium.launch({ channel: process.env.PLAYWRIGHT_CHANNEL || "chromium" }));
  return browsers.get(engine);
}
after(async () => { await Promise.all([...browsers.values()].map(async (browser) => (await browser).close())); });
const labels = {
  zh: ["额度概览", "额度管理", "请求日志"],
  en: ["Overview", "Token packages", "Request logs"],
  "zh-TW": ["額度總覽", "額度管理", "請求記錄"],
  ja: ["使用状況", "トークンパッケージ", "リクエストログ"],
  de: ["Übersicht", "Token-Pakete", "Anfrageprotokoll"],
};

async function fixture(page, options = {}) {
  const state = {
    packs: [{ id: "one", name: "Research", models: ["openai/research"], totalTokens: 1000000,
      usedTokens: 250000, reservedTokens: 1200, remainingTokens: 748800, enabled: true,
      createdAt: "2026-10-30T02:00:00Z", shareURL: "http://magpie.test/share/fixture-token" }],
    events: [], logsQueries: [], failed: false,
  };
  await page.addInitScript(() => {
    window.quotaCopies = [];
    Object.defineProperty(navigator, "clipboard", { value: { writeText: async (text) => window.quotaCopies.push(text) } });
  });
  await page.route("**/*", async (route) => {
    const req = route.request(), url = new URL(req.url()), p = url.pathname;
    if (p.startsWith("/api/token-packs")) {
      if (state.failed) return route.fulfill({ status: 503, json: { error: "Storage unavailable" } });
      if (p === "/api/token-packs/models") return route.fulfill({ json: { models: [{ id: "openai/research", name: "Research model" }] } });
      if (p === "/api/token-packs/logs") {
        state.logsQueries.push(url.searchParams.get("pack") || "");
        return route.fulfill({ json: { logs: [{ id: "log-1", packId: "one", packName: "Research", model: "openai/research",
          tokens: options.outcome ? 0 : 250, status: options.statusCode ? String(options.statusCode) : options.outcome || "completed", outcome: options.outcome,
          message: "private upstream body", createdAt: "2026-10-30T03:00:00Z", durationMs: 730 }] } });
      }
      if (p === "/api/token-packs" && req.method() === "GET") return route.fulfill({ json: { packs: state.packs, baseURL: "http://magpie.test/v1" } });
      const body = req.postDataJSON();
      state.events.push({ method: req.method(), path: p, body });
      if (p.endsWith("/settle")) return options.settleBusy
        ? route.fulfill({ status: 409, json: { error: "Request is still active" } })
        : route.fulfill({ json: { log: { id: "log-1", tokens: body.actualTokens, status: "completed" } } });
      if (req.method() === "POST") {
        const pack = { ...body, id: "two", usedTokens: 0, remainingTokens: body.totalTokens, enabled: true,
          createdAt: "2026-10-30T04:00:00Z", shareURL: "http://magpie.test/share/second-token" };
        state.packs.push(pack);
        return route.fulfill({ json: { pack } });
      }
      const pack = state.packs.find((x) => x.id === p.split("/").at(-1));
      Object.assign(pack, body);
      pack.remainingTokens = Math.max(0, pack.totalTokens - pack.usedTokens - (pack.reservedTokens || 0));
      return route.fulfill({ json: { pack } });
    }
    if (p === "/" || p.startsWith("/quota/")) {
      const name = p === "/" ? "index.html" : p.slice("/quota/".length);
      try {
        const body = await fs.readFile(path.join(assets, name));
        return route.fulfill({ body, contentType: name.endsWith(".js") ? "text/javascript" : name.endsWith(".css") ? "text/css" : "text/html" });
      } catch {
        // An old tree without this console serves no console; the behavior
        // assertion below, rather than a missing fixture file, goes red.
        return route.fulfill({ body: "<!doctype html><html><body></body></html>", contentType: "text/html" });
      }
    }
    return route.abort();
  });
  return state;
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const locale of ["zh", "en", "zh-TW", "ja", "de"]) {
    test(`${engine} ${locale}: quota console has exactly three views and handles narrow layouts`, async (t) => {
      const browser = await browserFor(engine);
      const context = await browser.newContext({ locale, viewport: { width: 1280, height: 850 }, reducedMotion: "reduce" });
      t.after(() => context.close());
      const page = await context.newPage();
      page.setDefaultTimeout(15000);
      page.setDefaultNavigationTimeout(30000);
      const errors = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await fixture(page);
      await page.goto("http://magpie.test/");
      await page.locator('[data-stat="totalTokens"]').filter({ hasText: "1" }).waitFor();
      assert.deepEqual(await page.locator("nav button [data-i18n]").allTextContents(), labels[locale]);
      assert.equal(await page.locator('[data-stat="remainingTokens"]').getAttribute("data-value"), "748800");
      assert.equal(await page.locator("[data-pack-id=one]").count(), 2, "overview and management share the same figures");
      await page.getByRole("button", { name: labels[locale][1], exact: true }).click();
      assert.equal(await page.locator("#view-packages").isVisible(), true);
      assert.equal(await page.locator("select").count(), 0, "filters use custom menus");
      for (const width of [390, 768]) {
        await page.setViewportSize({ width, height: 844 });
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
      }
      if (process.env.ARTIFACT_DIR && locale === "zh") {
        await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
        await page.setViewportSize({ width: 1440, height: 1000 });
        await page.getByRole("button", { name: labels[locale][0], exact: true }).click();
        await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `${engine}-quota-console.png`), fullPage: true });
      }
      assert.deepEqual(errors, []);
    });
  }

  test(`${engine}: create a model package, edit its quota, share, disable and inspect logs`, async (t) => {
    const browser = await browserFor(engine);
    const context = await browser.newContext({ locale: "zh", viewport: { width: 1280, height: 850 } });
    t.after(() => context.close());
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    page.setDefaultNavigationTimeout(30000);
    const state = await fixture(page);
    await page.goto("http://magpie.test/");
    await page.locator('[data-stat="totalTokens"]').filter({ hasText: "1" }).waitFor();
    await page.getByRole("button", { name: "创建套餐", exact: true }).first().click();
    await page.getByLabel("套餐名称", { exact: true }).fill('<img src=x onerror="alert(1)">');
    await page.getByLabel("Token 总额度", { exact: true }).fill("1mish");
    await page.getByRole("checkbox", { name: "Research model" }).check();
    await page.locator("#pack-form").getByRole("button", { name: "创建套餐", exact: true }).click();
    assert.equal(state.events.length, 0, "invalid quota never reaches the server");
    await page.getByLabel("Token 总额度", { exact: true }).fill("100万");
    await page.locator("#pack-form").getByRole("button", { name: "创建套餐", exact: true }).click();
    await page.locator("#pack-dialog").waitFor({ state: "hidden" });
    const row = page.locator("#package-list [data-pack-id=two]");
    await row.waitFor();
    assert.equal(state.events[0].body.totalTokens, 1000000);
    assert.deepEqual(state.events[0].body.models, ["openai/research"]);
    assert.equal(await row.locator("img").count(), 0, "untrusted names stay literal text");
    await row.getByRole("button", { name: "复制分享链接", exact: true }).click();
    await page.waitForFunction(() => window.quotaCopies.length === 1);
    assert.deepEqual(await page.evaluate(() => window.quotaCopies), ["http://magpie.test/share/second-token"]);
    await row.getByRole("button", { name: "调整额度", exact: true }).click();
    await page.getByLabel("Token 总额度", { exact: true }).fill("2M");
    await page.getByRole("button", { name: "保存额度", exact: true }).click();
    await page.locator("#pack-dialog").waitFor({ state: "hidden" });
    assert.deepEqual(state.events.at(-1).body, { totalTokens: 2000000 }, "quota edits must not overwrite a concurrently changed enabled state");
    await row.getByRole("button", { name: "停用套餐", exact: true }).click();
    await row.getByRole("button", { name: "启用套餐", exact: true }).waitFor();
    assert.deepEqual(state.events.at(-1).body, { enabled: false }, "toggling must not overwrite a concurrently changed quota");
    await page.getByRole("button", { name: "请求日志", exact: true }).click();
    await page.getByRole("button", { name: "筛选套餐", exact: true }).click();
    await page.getByRole("menuitemradio", { name: "Research", exact: true }).click();
    await page.waitForFunction(() => document.querySelector("#log-list")?.textContent.includes("250"));
    assert.equal(state.logsQueries.at(-1), "one");
    assert.match(await page.locator("#log-list").textContent(), /openai\/research/);
  });

  test(`${engine}: failed refresh preserves last-known balances and says they could not refresh`, async (t) => {
    const browser = await browserFor(engine);
    const context = await browser.newContext({ locale: "en" });
    t.after(() => context.close());
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    page.setDefaultNavigationTimeout(30000);
    const state = await fixture(page);
    await page.goto("http://magpie.test/");
    await page.locator('[data-stat="totalTokens"]').filter({ hasText: "1" }).waitFor();
    state.failed = true;
    await page.getByRole("button", { name: "Refresh", exact: true }).click();
    await page.getByRole("alert").filter({ hasText: "Storage unavailable" }).waitFor();
    assert.equal(await page.locator('[data-stat="remainingTokens"]').getAttribute("data-value"), "748800");
  });

  for (const outcome of ["uncertain", "pending"]) test(`${engine}: ${outcome} logs require an explicit actual usage settlement`, async (t) => {
    const browser = await browserFor(engine);
    const context = await browser.newContext({ locale: "zh" });
    t.after(() => context.close());
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    page.setDefaultNavigationTimeout(30000);
    const state = await fixture(page, { outcome });
    await page.goto("http://magpie.test/");
    await page.locator('[data-stat="totalTokens"]').filter({ hasText: "1" }).waitFor();
    assert.match(await page.locator("#overview-list").textContent(), /1,200 Token 处理中 \/ 待核销/);
    await page.getByRole("button", { name: "请求日志", exact: true }).click();
    await page.getByRole("button", { name: "核销用量", exact: true }).click();
    await page.getByLabel("实际 Token 用量", { exact: true }).fill("0");
    await page.locator("#pack-form").getByRole("button", { name: "核销用量", exact: true }).click();
    await page.locator("#pack-dialog").waitFor({ state: "hidden" });
    assert.deepEqual(state.events.at(-1), { method: "POST", path: "/api/token-packs/logs/log-1/settle", body: { actualTokens: 0 } });
  });

  test(`${engine}: an executing request refuses manual settlement and keeps its reservation`, async (t) => {
    const browser = await browserFor(engine);
    const context = await browser.newContext({ locale: "zh" });
    t.after(() => context.close());
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    page.setDefaultNavigationTimeout(30000);
    await fixture(page, { outcome: "pending", settleBusy: true });
    await page.goto("http://magpie.test/");
    await page.locator('[data-stat="totalTokens"]').filter({ hasText: "1" }).waitFor();
    await page.getByRole("button", { name: "请求日志", exact: true }).click();
    await page.getByRole("button", { name: "核销用量", exact: true }).click();
    await page.getByLabel("实际 Token 用量", { exact: true }).fill("0");
    await page.locator("#pack-form").getByRole("button", { name: "核销用量", exact: true }).click();
    await page.locator("#form-error").filter({ hasText: "Request is still active" }).waitFor();
    assert.equal(await page.locator("#pack-dialog").isVisible(), true);
    assert.equal(await page.locator('[data-stat="remainingTokens"]').getAttribute("data-value"), "748800");
  });

  test(`${engine}: failed logs show the HTTP status without upstream response bodies`, async (t) => {
    const browser = await browserFor(engine);
    const context = await browser.newContext({ locale: "zh" });
    t.after(() => context.close());
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    page.setDefaultNavigationTimeout(30000);
    await fixture(page, { outcome: "failed", statusCode: 502 });
    await page.goto("http://magpie.test/");
    await page.locator('[data-stat="totalTokens"]').filter({ hasText: "1" }).waitFor();
    await page.getByRole("button", { name: "请求日志", exact: true }).click();
    await page.locator("#log-list").getByText("HTTP 502", { exact: true }).waitFor();
    assert.doesNotMatch(await page.locator("#log-list").textContent(), /private upstream body/);
  });
}
