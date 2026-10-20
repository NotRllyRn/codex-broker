import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { runInNewContext } from "node:vm";

test("shared model selector updates rolling/lifetime tokens and quota without fallback", async () => {
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
	const script = readFileSync(new URL("../internal/webassets/static/app.js", import.meta.url), "utf8");
	runInNewContext(script.slice(script.indexOf("const proxyCard =")), {
		document: { querySelector: () => card, createElement: () => ({ dataset: {} }) },
		Intl, Date, appPath: (path) => path,
		fetch: async () => ({ ok: true, json: async () => data }),
		setTimeout: (callback) => { refresh = callback; },
	});
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
