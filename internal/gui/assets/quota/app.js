"use strict";

(() => {
  /** @typedef {{id:string,name:string,models:string[],totalTokens:number,usedTokens:number,reservedTokens?:number,remainingTokens:number,mcpTotalCalls?:number,mcpUsedCalls?:number,mcpRemainingCalls?:number,enabled:boolean,expiresAt?:string,createdAt:string,shareURL?:string}} Pack */
  /** @typedef {{id:string,packId:string,packName?:string,model:string,tokens:number,status:string,createdAt:string,durationMs?:number,outcome?:string,source?:string,unit?:string,calls?:number,tool?:string}} RequestLog */
  const $ = (selector) => document.querySelector(selector);
  /** @type {{packs:Pack[],models:{id:string,name:string}[],baseURL:string,loaded:boolean,view:string,filter:string,edit:Pack|null,settle:RequestLog|null}} */
  const state = { packs: [], models: [], baseURL: "", loaded: false, view: "overview", filter: "", edit: null, settle: null };
  let modelLoaded = false, logsSequence = 0, formSequence = 0, toastTimer;
  /** @type {Promise<void>|null} */
  let packLoad = null;

  const node = (tag, className = "", text) => {
    const element = document.createElement(tag);
    if (className) element.className = className;
    if (text !== undefined) element.textContent = String(text);
    return element;
  };
  const button = (label, className, action) => {
    const element = node("button", className, label);
    element.type = "button";
    element.addEventListener("click", action);
    return element;
  };
  const number = (value) => new Intl.NumberFormat(quotaLang, { maximumFractionDigits: 0 }).format(value);
  const compact = (value) => {
    if (!Number.isFinite(value)) return "—";
    if (value >= 1e6) return `${new Intl.NumberFormat("en", { maximumFractionDigits: 2 }).format(value / 1e6)}M`;
    if (value >= 1e3) return `${new Intl.NumberFormat("en", { maximumFractionDigits: 2 }).format(value / 1e3)}K`;
    return number(value);
  };
  const date = (value, timeOnly = false) => {
    const at = new Date(value);
    return Number.isNaN(at.getTime()) ? "—" : new Intl.DateTimeFormat(quotaLang, timeOnly
      ? { hour: "2-digit", minute: "2-digit", second: "2-digit" }
      : { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" }).format(at);
  };
  const expired = (pack) => !!pack.expiresAt && new Date(pack.expiresAt).getTime() <= Date.now();
  const packStatus = (pack) => !pack.enabled ? "disabled" : expired(pack) ? "expired" : pack.reservedTokens > 0 ? "pending" : pack.remainingTokens <= 0 ? "exhausted" : "active";
  const badge = (status) => node("span", `badge ${status}`, quotaT(status));
  const empty = (title, description) => {
    const wrap = node("div", "empty-state");
    wrap.append(node("div", "empty-symbol", "◫"), node("h3", "", quotaT(title)));
    if (description) wrap.append(node("p", "", quotaT(description)));
    return wrap;
  };

  /** Read JSON without losing an error response's explanation. */
  async function api(url, options = {}) {
    const response = await fetch(url, { credentials: "same-origin", cache: "no-store", ...options,
      headers: { "Content-Type": "application/json", ...options.headers } });
    const body = await response.json().catch(() => null);
    if (!response.ok) throw new Error(body?.error || `${quotaT("loadFailed")} (${response.status})`);
    if (!body || typeof body !== "object") throw new Error(quotaT("loadFailed"));
    return body;
  }

  function message(error, target = "#global-error") {
    const line = $(target);
    line.textContent = error instanceof Error ? error.message : String(error);
    line.hidden = false;
  }

  function toast(text) {
    clearTimeout(toastTimer);
    $("#toast").textContent = text;
    $("#toast").hidden = false;
    toastTimer = setTimeout(() => { $("#toast").hidden = true; }, 3000);
  }

  async function copy(value) {
    if (!value) return;
    try {
      if (!navigator.clipboard?.writeText) throw new Error("clipboard unavailable");
      await navigator.clipboard.writeText(value);
      toast(quotaT("copied"));
    } catch {
      const text = node("textarea", "manual-copy", value);
      text.readOnly = true;
      text.setAttribute("aria-label", quotaT("copyAddress"));
      const line = $("#global-error");
      line.replaceChildren(node("span", "", quotaT("copyFailed")), text);
      line.hidden = false;
      text.focus({ preventScroll: true });
      text.select();
    }
  }

  function translatePage() {
    document.documentElement.lang = quotaLang;
    document.title = `${quotaT("console")} · magpie`;
    document.querySelectorAll("[data-i18n]").forEach((element) => { element.textContent = quotaT(element.dataset.i18n); });
    document.querySelectorAll("[data-i18n-label]").forEach((element) => { element.setAttribute("aria-label", quotaT(element.dataset.i18nLabel)); });
    $("#log-filter").setAttribute("aria-label", quotaT("filter"));
    $("#log-filter-label").textContent = quotaT("all");
    $("nav").setAttribute("aria-label", quotaT("console"));
  }

  function setView(view) {
    if (!["overview", "packages", "logs", "providers"].includes(view)) return;
    state.view = view;
    document.querySelectorAll(".view").forEach((element) => { element.hidden = element.id !== `view-${view}`; });
    document.querySelectorAll("nav [data-view]").forEach((element) => {
      const active = element.dataset.view === view;
      element.classList.toggle("active", active);
      if (active) element.setAttribute("aria-current", "page");
      else element.removeAttribute("aria-current");
    });
    $("#breadcrumb").textContent = quotaT(view);
    $("#page-title").textContent = quotaT(view);
    $("#page-description").textContent = quotaT(`${view}Description`);
    document.querySelectorAll(".create-pack").forEach((element) => { element.hidden = view === "providers"; });
    $("#add-provider").hidden = view !== "providers";
    if (view === "logs") void loadLogs();
    if (view === "providers") { void window.quotaProviders?.load(); void window.quotaSearches?.load(); }
  }

  function renderBalance(pack, summary) {
    const row = node("article", summary ? "pack-summary" : "package-row");
    row.dataset.packId = pack.id;
    const identity = node("div", "package-identity");
    const name = node("div", "name-line");
    name.append(node("h3", "package-name", pack.name), badge(packStatus(pack)));
    identity.append(name, node("p", "model-ids", (pack.models || []).join(" · ")));
    const amount = node("div", "package-amount");
    amount.append(node("strong", "", quotaT("leftDetail", { n: compact(pack.remainingTokens) })),
      node("span", "muted", quotaT("balanceDetail", { used: compact(pack.usedTokens), total: compact(pack.totalTokens) })));
    const meter = node("div", "meter");
    meter.setAttribute("role", "meter");
    meter.setAttribute("aria-label", quotaT("used"));
    meter.setAttribute("aria-valuemin", "0");
    meter.setAttribute("aria-valuemax", String(pack.totalTokens));
    meter.setAttribute("aria-valuenow", String(Math.min(pack.usedTokens, pack.totalTokens)));
    const fill = node("span", "meter-fill");
    fill.style.width = `${Math.max(0, Math.min(100, pack.totalTokens ? pack.usedTokens / pack.totalTokens * 100 : 0))}%`;
    meter.append(fill);
    const balance = node("div", "balance-group");
    balance.append(amount, meter);
    if (pack.reservedTokens > 0) balance.append(node("span", "reserved-note", quotaT("reserved", { n: number(pack.reservedTokens) })));
    balance.append(node("div", "calls-balance", quotaT("callsBalance", { remaining: number(pack.mcpRemainingCalls || 0), used: number(pack.mcpUsedCalls || 0), total: number(pack.mcpTotalCalls || 0) })));
    row.append(identity, balance);
    if (!summary) {
      const actions = node("div", "row-actions");
      const share = button(quotaT("share"), "button secondary", () => copy(pack.shareURL));
      share.disabled = !pack.shareURL;
      actions.append(share, button(quotaT("edit"), "button quiet", () => openForm(pack)),
        button(quotaT(pack.enabled ? "disable" : "enable"), "button quiet", (event) => togglePack(pack, event.currentTarget)));
      row.append(actions);
    }
    return row;
  }

  function renderPacks() {
    const totals = state.packs.reduce((sum, pack) => ({ totalTokens: sum.totalTokens + pack.totalTokens,
      usedTokens: sum.usedTokens + pack.usedTokens, remainingTokens: sum.remainingTokens + pack.remainingTokens }),
    { totalTokens: 0, usedTokens: 0, remainingTokens: 0 });
    for (const key of Object.keys(totals)) {
      const element = $(`[data-stat="${key}"]`);
      element.textContent = compact(totals[key]);
      element.dataset.value = String(totals[key]);
      element.title = `${number(totals[key])} Token`;
    }
    $("[data-stat=packageCount]").textContent = number(state.packs.length);
    $("#active-count").textContent = quotaT("activeCount", { n: state.packs.filter((pack) => pack.enabled).length });
    $("#usage-ratio").textContent = quotaT("ratio", { n: Math.round(totals.totalTokens ? totals.usedTokens / totals.totalTokens * 100 : 0) });
    $("#package-count-note").textContent = quotaT("packageCountNote", { n: state.packs.length });
    const overview = $("#overview-list"), packages = $("#package-list");
    overview.replaceChildren();
    packages.replaceChildren();
    if (!state.packs.length) {
      overview.append(empty("noPackages", "noPackagesHelp"));
      packages.append(empty("noPackages", "noPackagesHelp"));
      overview.firstElementChild.append(button(quotaT("addProvider"), "button secondary empty-provider-link", () => setView("providers")));
    } else {
      state.packs.slice(0, 6).forEach((pack) => overview.append(renderBalance(pack, true)));
      state.packs.forEach((pack) => packages.append(renderBalance(pack, false)));
    }
    $("#base-url").textContent = state.baseURL || "—";
    $("#copy-base-url").disabled = !state.baseURL;
    renderFilter();
  }

  function loadPacks() {
    if (packLoad) return packLoad;
    packLoad = fetchPacks().finally(() => { packLoad = null; });
    return packLoad;
  }

  // A mutation must not reuse an older refresh already in flight: its response
  // may describe the package from before that mutation.
  async function reloadPacks() {
    if (packLoad) await packLoad;
    await loadPacks();
  }

  async function fetchPacks() {
    $("#refresh").disabled = true;
    $("#global-error").hidden = true;
    if (!state.loaded) {
      $("#overview-list").replaceChildren(empty("loading"));
      $("#package-list").replaceChildren(empty("loading"));
    }
    try {
      const body = await api("/api/token-packs");
      if (!Array.isArray(body.packs)) throw new Error(quotaT("loadFailed"));
      state.packs = body.packs;
      state.baseURL = typeof body.baseURL === "string" ? body.baseURL : "";
      state.loaded = true;
      renderPacks();
      $("#updated-at").textContent = quotaT("refreshed", { time: date(new Date().toISOString(), true) });
    } catch (error) {
      message(`${quotaT(state.loaded ? "refreshFailed" : "loadFailed")} ${error.message}`);
      if (!state.loaded) {
        $("#overview-list").replaceChildren(empty("loadFailed"));
        $("#package-list").replaceChildren(empty("loadFailed"));
      }
    } finally {
      $("#refresh").disabled = false;
    }
  }

  function renderFilter() {
    if (state.filter && !state.packs.some((pack) => pack.id === state.filter)) state.filter = "";
    $("#log-filter-label").textContent = state.packs.find((pack) => pack.id === state.filter)?.name || quotaT("all");
    const menu = $("#log-filter-menu");
    menu.replaceChildren();
    for (const choice of [{ id: "", name: quotaT("all") }, ...state.packs]) {
      const item = button(choice.name, "filter-item", () => {
        state.filter = choice.id;
        closeFilter(true);
        renderFilter();
        void loadLogs();
      });
      item.setAttribute("role", "menuitemradio");
      item.setAttribute("aria-checked", String(state.filter === choice.id));
      menu.append(item);
    }
  }

  function closeFilter(returnFocus = false) {
    const wasOpen = !$("#log-filter-menu").hidden;
    $("#log-filter-menu").hidden = true;
    $("#log-filter").setAttribute("aria-expanded", "false");
    if (returnFocus && wasOpen) $("#log-filter").focus({ preventScroll: true });
  }

  async function loadLogs() {
    const sequence = ++logsSequence;
    const container = $("#log-list");
    container.replaceChildren(empty("loading"));
    try {
      const query = new URLSearchParams({ limit: "100" });
      if (state.filter) query.set("pack", state.filter);
      const body = await api(`/api/token-packs/logs?${query}`);
      if (sequence !== logsSequence) return;
      if (!Array.isArray(body.logs)) throw new Error(quotaT("loadFailed"));
      container.replaceChildren();
      if (!body.logs.length) return void container.append(empty("noLogs", "noLogsHelp"));
      const scroll = node("div", "table-scroll"), table = node("table", "log-table"), head = node("thead"), tr = node("tr");
      for (const label of ["time", "package", "modelOrTool", "consumed", "status", "duration"]) tr.append(node("th", "", quotaT(label)));
      head.append(tr);
      table.append(head);
      const tbody = node("tbody");
      for (const log of body.logs) {
        const outcome = log.outcome || log.status;
        const calls = log.unit === "calls";
        const unsettled = !calls && (outcome === "uncertain" || outcome === "pending");
        const row = node("tr");
        const model = node("td", "model-cell", calls ? log.tool || "—" : log.model || "—");
        if (log.source === "mcp") model.append(node("span", "http-code mcp-source", "MCP"));
        row.append(node("td", "time-cell", date(log.createdAt)), node("td", "", log.packName || state.packs.find((pack) => pack.id === log.packId)?.name || log.packId),
          model, node("td", "numeric", calls ? quotaT("callsCount", { n: number(log.calls || 0) }) : unsettled ? "—" : `${number(log.tokens || 0)} Token`));
        const status = node("td", "");
        status.append(badge(["completed", "failed", "pending", "uncertain"].includes(outcome) ? outcome : "uncertain"));
        if (/^[1-5]\d{2}$/.test(String(log.status))) status.append(node("span", "http-code", `HTTP ${log.status}`));
        if (unsettled) status.append(button(quotaT("settle"), "text-button settle-button", () => openSettlement(log)));
        row.append(status, node("td", "time-cell", Number.isFinite(log.durationMs) ? `${number(log.durationMs)} ms` : "—"));
        tbody.append(row);
      }
      table.append(tbody);
      scroll.append(table);
      container.append(scroll);
    } catch (error) {
      if (sequence !== logsSequence) return;
      container.replaceChildren(empty("loadFailed"));
      message(error);
    }
  }

  /** Parse explicit token units; partial numbers, fractions and unsafe integers fail. */
  function tokenAmount(value, allowZero = false) {
    const match = value.trim().match(/^(\d+(?:\.\d+)?)\s*([km]|万|萬|亿|億)?$/iu);
    if (!match) return null;
    const multiplier = { k: 1e3, m: 1e6, "万": 1e4, "萬": 1e4, "亿": 1e8, "億": 1e8 }[match[2]?.toLowerCase()] || 1;
    const result = Number(match[1]) * multiplier;
    return Number.isSafeInteger(result) && (allowZero ? result >= 0 : result > 0) ? result : null;
  }

  async function openForm(pack = null) {
    const sequence = ++formSequence;
    state.edit = pack;
    state.settle = null;
    $("#pack-form").reset();
    $("#form-error").hidden = true;
    $("#dialog-title").textContent = quotaT(pack ? "edit" : "create");
    $("#dialog-description").textContent = quotaT(pack ? "editHelp" : "createHelp");
    $("#submit-pack").textContent = quotaT(pack ? "save" : "create");
    $("#create-fields").hidden = !!pack;
    $("#expiry-field").hidden = !!pack;
    $("#calls-field").hidden = false;
    $("#pack-quota").closest("label").querySelector("[data-i18n=quota]").textContent = quotaT("quota");
    $("#pack-quota").value = pack ? String(pack.totalTokens) : "1M";
    $("#pack-calls").value = String(pack?.mcpTotalCalls || 0);
    $("#pack-dialog").showModal();
    (pack ? $("#pack-quota") : $("#pack-name")).focus({ preventScroll: true });
    if (!pack) {
      $("#model-choices").replaceChildren();
      if (!modelLoaded) {
        try {
          const body = await api("/api/token-packs/models");
          state.models = Array.isArray(body.models) ? body.models : [];
          modelLoaded = true;
        } catch (error) {
          // Manual IDs stay available when discovery fails.
          if (sequence === formSequence) message(error, "#form-error");
        }
      }
      if (sequence !== formSequence || !$("#pack-dialog").open || state.settle) return;
      for (const model of state.models) {
        const label = node("label", "model-choice"), input = node("input");
        input.type = "checkbox";
        input.value = model.id;
        label.append(input, node("span", "", model.name || model.id));
        $("#model-choices").append(label);
      }
      if (!state.models.length) {
        $("#model-choices").append(button(quotaT("providerFirst"), "button secondary", () => { closeForm(); setView("providers"); }));
      }
    }
  }

  function openSettlement(log) {
    void openForm(state.packs.find((pack) => pack.id === log.packId) || { totalTokens: 0 });
    state.settle = log;
    $("#dialog-title").textContent = quotaT("settle");
    $("#dialog-description").textContent = quotaT("settleHelp");
    $("#submit-pack").textContent = quotaT("settle");
    $("#pack-quota").closest("label").querySelector("[data-i18n=quota]").textContent = quotaT("actualTokens");
    $("#pack-quota").value = "";
    $("#calls-field").hidden = true;
  }

  function closeForm() {
    if ($("#submit-pack").disabled) return;
    $("#pack-dialog").close();
    state.edit = null;
    state.settle = null;
    formSequence++;
  }

  async function submitForm(event) {
    event.preventDefault();
    const submit = $("#submit-pack");
    if (submit.disabled) return;
    $("#form-error").hidden = true;
    const totalTokens = tokenAmount($("#pack-quota").value, !!state.settle);
    if (totalTokens === null) return message(quotaT("invalidQuota"), "#form-error");
    const callInput = $("#pack-calls").value.trim();
    const mcpTotalCalls = /^\d+$/.test(callInput) ? Number(callInput) : NaN;
    if (!state.settle && (!Number.isSafeInteger(mcpTotalCalls) || mcpTotalCalls < 0)) return message(quotaT("invalidCalls"), "#form-error");
    let body, url = "/api/token-packs", method = "POST";
    if (state.settle) {
      url = `/api/token-packs/logs/${encodeURIComponent(state.settle.id)}/settle`;
      body = { actualTokens: totalTokens };
    } else if (state.edit) {
      body = {};
      if (totalTokens !== state.edit.totalTokens) body.totalTokens = totalTokens;
      if (mcpTotalCalls !== (state.edit.mcpTotalCalls || 0)) body.mcpTotalCalls = mcpTotalCalls;
      if (!Object.keys(body).length) return closeForm();
      url += `/${encodeURIComponent(state.edit.id)}`;
      method = "PATCH";
    } else {
      const name = $("#pack-name").value.trim();
      if (!name) return message(quotaT("nameRequired"), "#form-error");
      const models = [...new Set([...document.querySelectorAll("#model-choices input:checked")].map((input) => input.value)
        .concat($("#custom-models").value.split(/[,，\n]/).map((value) => value.trim()).filter(Boolean)))];
      if (!models.length) return message(quotaT("modelsRequired"), "#form-error");
      body = { name, models, totalTokens, mcpTotalCalls };
      const expires = $("#pack-expires").value;
      if (expires) {
        const value = new Date(expires);
        if (!Number.isFinite(value.getTime()) || value.getTime() <= Date.now()) return message(quotaT("invalidExpiry"), "#form-error");
        body.expiresAt = value.toISOString();
      }
    }
    const previousLabel = submit.textContent;
    submit.disabled = true;
    submit.textContent = quotaT("saving");
    try {
      await api(url, { method, body: JSON.stringify(body) });
      const settled = !!state.settle;
      submit.disabled = false;
      closeForm();
      if (!settled) setView("packages");
      await reloadPacks();
      if (settled) await loadLogs();
    } catch (error) {
      message(error, "#form-error");
    } finally {
      submit.disabled = false;
      submit.textContent = previousLabel;
    }
  }

  async function togglePack(pack, control) {
    control.disabled = true;
    try {
      await api(`/api/token-packs/${encodeURIComponent(pack.id)}`, { method: "PATCH", body: JSON.stringify({ enabled: !pack.enabled }) });
      await reloadPacks();
    } catch (error) {
      message(error);
    } finally {
      control.disabled = false;
    }
  }

  translatePage();
  document.querySelectorAll("nav [data-view]").forEach((element) => element.addEventListener("click", () => setView(element.dataset.view)));
  document.querySelectorAll(".create-pack").forEach((element) => element.addEventListener("click", () => openForm()));
  $("#manage-all").addEventListener("click", () => setView("packages"));
  $("#refresh").addEventListener("click", async () => {
    await loadPacks();
    if (state.view === "logs") await loadLogs();
    if (state.view === "providers") { await window.quotaProviders?.load(); await window.quotaSearches?.load(); }
  });
  $("#copy-base-url").addEventListener("click", () => copy(state.baseURL));
  $("#pack-form").addEventListener("submit", submitForm);
  $("#close-dialog").addEventListener("click", closeForm);
  $("#cancel-dialog").addEventListener("click", closeForm);
  $("#pack-dialog").addEventListener("cancel", (event) => { if ($("#submit-pack").disabled) event.preventDefault(); });
  $("#log-filter").addEventListener("click", () => {
    const menu = $("#log-filter-menu");
    menu.hidden = !menu.hidden;
    $("#log-filter").setAttribute("aria-expanded", String(!menu.hidden));
    if (!menu.hidden) menu.querySelector("[aria-checked=true]")?.focus({ preventScroll: true });
  });
  document.addEventListener("click", (event) => { if (!event.target.closest(".filter-wrap")) closeFilter(); });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") closeFilter(true);
    if (!$("#log-filter-menu").hidden && ["ArrowDown", "ArrowUp"].includes(event.key)) {
      event.preventDefault();
      const choices = [...$("#log-filter-menu").children], current = choices.indexOf(document.activeElement);
      choices[(current + (event.key === "ArrowDown" ? 1 : -1) + choices.length) % choices.length]?.focus({ preventScroll: true });
    }
  });
  // The provider view shares the small console's primitives, not the desktop
  // shell. Saving a provider invalidates model discovery immediately.
  window.quotaConsole = { api, node, button, empty, message, toast, setView,
    modelsChanged() { modelLoaded = false; state.models = []; } };
  setView("overview");
  void loadPacks();
})();
