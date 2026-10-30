"use strict";

(() => {
  /** @typedef {{vendor:string,name:string,baseURL:string,enabled:boolean,keyConfigured:boolean,editable?:boolean,unsupportedReason?:string}} SearchService */
  /** @typedef {{id:string,name:string,baseURL:string,keyOptional:boolean}} SearchVendor */
  const { api, node, button, empty, message } = window.quotaConsole;
  const $ = (selector) => document.querySelector(selector);
  /** @type {SearchService[]} */
  let searches = [];
  /** @type {SearchVendor[]} */
  let vendors = [];
  /** @type {SearchService|null} */
  let editing = null;
  let vendor = "", busy = false, loaded = false, request = null;

  function render() {
    const list = $("#search-list");
    list.replaceChildren();
    if (!searches.length) return void list.append(empty("noSearches", "noSearchesHelp"));
    for (const search of searches) {
      const row = node("article", "provider-row search-row");
      row.dataset.searchVendor = search.vendor;
      const identity = node("div", "provider-identity"), name = node("div", "name-line");
      name.append(node("h3", "package-name", search.name), node("span", `badge ${search.enabled ? "active" : "disabled"}`, quotaT(search.enabled ? "active" : "disabled")));
      identity.append(name, node("p", "provider-address", search.baseURL), node("p", "muted", quotaT(search.keyConfigured ? "providerKeyConfigured" : "providerNoKey")));
      const actions = node("div", "row-actions");
      const edit = button(quotaT("editSearch"), "button secondary", () => open(search));
      const toggle = button(quotaT(search.enabled ? "disableSearch" : "enableSearch"), "button quiet", () => toggleSearch(search, toggle));
      if (search.editable === false) {
        edit.disabled = toggle.disabled = true;
        identity.append(node("p", "provider-readonly", search.unsupportedReason || quotaT("providerReadOnly")));
      }
      actions.append(edit, toggle);
      row.append(identity, actions);
      list.append(row);
    }
  }

  async function fetchSearches() {
    if (!loaded) $("#search-list").replaceChildren(empty("loading"));
    $("#add-search").disabled = true;
    try {
      const body = await api("/api/quota-searches");
      if (!Array.isArray(body.searches) || !Array.isArray(body.vendors)) throw new Error(quotaT("loadFailed"));
      searches = body.searches;
      vendors = body.vendors;
      loaded = true;
      render();
    } catch (error) {
      if (!loaded) $("#search-list").replaceChildren(empty("loadFailed"));
      message(error);
    } finally { $("#add-search").disabled = !vendors.length; }
  }

  function load() {
    if (request) return request;
    request = fetchSearches().finally(() => { request = null; });
    return request;
  }

  async function reload() {
    if (request) await request;
    await load();
  }

  function renderVendors() {
    const selected = vendors.find((item) => item.id === vendor);
    $("#search-vendor-name").textContent = selected?.name || vendor;
    $("#search-key-help").textContent = quotaT(editing?.keyConfigured ? "providerKeepKey" : selected?.keyOptional ? "searchKeyOptional" : "providerKeyHelp");
    $("#search-vendor-menu").replaceChildren();
    for (const choice of vendors) {
      const item = button(choice.name, "filter-item", () => {
        vendor = choice.id;
        $("#search-url").value = choice.baseURL || "";
        closeMenu(true);
        renderVendors();
      });
      item.setAttribute("role", "menuitemradio");
      item.setAttribute("aria-checked", String(vendor === choice.id));
      $("#search-vendor-menu").append(item);
    }
  }

  function open(search = null) {
    if (search?.editable === false || !vendors.length) return;
    editing = search;
    vendor = search?.vendor || vendors[0].id;
    $("#search-form").reset();
    $("#search-form-error").hidden = true;
    $("#search-dialog-title").textContent = quotaT(search ? "editSearch" : "addSearch");
    $("#search-url").value = search?.baseURL || vendors.find((item) => item.id === vendor)?.baseURL || "";
    $("#search-key").value = "";
    $("#search-key").placeholder = search?.keyConfigured ? quotaT("providerKeyPlaceholder") : "";
    $("#search-vendor").disabled = !!search;
    closeMenu();
    renderVendors();
    $("#search-dialog").showModal();
    (search ? $("#search-url") : $("#search-vendor")).focus({ preventScroll: true });
  }

  function close() {
    if (busy) return;
    $("#search-key").value = "";
    $("#search-dialog").close();
    editing = null;
    closeMenu();
  }

  function normalize(value) {
    let url;
    try { url = new URL(value.trim()); } catch { throw new Error(quotaT("providerURLInvalid")); }
    if (!["https:", "http:"].includes(url.protocol) || url.username || url.password || url.search || url.hash) throw new Error(quotaT("providerURLInvalid"));
    return url.href.replace(/\/+$/, "");
  }

  function setBusy(value) {
    busy = value;
    $("#search-form").querySelectorAll("input,button").forEach((element) => { element.disabled = value; });
    $("#search-vendor").disabled = value || !!editing;
  }

  async function save(event) {
    event.preventDefault();
    if (busy) return;
    $("#search-form-error").hidden = true;
    let payload;
    try {
      const baseURL = normalize($("#search-url").value), apiKey = $("#search-key").value.trim();
      if (editing?.keyConfigured && !apiKey && normalize(editing.baseURL) !== baseURL) throw new Error(quotaT("providerChangedKey"));
      const choice = vendors.find((item) => item.id === vendor);
      if (!choice) throw new Error(quotaT("loadFailed"));
      if (!editing?.keyConfigured && !choice.keyOptional && !apiKey) throw new Error(quotaT("searchKeyRequired"));
      payload = editing ? {} : { vendor, baseURL };
      if (editing && baseURL !== normalize(editing.baseURL)) payload.baseURL = baseURL;
      if (apiKey) payload.apiKey = apiKey;
      if (editing && !Object.keys(payload).length) return close();
    } catch (error) { return message(error, "#search-form-error"); }
    const endpoint = editing ? `/api/quota-searches/${encodeURIComponent(editing.vendor)}` : "/api/quota-searches";
    setBusy(true);
    try {
      await api(endpoint, { method: editing ? "PATCH" : "POST", body: JSON.stringify(payload) });
      setBusy(false);
      close();
      await reload();
    } catch (error) { message(error, "#search-form-error"); }
    finally { setBusy(false); }
  }

  async function toggleSearch(search, control) {
    control.disabled = true;
    try {
      await api(`/api/quota-searches/${encodeURIComponent(search.vendor)}`, { method: "PATCH", body: JSON.stringify({ enabled: !search.enabled }) });
      await reload();
    } catch (error) { message(error); }
    finally { control.disabled = false; }
  }

  function closeMenu(focus = false) {
    const wasOpen = !$("#search-vendor-menu").hidden;
    $("#search-vendor-menu").hidden = true;
    $("#search-vendor").setAttribute("aria-expanded", "false");
    if (focus && wasOpen) $("#search-vendor").focus({ preventScroll: true });
  }

  $("#add-search").addEventListener("click", () => open());
  $("#search-form").addEventListener("submit", save);
  $("#close-search").addEventListener("click", close);
  $("#cancel-search").addEventListener("click", close);
  $("#search-dialog").addEventListener("cancel", (event) => { if (busy) event.preventDefault(); });
  $("#search-dialog").addEventListener("close", () => { $("#search-key").value = ""; editing = null; closeMenu(); });
  $("#search-vendor").addEventListener("click", () => {
    const menu = $("#search-vendor-menu");
    menu.hidden = !menu.hidden;
    $("#search-vendor").setAttribute("aria-expanded", String(!menu.hidden));
    if (!menu.hidden) menu.querySelector("[aria-checked=true]")?.focus({ preventScroll: true });
  });
  document.addEventListener("click", (event) => { if (!event.target.closest(".search-picker")) closeMenu(); });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") closeMenu(true);
    if (!$("#search-vendor-menu").hidden && ["ArrowDown", "ArrowUp"].includes(event.key)) {
      event.preventDefault();
      const choices = [...$("#search-vendor-menu").children], current = choices.indexOf(document.activeElement);
      choices[(current + (event.key === "ArrowDown" ? 1 : -1) + choices.length) % choices.length]?.focus({ preventScroll: true });
    }
  });
  window.quotaSearches = { load };
})();
