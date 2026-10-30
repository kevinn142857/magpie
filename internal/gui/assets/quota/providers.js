"use strict";

(() => {
  /** @typedef {{id:string,name:string,protocol:string,baseURL:string,models:string[],enabled:boolean,keyConfigured:boolean,keyMasked?:string,editable?:boolean,unsupportedReason?:string}} QuotaProvider */
  const { api, node, button, empty, message, modelsChanged } = window.quotaConsole;
  const $ = (selector) => document.querySelector(selector);
  const protocols = { chat: "OpenAI Chat", responses: "OpenAI Responses", anthropic: "Anthropic Messages", gemini: "Gemini" };
  /** @type {QuotaProvider[]} */
  let providers = [];
  /** @type {QuotaProvider|null} */
  let editing = null;
  let protocol = "chat", busy = false, loaded = false, listRequest = null;

  function renderProviders() {
    const list = $("#provider-list");
    list.replaceChildren();
    if (!providers.length) {
      const none = empty("noProviders", "noProvidersHelp");
      none.append(button(quotaT("addProvider"), "button primary empty-provider-link", () => openProvider()));
      list.append(none);
      return;
    }
    for (const provider of providers) {
      const row = node("article", "provider-row");
      row.dataset.providerId = provider.id;
      const identity = node("div", "provider-identity"), name = node("div", "name-line");
      name.append(node("h3", "package-name", provider.name), node("span", `badge ${provider.enabled ? "active" : "disabled"}`, quotaT(provider.enabled ? "active" : "disabled")));
      identity.append(name, node("p", "provider-address", provider.baseURL));
      const meta = node("div", "provider-meta");
      meta.append(node("span", "protocol-tag", protocols[provider.protocol] || provider.protocol),
        node("span", "muted", quotaT(provider.keyConfigured ? "providerKeyConfigured" : "providerNoKey")));
      identity.append(meta);
      const models = node("div", "provider-models");
      for (const model of provider.models || []) models.append(node("code", "model-chip", model));
      const actions = node("div", "row-actions");
      const edit = button(quotaT("editProvider"), "button secondary", () => openProvider(provider));
      const toggle = button(quotaT(provider.enabled ? "disableProvider" : "enableProvider"), "button quiet", () => toggleProvider(provider, toggle));
      if (provider.editable === false) {
        edit.disabled = true;
        toggle.disabled = true;
        edit.title = provider.unsupportedReason || quotaT("providerReadOnly");
        identity.append(node("p", "provider-readonly", provider.unsupportedReason || quotaT("providerReadOnly")));
      }
      actions.append(edit, toggle);
      row.append(identity, models, actions);
      list.append(row);
    }
  }

  async function fetchProviders() {
    if (!loaded) $("#provider-list").replaceChildren(empty("loading"));
    try {
      const body = await api("/api/quota-providers");
      if (!Array.isArray(body.providers)) throw new Error(quotaT("loadFailed"));
      providers = body.providers;
      loaded = true;
      renderProviders();
    } catch (error) {
      if (!loaded) $("#provider-list").replaceChildren(empty("loadFailed"));
      message(error);
    }
  }

  function load() {
    if (listRequest) return listRequest;
    listRequest = fetchProviders().finally(() => { listRequest = null; });
    return listRequest;
  }

  async function refreshAfterMutation() {
    modelsChanged();
    if (listRequest) await listRequest;
    await load();
  }

  function urlHelp() {
    $("#provider-protocol-name").textContent = protocols[protocol];
    $("#provider-url-help").textContent = quotaT(protocol === "gemini" ? "providerGeminiHelp" : protocol === "anthropic" ? "providerAnthropicHelp" : "providerChatHelp");
    $("#provider-url").placeholder = protocol === "anthropic" ? "https://api.example.com" : protocol === "gemini" ? "https://api.example.com/v1beta" : "https://api.example.com/v1";
    $("#provider-protocol-menu").replaceChildren();
    for (const [id, name] of Object.entries(protocols)) {
      const item = button(name, "filter-item", () => { protocol = id; closeProtocols(true); urlHelp(); });
      item.setAttribute("role", "menuitemradio");
      item.setAttribute("aria-checked", String(protocol === id));
      $("#provider-protocol-menu").append(item);
    }
  }

  function openProvider(provider = null) {
    if (provider?.editable === false) return;
    editing = provider;
    protocol = provider?.protocol || "chat";
    if (!(protocol in protocols)) protocol = "chat";
    $("#provider-form").reset();
    $("#provider-form-error").hidden = true;
    $("#provider-feedback").hidden = true;
    $("#provider-dialog-title").textContent = quotaT(provider ? "editProvider" : "addProvider");
    $("#provider-name").value = provider?.name || "";
    $("#provider-url").value = provider?.baseURL || "";
    $("#provider-models").value = (provider?.models || []).join("\n");
    $("#provider-key").value = "";
    $("#provider-key").placeholder = provider?.keyConfigured ? quotaT("providerKeyPlaceholder") : "sk-…";
    $("#provider-key-help").textContent = quotaT(provider?.keyConfigured ? "providerKeepKey" : "providerKeyHelp");
    closeProtocols();
    urlHelp();
    $("#provider-dialog").showModal();
    $("#provider-name").focus({ preventScroll: true });
  }

  function closeProvider() {
    if (busy) return;
    $("#provider-key").value = "";
    $("#provider-dialog").close();
    editing = null;
    closeProtocols();
  }

  /** Normalize enough to compare destinations; the server performs the final validation. */
  function normalizedURL(value) {
    let url;
    try { url = new URL(value.trim()); } catch { throw new Error(quotaT("providerURLInvalid")); }
    if (!["https:", "http:"].includes(url.protocol) || url.username || url.password || url.search || url.hash) throw new Error(quotaT("providerURLInvalid"));
    return url.href.replace(/\/+$/, "");
  }

  function providerInput(requireModels) {
    const baseURL = normalizedURL($("#provider-url").value);
    const apiKey = $("#provider-key").value.trim();
    if (editing?.keyConfigured && !apiKey && (editing.protocol !== protocol || normalizedURL(editing.baseURL) !== baseURL)) throw new Error(quotaT("providerChangedKey"));
    const models = [...new Set($("#provider-models").value.split(/[,，\n]/).map((model) => model.trim()).filter(Boolean))];
    if (requireModels && !models.length) throw new Error(quotaT("modelsRequired"));
    const input = { name: $("#provider-name").value.trim(), protocol, baseURL, models };
    if (apiKey) input.apiKey = apiKey;
    return input;
  }

  function setBusy(value) {
    busy = value;
    $("#provider-form").querySelectorAll("input,textarea,button").forEach((element) => { element.disabled = value; });
  }

  async function saveProvider(event) {
    event.preventDefault();
    if (busy) return;
    $("#provider-form-error").hidden = true;
    let input;
    try {
      input = providerInput(true);
      if (!input.name) throw new Error(quotaT("providerNameRequired"));
    } catch (error) { return message(error, "#provider-form-error"); }
    let payload = input;
    if (editing) {
      payload = {};
      for (const key of ["name", "protocol", "baseURL", "models"]) {
        if (JSON.stringify(input[key]) !== JSON.stringify(editing[key])) payload[key] = input[key];
      }
      if (input.apiKey) payload.apiKey = input.apiKey;
      if (!Object.keys(payload).length) return closeProvider();
    }
    const endpoint = editing ? `/api/quota-providers/${encodeURIComponent(editing.id)}` : "/api/quota-providers";
    const method = editing ? "PATCH" : "POST";
    setBusy(true);
    try {
      await api(endpoint, { method, body: JSON.stringify(payload) });
      setBusy(false);
      closeProvider();
      await refreshAfterMutation();
    } catch (error) { message(error, "#provider-form-error"); }
    finally { setBusy(false); }
  }

  async function checkProvider(kind) {
    if (busy) return;
    $("#provider-form-error").hidden = true;
    $("#provider-feedback").hidden = true;
    let input;
    try { input = providerInput(kind === "probe"); }
    catch (error) { return message(error, "#provider-form-error"); }
    if (editing) input.id = editing.id;
    setBusy(true);
    try {
      const result = await api(`/api/quota-providers/${kind}`, { method: "POST", body: JSON.stringify(input) });
      if (kind === "models") {
        if (!Array.isArray(result.models) || !result.models.every((model) => typeof model === "string")) throw new Error(quotaT("loadFailed"));
        $("#provider-models").value = result.models.join("\n");
        $("#provider-feedback").textContent = quotaT("modelsFetched", { n: result.models.length });
      } else {
        if (!result.ok) throw new Error(result.message || quotaT("providerProbeFailed"));
        const details = [];
        if (/^[1-5]\d{2}$/.test(String(result.statusCode))) details.push(`HTTP ${result.statusCode}`);
        if (typeof result.model === "string") details.push(result.model);
        if (Number.isFinite(result.millis)) details.push(`${Math.max(0, Math.round(result.millis))} ms`);
        $("#provider-feedback").textContent = [quotaT("providerConnected"), ...details].join(" · ");
      }
      $("#provider-feedback").hidden = false;
    } catch (error) { message(error, "#provider-form-error"); }
    finally { setBusy(false); }
  }

  async function toggleProvider(provider, control) {
    control.disabled = true;
    try {
      await api(`/api/quota-providers/${encodeURIComponent(provider.id)}`, { method: "PATCH", body: JSON.stringify({ enabled: !provider.enabled }) });
      await refreshAfterMutation();
    } catch (error) { message(error); }
    finally { control.disabled = false; }
  }

  function closeProtocols(focus = false) {
    const open = !$("#provider-protocol-menu").hidden;
    $("#provider-protocol-menu").hidden = true;
    $("#provider-protocol").setAttribute("aria-expanded", "false");
    if (focus && open) $("#provider-protocol").focus({ preventScroll: true });
  }

  $("#add-provider").addEventListener("click", () => openProvider());
  $("#provider-form").addEventListener("submit", saveProvider);
  $("#close-provider").addEventListener("click", closeProvider);
  $("#cancel-provider").addEventListener("click", closeProvider);
  $("#discover-provider").addEventListener("click", () => checkProvider("models"));
  $("#probe-provider").addEventListener("click", () => checkProvider("probe"));
  $("#provider-dialog").addEventListener("cancel", (event) => { if (busy) event.preventDefault(); });
  $("#provider-dialog").addEventListener("close", () => { $("#provider-key").value = ""; editing = null; });
  $("#provider-protocol").addEventListener("click", () => {
    const menu = $("#provider-protocol-menu");
    menu.hidden = !menu.hidden;
    $("#provider-protocol").setAttribute("aria-expanded", String(!menu.hidden));
    if (!menu.hidden) menu.querySelector("[aria-checked=true]")?.focus({ preventScroll: true });
  });
  document.addEventListener("click", (event) => { if (!event.target.closest(".protocol-picker")) closeProtocols(); });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") closeProtocols(true);
    if (!$("#provider-protocol-menu").hidden && ["ArrowDown", "ArrowUp"].includes(event.key)) {
      event.preventDefault();
      const choices = [...$("#provider-protocol-menu").children], current = choices.indexOf(document.activeElement);
      choices[(current + (event.key === "ArrowDown" ? 1 : -1) + choices.length) % choices.length]?.focus({ preventScroll: true });
    }
  });
  window.quotaProviders = { load };
})();
