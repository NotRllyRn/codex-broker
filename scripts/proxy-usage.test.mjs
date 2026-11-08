import assert from "node:assert/strict";
import { cpSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import test from "node:test";

// Import the unmodified browser module graph in a fresh ESM package per fixture.
// This isolates cache.js state without requiring experimental VM flags.
async function browser(t, context) {
 const directory = mkdtempSync(join(tmpdir(), "proxy-dashboard-test-"));
 const cleanups = [], restores = [];
 t.after(() => {
  try { for (const cleanup of cleanups.reverse()) cleanup(); }
  finally {
   for (const restore of restores.reverse()) restore();
   rmSync(directory, { recursive: true, force: true });
  }
 });
 cpSync(new URL("../internal/webassets/static/", import.meta.url), directory, { recursive: true });
 writeFileSync(join(directory, "package.json"), '{"type":"module"}');
 const document = context.document;
 const querySelector = document.querySelector?.bind(document);
 document.querySelector = (selector) => selector === "[data-dashboard-shell]" ? {} : querySelector?.(selector) ?? null;
 document.querySelectorAll ??= () => [];
 document.documentElement ??= { dataset: {} };
 document.createTreeWalker ??= () => ({ nextNode: () => false });
 const globals = {
  document, NodeFilter: { SHOW_TEXT: 4 },
  localStorage: { getItem: () => null, setItem() {} },
  location: context.location ?? { protocol: "https:", reload() {} },
  fetch: context.fetch,
  window: { fetch: context.fetch, addEventListener() {},
   setTimeout: context.setTimeout ?? (() => 1), clearTimeout() {},
   setInterval: () => 1, clearInterval() {} },
 };
 for (const [key, value] of Object.entries(globals)) {
  const previous = Object.getOwnPropertyDescriptor(globalThis, key);
  Object.defineProperty(globalThis, key, { configurable: true, writable: true, value });
  restores.push(() => previous ? Object.defineProperty(globalThis, key, previous) : delete globalThis[key]);
 }
 const loadModule = (name) => import(pathToFileURL(join(directory, name)).href);
 loadModule.cleanup = (callback) => cleanups.push(callback);
 return loadModule;
}

test("shared model selector updates rolling/lifetime tokens and quota without fallback", async (t) => {
	const panels = ["rolling_7d", "lifetime"].map((period) => {
		const nodes = Object.fromEntries(["active_ms", "total_tokens", "tokens_per_minute", "tokens_per_hour"].map((name) => [name, { dataset: { usage: name } }]));
		for (const name of ["short_rate", "short_time", "weekly_rate", "weekly_time"]) nodes[name] = {};
		return {
			dataset: { proxyPeriod: period }, nodes,
			querySelectorAll: () => Object.values(nodes).filter((node) => node.dataset),
			querySelector: (selector) => nodes[selector.match(/="(.*)"/)[1]],
		};
	});
	let change, refresh;
	const select = {
		value: "all", options: [],
		addEventListener: (_, callback) => { change = callback; },
		replaceChildren(...options) { this.options = options; this.value = options[0].value; },
	};
	const status = { dataset: {}, replaceChildren() {} };
	const card = {
		querySelector: (selector) => selector === "[data-proxy-model]" ? select : status,
		querySelectorAll: () => panels,
	};
	const tokens = (rate) => ({ tokens_per_hour: rate, total_tokens: rate, active_ms: 3600000 });
	const quota = (rate) => ({ short: { usage_per_hour: rate, active_ms: 3600000, samples: 1 } });
	const data = {
		ready: true, recent_clients: [], relays: [],
		usage: [
			{ model: "all", rolling_7d: tokens(300), lifetime: tokens(600) },
			{ model: "gpt-a", rolling_7d: tokens(100), lifetime: tokens(200) },
			{ model: "gpt-b", rolling_7d: tokens(200), lifetime: tokens(400) },
		],
		quota_usage: [
			{ account_id: "all", model: "all", rolling_7d: quota(9), lifetime: quota(8) },
			{ account_id: "all", model: "gpt-a", rolling_7d: quota(3), lifetime: quota(4) },
			{ account_id: "account0", model: "gpt-a", rolling_7d: quota(99), lifetime: quota(99) },
			// Quota lifetime may outlive response detail or have no token samples.
			{ account_id: "all", model: "quota-only", lifetime: quota(5) },
		],
	};
	const loadModule = await browser(t, {
		document: { querySelector: (selector) => selector === "[data-proxy-card]" ? card : null, createElement: () => ({ dataset: {} }) },
		fetch: async () => ({ ok: true, json: async () => data }),
		setTimeout: (callback) => { refresh = callback; },
	});
	const { initApp } = await loadModule("app.js");
	loadModule.cleanup(initApp());
	await new Promise(setImmediate);
	assert.equal(panels[0].nodes.tokens_per_hour.textContent, "300");
	assert.equal(panels[1].nodes.short_rate.textContent, "8 pp/h");
	select.value = "gpt-a";
	change();
	assert.equal(panels[0].nodes.tokens_per_hour.textContent, "100");
	assert.equal(panels[1].nodes.tokens_per_hour.textContent, "200");
	assert.equal(panels[0].nodes.short_rate.textContent, "3 pp/h");
	assert.equal(panels[1].nodes.short_rate.textContent, "4 pp/h");
	assert.equal(panels[0].nodes.weekly_rate.textContent, "—");
	await refresh();
	assert.equal(select.value, "gpt-a", "refresh must preserve selection");
	select.value = "gpt-b";
	change();
	assert.equal(panels[1].nodes.tokens_per_hour.textContent, "400");
	assert.equal(panels[1].nodes.short_rate.textContent, "—", "missing model data must not use All models");
	assert.ok(select.options.some((option) => option.value === "quota-only"));
	select.value = "quota-only";
	change();
	assert.equal(panels[0].nodes.tokens_per_hour.textContent, "0");
	assert.equal(panels[0].nodes.short_rate.textContent, "—");
	assert.equal(panels[1].nodes.short_rate.textContent, "5 pp/h");
});

test("usage dashboard scopes identity/device queries and resets device on identity changes", async (t) => {
	const node = () => ({ value: "", dataset: {}, style: {}, options: [], listeners: {},
		addEventListener(event, callback) { this.listeners[event] = callback; },
		replaceChildren() {}, append() {}, setAttribute() {},
		querySelector(selector) { this.children ??= {}; return this.children[selector] ??= node(); } });
	const nodes = new Map();
	const find = (selector) => {
		if (!nodes.has(selector)) nodes.set(selector, node());
		return nodes.get(selector);
	};
	const panel = { hidden: false, querySelector: find, querySelectorAll: () => [], setAttribute() {} };
	const identity = find("[data-usage-identity]"), device = find("[data-usage-device]");
	find("[data-usage-days]").value = "all";
 identity.value = "all";
	device.value = "all";
 find("[data-usage-provider]").value = "openai";
 find("[data-usage-model]").value = "all";
	device.options = [{ dataset: {} }, { dataset: { identity: "example" } }, { dataset: { identity: "other" } }];
	const urls = [];
	const zero = { responses: 0, unpriced_responses: 0, missing_usage_responses: 0, cost_usd: 0,
		processed_tokens: 0, cached_input: 0, uncached_input: 0, daily: [], since: "2026-01-01", until: "2026-01-01",
		category: { input: 0, cache_read: 0, output: 0, other: 0 },
		speed: { standard: 0, fast: 0, ultrafast: 0, premium: 0 }, pricing: { status: "unavailable" } };
	const context = {
		document: { createElement: node, createElementNS: node },
		fetch: async (url) => { urls.push(url); return { ok: true, json: async () => new URL(url,"https://test").searchParams.has("identity") ? {...zero,ccusage:true,reports:1,processed_tokens:7,output:7,pricing:{status:"ccusage"}} : zero }; } };
	const loadModule = await browser(t, context);
	const { initUsageDashboard } = await loadModule("usage.js");
	const load = initUsageDashboard(panel, (path) => "/broker" + path);
	await load("account1");
	assert.equal(urls.at(-1), "/broker/api/internal/v1/usage?account=account1&days=all&provider=openai&model=all&identity=all&device=all");
	identity.value = "example";
	identity.listeners.change();
	await new Promise(setImmediate);
	assert.equal(device.disabled, false);
	assert.equal(device.options[1].hidden, false);
	assert.equal(device.options[2].hidden, true);
	assert.match(urls.at(-1), /identity=example&device=all$/);
 assert.match(find("[data-usage-caption]").textContent,/1 daily device reports/);
 assert.equal(find("[data-usage-costs]").hidden,true);
 assert.equal(find("[data-usage-cache-savings]").querySelector("dd").textContent,"—");
 assert.match(find("[data-usage-cache-savings]").title,/does not report dollar cache savings/);
	device.value = "laptop";
	device.listeners.change();
	await new Promise(setImmediate);
	assert.match(urls.at(-1), /identity=example&device=laptop$/);
	identity.value = "all";
	identity.listeners.change();
	await new Promise(setImmediate);
	assert.equal(device.value, "all");
	assert.equal(device.options[2].hidden, false);
	identity.value = "";
	identity.listeners.change();
	await new Promise(setImmediate);
	assert.equal(device.disabled, true);
 assert.equal(find("[data-usage-costs]").hidden,false);
	assert.equal(new URL(urls.at(-1), "https://test").searchParams.has("identity"), false);
});

test("streamer preference persists at the root path and masks live labels", async (t) => {
 for (const enabled of [false, true]) await t.test(`streamer ${enabled ? "on" : "off"}`, async (t) => {
  let click, reloads = 0, refresh;
  const node = () => ({ dataset: {}, children: [], append(...children) { this.children.push(...children); }, replaceChildren(...children) { this.children = children; } });
  const nodes = new Map();
  const find = (selector) => { if (!nodes.has(selector)) nodes.set(selector, node()); return nodes.get(selector); };
  const select = find("[data-proxy-model]");
  select.value = "all";
  select.addEventListener = () => {};
  const document = {
   documentElement: { dataset: { rootPath: "/broker", streamerMode: enabled ? "on" : "off" } },
   querySelector: (selector) => selector === "[data-streamer-toggle]"
    ? { addEventListener: (_, callback) => { click = callback; } }
    : selector === "[data-proxy-card]" ? { querySelector: find, querySelectorAll: () => [] } : null,
   createElement: node,
  };
  const context = { document,
   location: { protocol: "https:", reload: () => { reloads++; } },
   setTimeout: (callback) => { refresh = callback; },
   fetch: async () => ({ ok: true, json: async () => ({ ready: true, active: 1, requests: 2, failovers: 0, usage: [], quota_usage: [],
    recent_clients: [{ name: "Work laptop", key_prefix: "cbk_example", active: 1, last_seen: new Date().toISOString() }],
    relays: [{ id: "Example edge", state: "ready", ready: true, connected: true, protocol_version: 5, last_seen: new Date().toISOString() }] }) }) };
  const loadModule = await browser(t, context);
  const { initApp } = await loadModule("app.js");
  const dispose = initApp();
  loadModule.cleanup(dispose);
  click();
  assert.equal(reloads, 1);
  assert.equal(document.cookie, `codex-broker.streamer=${enabled ? "off" : "on"}; Path=/broker; Max-Age=31536000; SameSite=Strict; Secure`);

  for (let iteration = 0; iteration < 2; iteration++) {
   await new Promise(setImmediate);
   assert.equal(find("[data-proxy-clients]").children[0].children[0].textContent, enabled ? "Client 1" : "Work laptop · cbk_example");
   assert.equal(find("[data-proxy-relays]").children[0].children[0].textContent, enabled ? "Relay 1" : "Example edge");
   assert.match(find("[data-proxy-clients]").children[0].children[1].textContent, /1 active/);
   if (!iteration) await refresh();
  }
  dispose();
  let toggles = 0;
  const disposeToggle = initApp({ toggleStreamer: () => { toggles++; } });
  loadModule.cleanup(disposeToggle);
  click();
  assert.equal(toggles, 1, "shell callback handles streamer toggling when supplied");
  assert.equal(reloads, 1, "shell callback must not reload the page");
  await new Promise(setImmediate);
  disposeToggle();
 });
});
