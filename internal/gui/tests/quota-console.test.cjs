const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const vm = require("node:vm");
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
  zh: ["额度概览", "额度管理", "请求日志", "供应商接入"],
  en: ["Overview", "Token packages", "Request logs", "Providers"],
  "zh-TW": ["額度總覽", "額度管理", "請求記錄", "供應商接入"],
  ja: ["使用状況", "トークンパッケージ", "リクエストログ", "プロバイダー"],
  de: ["Übersicht", "Token-Pakete", "Anfrageprotokoll", "Anbieter"],
};

test("quota console messages include all five languages and resolve every literal key", async () => {
  const source = await fs.readFile(path.join(assets, "i18n.js"), "utf8");
  const words = vm.runInNewContext(`${source}; quotaWords`, { navigator: { language: "en" } });
  for (const [key, translations] of Object.entries(words)) {
    assert.equal(translations.length, 5, key);
    assert.ok(translations.every((text) => typeof text === "string" && text.length > 0), key);
  }
  for (const filename of ["app.js", "providers.js", "searches.js", "index.html"]) {
    const script = await fs.readFile(path.join(assets, filename), "utf8");
    for (const match of script.matchAll(/quotaT\("([\w]+)"|data-i18n(?:-label)?="([\w]+)"/g)) {
      assert.ok(words[match[1] || match[2]], `${filename}: ${match[1] || match[2]}`);
    }
  }
});

async function fixture(page, options = {}) {
  const state = {
    packs: [{ id: "one", name: "Research", models: ["openai/research"], totalTokens: 1000000,
      usedTokens: 250000, reservedTokens: 1200, remainingTokens: 748800, enabled: true,
      mcpTotalCalls: 100, mcpUsedCalls: 7, mcpRemainingCalls: 93,
      createdAt: "2026-10-30T02:00:00Z", shareURL: "http://magpie.test/share/fixture-token" }],
    providers: options.emptyProviders ? [] : [{ id: "openai", name: "Research provider", protocol: "chat", baseURL: "https://research.example/v1", models: ["research"], enabled: true, keyConfigured: true, keyMasked: "••••cret" }],
    searches: options.emptySearches ? [] : [{ vendor: "tavily", name: "Tavily", baseURL: "https://api.tavily.com", enabled: true, keyConfigured: true, keyMasked: "••••cret", editable: !options.searchReadOnly, unsupportedReason: options.searchReadOnly ? "Multiple keys are configured." : undefined }],
    events: [], providerEvents: [], searchEvents: [], logsQueries: [], failed: false, providerFailed: false, searchFailed: false, searchWait: null, probeWait: null,
  };
  if (options.emptyPacks) state.packs = [];
  if (options.providerReadOnly) Object.assign(state.providers[0], { editable: false, unsupportedReason: "Multiple protocol endpoints are configured." });
  await page.addInitScript(() => {
    window.quotaCopies = [];
    Object.defineProperty(navigator, "clipboard", { value: { writeText: async (text) => window.quotaCopies.push(text) } });
  });
  await page.route("**/*", async (route) => {
    const req = route.request(), url = new URL(req.url()), p = url.pathname;
    if (p.startsWith("/api/quota-searches")) {
      if (state.searchFailed) return route.fulfill({ status: 503, json: { error: "Search service unavailable" } });
      const vendors = ["tavily", "brave", "exa", "firecrawl", "you", "deepseek", "searxng"].map((id) => ({ id, name: id === "tavily" ? "Tavily" : id, baseURL: id === "searxng" ? "" : `https://api.${id}.com`, keyOptional: id === "searxng" }));
      if (req.method() === "GET") return route.fulfill({ json: { searches: state.searches, vendors } });
      const body = req.postDataJSON();
      state.searchEvents.push({ method: req.method(), path: p, body });
      if (state.searchWait) await state.searchWait;
      let search = state.searches.find((item) => item.vendor === (body.vendor || p.split("/").at(-1)));
      if (!search) {
        search = { vendor: body.vendor, name: vendors.find((item) => item.id === body.vendor).name, enabled: true, editable: true };
        state.searches.push(search);
      }
      for (const field of ["baseURL", "enabled"]) if (field in body) search[field] = body[field];
      if (body.apiKey) Object.assign(search, { keyConfigured: true, keyMasked: "••••cret" });
      return route.fulfill({ json: { search } });
    }
    if (p.startsWith("/api/quota-providers")) {
      if (state.providerFailed) return route.fulfill({ status: 503, json: { error: "Provider unavailable" } });
      if (p === "/api/quota-providers" && req.method() === "GET") return route.fulfill({ json: { providers: state.providers } });
      const body = req.postDataJSON();
      state.providerEvents.push({ method: req.method(), path: p, body });
      if (p.endsWith("/models")) return route.fulfill({ json: { models: ["gpt-test", "o1-test"] } });
      if (p.endsWith("/probe")) {
        if (state.probeWait) await state.probeWait;
        return route.fulfill({ json: { ok: true, statusCode: 200, model: "gpt-test" } });
      }
      if (req.method() === "POST") {
        const provider = { id: "studio", name: body.name, protocol: body.protocol, baseURL: body.baseURL,
          models: body.models, enabled: true, keyConfigured: true, keyMasked: "••••cret" };
        state.providers.push(provider);
        return route.fulfill({ json: { provider } });
      }
      const provider = state.providers.find((item) => item.id === p.split("/").at(-1));
      for (const field of ["name", "protocol", "baseURL", "models", "enabled"]) if (field in body) provider[field] = body[field];
      return route.fulfill({ json: { provider } });
    }
    if (p.startsWith("/api/token-packs")) {
      if (state.failed) return route.fulfill({ status: 503, json: { error: "Storage unavailable" } });
      if (p === "/api/token-packs/models") return route.fulfill({ json: { models: state.providers.filter((item) => item.enabled).flatMap((provider) => provider.models.map((model) => ({ id: `${provider.id}/${model}`, name: provider.id === "openai" ? "Research model" : model }))) } });
      if (p === "/api/token-packs/logs") {
        state.logsQueries.push(url.searchParams.get("pack") || "");
        return route.fulfill({ json: { logs: [{ id: "log-1", packId: "one", packName: "Research", model: "openai/research",
          tokens: options.outcome ? 0 : 250, status: options.statusCode ? String(options.statusCode) : options.outcome || "completed", outcome: options.outcome,
          source: options.mcpLog ? "mcp" : "api", tool: options.mcpLog ? "generate_text" : undefined,
          ...(options.callLog ? { unit: "calls", calls: 1, tool: "web_search", model: "", source: "mcp" } : {}),
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
      pack.mcpRemainingCalls = Math.max(0, (pack.mcpTotalCalls || 0) - (pack.mcpUsedCalls || 0));
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
    test(`${engine} ${locale}: quota console has four focused views and handles narrow layouts`, async (t) => {
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
      await page.getByRole("button", { name: labels[locale][3], exact: true }).click();
      await page.locator("#provider-list [data-provider-id=openai]").waitFor();
      for (const width of [390, 768]) {
        await page.setViewportSize({ width, height: 844 });
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
      }
      if (process.env.ARTIFACT_DIR && locale === "zh") {
        await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
        await page.setViewportSize({ width: 1440, height: 1000 });
        await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `${engine}-quota-providers.png`), fullPage: true });
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

  test(`${engine}: package MCP requests are labeled in the same request log`, async (t) => {
    const browser = await browserFor(engine), page = await browser.newPage();
    t.after(() => page.close());
    await fixture(page, { mcpLog: true });
    await page.goto("http://magpie.test/");
    await page.locator('nav [data-view="logs"]').click();
    await page.locator("#log-list .mcp-source").waitFor();
    assert.equal(await page.locator("#log-list .mcp-source").textContent(), "MCP");
    assert.match(await page.locator("#log-list").textContent(), /openai\/research/);
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

  test(`${engine}: onboard and edit providers without exposing or forwarding a stored key to another address`, async (t) => {
    const browser = await browserFor(engine);
    const context = await browser.newContext({ locale: "zh", viewport: { width: 1280, height: 900 } });
    t.after(() => context.close());
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    page.setDefaultNavigationTimeout(30000);
    const state = await fixture(page);
    await page.goto("http://magpie.test/");
    await page.locator('[data-stat="totalTokens"]').filter({ hasText: "1" }).waitFor();
    // Warm the package model cache before adding a provider.
    await page.getByRole("button", { name: "创建套餐", exact: true }).click();
    await page.getByRole("checkbox", { name: "Research model" }).waitFor();
    await page.locator("#cancel-dialog").click();
    await page.getByRole("button", { name: "供应商接入", exact: true }).click();
    await page.getByRole("button", { name: "接入供应商", exact: true }).first().click();
    await page.getByLabel("供应商名称", { exact: true }).fill('Studio <img src=x>');
    await page.getByRole("button", { name: "API 协议", exact: true }).click();
    await page.getByRole("menuitemradio", { name: "OpenAI Responses", exact: true }).click();
    await page.getByLabel("请求地址", { exact: true }).fill("https://relay.example/v1");
    const key = page.getByLabel("API Key", { exact: true });
    assert.equal(await key.getAttribute("type"), "password");
    await key.fill("fixture-provider-secret");
    await page.getByRole("button", { name: "获取模型", exact: true }).click();
    await page.waitForFunction(() => document.querySelector("#provider-models")?.value.includes("o1-test"));
    let finishProbe;
    state.probeWait = new Promise((resolve) => { finishProbe = resolve; });
    const probe = page.getByRole("button", { name: "测试连接", exact: true });
    await probe.click();
    assert.equal(await probe.isDisabled(), true, "a pending probe cannot be submitted twice");
    finishProbe();
    await page.locator("#provider-feedback").getByText(/连接正常/).waitFor();
    await page.getByRole("button", { name: "保存供应商", exact: true }).click();
    await page.locator("#provider-dialog").waitFor({ state: "hidden" });
    const row = page.locator("[data-provider-id=studio]");
    await row.waitFor();
    assert.equal(await row.locator("img").count(), 0);
    assert.doesNotMatch(await page.locator("body").textContent(), /fixture-provider-secret/);
    assert.equal(await key.inputValue(), "", "secret input is wiped after saving");
    assert.equal(state.providerEvents.find((event) => event.path === "/api/quota-providers").body.protocol, "responses");
    await page.getByRole("button", { name: "额度管理", exact: true }).click();
    await page.getByRole("button", { name: "创建套餐", exact: true }).click();
    await page.getByRole("checkbox", { name: "gpt-test", exact: true }).waitFor();
    await page.locator("#cancel-dialog").click();
    await page.getByRole("button", { name: "供应商接入", exact: true }).click();
    await row.getByRole("button", { name: "编辑供应商", exact: true }).click();
    assert.equal(await key.inputValue(), "");
    await page.getByLabel("供应商名称", { exact: true }).fill("Studio");
    await page.getByRole("button", { name: "保存供应商", exact: true }).click();
    await page.locator("#provider-dialog").waitFor({ state: "hidden" });
    let change = state.providerEvents.at(-1);
    assert.equal(change.body.apiKey, undefined, "unchanged keys never travel back through the browser");
    assert.equal(change.body.enabled, undefined, "metadata edits leave concurrent enabled-state changes alone");
    await row.getByRole("button", { name: "编辑供应商", exact: true }).click();
    await page.getByLabel("请求地址", { exact: true }).fill("https://another.example/v1");
    const count = state.providerEvents.length;
    await page.getByRole("button", { name: "保存供应商", exact: true }).click();
    await page.locator("#provider-form-error").getByText(/重新填写 API Key/).waitFor();
    assert.equal(state.providerEvents.length, count, "changing the target never forwards a stored credential");
    await key.fill("replacement-key");
    await page.getByRole("button", { name: "保存供应商", exact: true }).click();
    await page.locator("#provider-dialog").waitFor({ state: "hidden" });
    assert.equal(state.providerEvents.at(-1).body.apiKey, "replacement-key");
    await row.getByRole("button", { name: "停用供应商", exact: true }).click();
    await row.getByRole("button", { name: "启用供应商", exact: true }).waitFor();
    assert.deepEqual(state.providerEvents.at(-1).body, { enabled: false });
    state.providerFailed = true;
    await page.getByRole("button", { name: "刷新", exact: true }).click();
    await page.locator("#global-error").getByText(/Provider unavailable/).waitFor();
    assert.equal(await row.isVisible(), true, "failed reads preserve existing provider rows");
  });

  test(`${engine}: empty packages and model pickers lead to provider onboarding in the panel`, async (t) => {
    const browser = await browserFor(engine);
    const context = await browser.newContext({ locale: "zh", viewport: { width: 390, height: 844 } });
    t.after(() => context.close());
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    await fixture(page, { emptyPacks: true, emptyProviders: true });
    await page.goto("http://magpie.test/");
    await page.locator("#overview-list").getByRole("button", { name: "接入供应商", exact: true }).click();
    await page.locator("#view-providers").waitFor({ state: "visible" });
    await page.getByRole("button", { name: "额度管理", exact: true }).click();
    await page.getByRole("button", { name: "创建套餐", exact: true }).click();
    await page.getByRole("button", { name: "先接入供应商", exact: true }).click();
    await page.locator("#pack-dialog").waitFor({ state: "hidden" });
    await page.locator("#view-providers").waitFor({ state: "visible" });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  });

  test(`${engine}: provider probe failures keep the form editable and never re-display a stored key`, async (t) => {
    const browser = await browserFor(engine);
    const context = await browser.newContext({ locale: "zh" });
    t.after(() => context.close());
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    const state = await fixture(page);
    await page.goto("http://magpie.test/");
    await page.getByRole("button", { name: "供应商接入", exact: true }).click();
    await page.locator("[data-provider-id=openai]").getByRole("button", { name: "编辑供应商", exact: true }).click();
    state.providerFailed = true;
    await page.getByRole("button", { name: "测试连接", exact: true }).click();
    await page.locator("#provider-form-error").getByText("Provider unavailable", { exact: true }).waitFor();
    assert.equal(await page.getByRole("button", { name: "测试连接", exact: true }).isEnabled(), true);
    assert.equal(await page.getByLabel("供应商名称", { exact: true }).inputValue(), "Research provider");
    assert.equal(await page.getByLabel("API Key", { exact: true }).inputValue(), "");
    state.providerFailed = false;
    await page.getByRole("button", { name: "测试连接", exact: true }).click();
    await page.locator("#provider-feedback").getByText(/连接正常/).waitFor();
    assert.equal(state.providerEvents.at(-1).body.apiKey, undefined);
    assert.equal(state.providerEvents.at(-1).body.id, "openai", "only the saved provider ID requests reuse at the unchanged address");
    await page.getByLabel("API Key", { exact: true }).fill("discard-this-key");
    await page.keyboard.press("Escape");
    await page.locator("#provider-dialog").waitFor({ state: "hidden" });
    assert.equal(await page.getByLabel("API Key", { exact: true }).inputValue(), "");
  });

  test(`${engine}: independent search call quota edits only the changed total`, async (t) => {
    const context = await (await browserFor(engine)).newContext({ locale: "zh" });
    t.after(() => context.close());
    const page = await context.newPage();
    const state = await fixture(page);
    await page.goto("http://magpie.test/");
    await page.locator("#overview-list").getByText(/剩余 93 次/).waitFor();
    await page.getByRole("button", { name: "额度管理", exact: true }).click();
    await page.locator("#package-list").getByRole("button", { name: "调整额度", exact: true }).click();
    const input = page.getByLabel("搜索与网页读取次数", { exact: true });
    assert.equal(await input.inputValue(), "100");
    await input.fill("3.5");
    await page.getByRole("button", { name: "保存额度", exact: true }).click();
    assert.equal(state.events.length, 0);
    await input.fill("200");
    await page.getByRole("button", { name: "保存额度", exact: true }).click();
    await page.locator("#pack-dialog").waitFor({ state: "hidden" });
    assert.deepEqual(state.events.at(-1).body, { mcpTotalCalls: 200 });
    await page.locator("#package-list").getByText(/剩余 193 次/).waitFor();
  });

  test(`${engine}: create includes a separate search budget and accepts zero calls`, async (t) => {
    const context = await (await browserFor(engine)).newContext({ locale: "zh" });
    t.after(() => context.close());
    const page = await context.newPage();
    const state = await fixture(page);
    await page.goto("http://magpie.test/");
    await page.locator(".create-pack").first().click();
    await page.getByLabel("套餐名称", { exact: true }).fill("Model only");
    await page.getByRole("checkbox", { name: "Research model" }).check();
    assert.equal(await page.getByLabel("搜索与网页读取次数", { exact: true }).inputValue(), "0");
    await page.locator("#submit-pack").click();
    await page.locator("#pack-dialog").waitFor({ state: "hidden" });
    assert.equal(state.events.at(-1).body.mcpTotalCalls, 0);
  });

  test(`${engine}: search logs show calls and cannot be settled as tokens`, async (t) => {
    const context = await (await browserFor(engine)).newContext({ locale: "zh" });
    t.after(() => context.close());
    const page = await context.newPage();
    await fixture(page, { callLog: true, outcome: "pending" });
    await page.goto("http://magpie.test/");
    await page.getByRole("button", { name: "请求日志", exact: true }).click();
    await page.locator("#log-list td").filter({ hasText: "web_search" }).waitFor();
    assert.match(await page.locator("#log-list").textContent(), /1 次/);
    assert.equal(await page.locator("#log-list .settle-button").count(), 0);
    assert.equal(await page.locator("#log-list .mcp-source").count(), 1);
  });

  test(`${engine}: search service CRUD keeps saved keys private and rejects reuse at a changed URL`, async (t) => {
    const context = await (await browserFor(engine)).newContext({ locale: "zh" });
    t.after(() => context.close());
    const page = await context.newPage();
    const state = await fixture(page, { emptySearches: true });
    await page.goto("http://magpie.test/");
    await page.getByRole("button", { name: "供应商接入", exact: true }).click();
    await page.getByRole("button", { name: "接入搜索服务", exact: true }).click();
    await page.getByRole("button", { name: "搜索服务类型", exact: true }).click();
    assert.equal(await page.locator("#search-vendor-menu [role=menuitemradio]").count(), 7);
    await page.getByRole("menuitemradio", { name: "Tavily", exact: true }).click();
    assert.equal(await page.getByLabel("搜索服务地址", { exact: true }).inputValue(), "https://api.tavily.com");
    await page.locator("#search-key").fill("new-search-secret");
    await page.getByRole("button", { name: "保存搜索服务", exact: true }).click();
    await page.locator("#search-dialog").waitFor({ state: "hidden" });
    const row = page.locator("[data-search-vendor=tavily]");
    await row.getByRole("button", { name: "编辑搜索服务", exact: true }).click();
    assert.equal(await page.locator("#search-key").inputValue(), "");
    assert.doesNotMatch(await page.locator("#view-providers").textContent(), /new-search-secret/);
    await page.getByLabel("搜索服务地址", { exact: true }).fill("https://other.example/search");
    await page.getByRole("button", { name: "保存搜索服务", exact: true }).click();
    assert.equal(state.searchEvents.length, 1, "saved keys cannot move to a new host");
    await page.locator("#search-key").fill("replacement-secret");
    await page.getByRole("button", { name: "保存搜索服务", exact: true }).click();
    await page.locator("#search-dialog").waitFor({ state: "hidden" });
    assert.deepEqual(state.searchEvents.at(-1).body, { baseURL: "https://other.example/search", apiKey: "replacement-secret" });
    const savedEvents = state.searchEvents.length;
    await row.getByRole("button", { name: "编辑搜索服务", exact: true }).click();
    assert.equal(await page.locator("#search-key").inputValue(), "");
    await page.getByLabel("搜索服务地址", { exact: true }).fill("https://other.example/search/");
    await page.getByRole("button", { name: "保存搜索服务", exact: true }).click();
    await page.locator("#search-dialog").waitFor({ state: "hidden" });
    assert.equal(state.searchEvents.length, savedEvents, "the unchanged normalized address keeps its key without resubmitting it");
    await row.getByRole("button", { name: "停用搜索服务", exact: true }).click();
    await row.getByRole("button", { name: "启用搜索服务", exact: true }).waitFor();
    assert.deepEqual(state.searchEvents.at(-1).body, { enabled: false });
    assert.equal(await page.locator("nav [data-view]").count(), 4);
  });

  test(`${engine}: search service retry, busy state and no-key local SearXNG`, async (t) => {
    const context = await (await browserFor(engine)).newContext({ locale: "zh", viewport: { width: 390, height: 844 } });
    t.after(() => context.close());
    const page = await context.newPage();
    const state = await fixture(page, { emptySearches: true });
    await page.goto("http://magpie.test/");
    await page.getByRole("button", { name: "供应商接入", exact: true }).click();
    await page.getByRole("button", { name: "接入搜索服务", exact: true }).click();
    await page.getByRole("button", { name: "搜索服务类型", exact: true }).click();
    await page.getByRole("menuitemradio", { name: "searxng", exact: true }).click();
    assert.equal(await page.getByLabel("搜索服务地址", { exact: true }).inputValue(), "");
    await page.getByLabel("搜索服务地址", { exact: true }).fill("http://search.local:8080");
    state.searchFailed = true;
    await page.getByRole("button", { name: "保存搜索服务", exact: true }).click();
    await page.locator("#search-form-error").getByText("Search service unavailable").waitFor();
    state.searchFailed = false;
    let release;
    state.searchWait = new Promise((resolve) => { release = resolve; });
    await page.getByRole("button", { name: "保存搜索服务", exact: true }).click();
    assert.equal(await page.locator("#save-search").isDisabled(), true);
    await page.keyboard.press("Escape");
    assert.equal(await page.locator("#search-dialog").isVisible(), true);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
    release();
    await page.locator("#search-dialog").waitFor({ state: "hidden" });
    assert.equal(state.searchEvents.at(-1).body.apiKey, undefined);
  });

  test(`${engine}: complex search services are readonly`, async (t) => {
    const context = await (await browserFor(engine)).newContext({ locale: "zh" });
    t.after(() => context.close());
    const page = await context.newPage();
    const state = await fixture(page, { searchReadOnly: true });
    await page.goto("http://magpie.test/");
    await page.getByRole("button", { name: "供应商接入", exact: true }).click();
    const row = page.locator("[data-search-vendor=tavily]");
    await row.waitFor();
    assert.equal(await row.getByRole("button", { name: "编辑搜索服务", exact: true }).isDisabled(), true);
    assert.equal(await row.getByRole("button", { name: "停用搜索服务", exact: true }).isDisabled(), true);
    assert.match(await row.textContent(), /Multiple keys/);
    assert.equal(state.searchEvents.length, 0);
  });

  test(`${engine}: complex providers remain visible and cannot be overwritten from the small panel`, async (t) => {
    const browser = await browserFor(engine);
    const context = await browser.newContext({ locale: "zh" });
    t.after(() => context.close());
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    const state = await fixture(page, { providerReadOnly: true });
    await page.goto("http://magpie.test/");
    await page.getByRole("button", { name: "供应商接入", exact: true }).click();
    const row = page.locator("[data-provider-id=openai]");
    await row.waitFor();
    assert.equal(await row.getByRole("button", { name: "编辑供应商", exact: true }).isDisabled(), true);
    assert.equal(await row.getByRole("button", { name: "停用供应商", exact: true }).isDisabled(), true);
    assert.match(await row.textContent(), /Multiple protocol endpoints/);
    assert.equal(state.providerEvents.length, 0);
  });
}
