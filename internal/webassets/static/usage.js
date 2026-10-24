const compact = new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 2 });
const usd = new Intl.NumberFormat(undefined, { style: "currency", currency: "USD" });
const dateLabel = (day) => new Date(`${day}T00:00:00Z`).toLocaleDateString(undefined, { month: "short", day: "numeric", timeZone: "UTC" });

export function cachedInputShare(data) {
	const input = data.cached_input + data.uncached_input;
	return input > 0 ? data.cached_input / input : null;
}

export function rankedModels(models, metric) {
	const value = (model) => metric === "cost" ? (model.cost_known ? model.cost_usd : -1) : model.processed_tokens;
	return [...models].sort((a, b) => value(b) - value(a) || a.name.localeCompare(b.name));
}

export function initUsageDashboard(panel, appPath) {
	if (panel.hidden) return () => {};
	const find = (name) => panel.querySelector(`[data-usage-${name}]`);
	const content = find("content");
	const status = find("status");
	const period = find("days");
	const provider = find("provider"), model = find("model");
	const identity = find("identity"), device = find("device");
	let account = "all", metric = "cost", data, controller, expandedModels = false;

	function renderChart() {
		const cost = metric === "cost";
		const unpriced = cost && data.responses > 0 && data.responses === data.unpriced_responses + data.missing_usage_responses;
		const field = cost ? "cost_usd" : "processed_tokens";
		const format = cost ? (v) => usd.format(v) : (v) => compact.format(v);
		const chart = find("chart");
		const title = cost ? "Daily cost" : "Daily processed tokens";
		find("chart-title").textContent = title;
		chart.setAttribute("aria-label", `${title}, ${dateLabel(data.since)} to ${dateLabel(data.until)}, UTC`);
		find("headline").textContent = cost ? costValue(data.cost_usd) : compact.format(data.processed_tokens);
		find("caption").textContent = `${(data.ccusage ? data.reports : data.responses).toLocaleString()} ${data.ccusage ? "daily device reports" : "responses"} · ${cost ? "API estimate" : "processed tokens"}`;
		chart.replaceChildren();
		const peak = Math.max(0, ...data.daily.map((day) => day[field]));
		const maximum = peak || 1;
		const axis = find("axis");
		axis.replaceChildren(...[peak, peak / 2, 0].map((value) => {
			const label = document.createElement("span");
			label.textContent = unpriced ? "—" : format(value);
			return label;
		}));
		const svg = (tag, attributes) => {
			const node = document.createElementNS("http://www.w3.org/2000/svg", tag);
			for (const [key, value] of Object.entries(attributes)) node.setAttribute(key, String(value));
			chart.append(node);
			return node;
		};
		for (const y of [10, 110, 210]) svg("line", { x1: 10, x2: 990, y1: y, y2: y, class: "usage-grid-line" });
		const points = data.daily.map((day, index) => [10 + index * 980 / Math.max(1, data.daily.length - 1), 210 - 200 * day[field] / maximum]);
		if (points.length && !unpriced) {
			svg("polygon", { points: `${points[0][0]},210 ${points.map((p) => p.join(",")).join(" ")} ${points.at(-1)[0]},210`, class: "usage-chart-area" });
			svg("polyline", { points: points.map((p) => p.join(",")).join(" "), class: "usage-chart-line", "vector-effect": "non-scaling-stroke" });
		}
		data.daily.forEach((day, index) => {
			if (unpriced) return;
			const missing = day.unpriced_responses + day.missing_usage_responses;
			const dayUnpriced = cost && day.responses > 0 && day.responses === missing;
			const message = `${dateLabel(day.day)} (UTC): ${dayUnpriced ? "Unpriced" : `${format(day[field])}${cost ? " estimated" : " tokens"}`}${missing ? ` · ${missing} responses without cost estimates` : ""}`;
			const point = svg("circle", { cx: points[index][0], cy: points[index][1], r: 6, tabindex: 0, role: "img", "aria-label": message, class: "usage-chart-point" });
			const label = document.createElementNS("http://www.w3.org/2000/svg", "title");
			label.textContent = message;
			point.append(label);
			for (const event of ["mouseenter", "focus"]) point.addEventListener(event, () => { find("chart-detail").textContent = message; });
		});
		find("dates").replaceChildren(...[data.since, data.daily[Math.floor(data.daily.length / 2)]?.day, data.until].filter(Boolean).map((day) => {
			const label = document.createElement("span");
			label.textContent = dateLabel(day);
			return label;
		}));
		find("chart-detail").textContent = unpriced ? "No cost estimates are available for this period. Switch to Tokens to see recorded usage." : (data.responses || data.reports) ? "Hover or focus a point to see a day." : "No recorded usage in this period.";
	}

	function costValue(value) {
		return data.responses > 0 && data.responses === data.unpriced_responses + data.missing_usage_responses ? "—" : usd.format(value);
	}

	function renderBar(kind, segments) {
		const bar = find(`${kind}-bar`), legend = find(`${kind}-legend`);
		bar.replaceChildren();
		legend.replaceChildren();
		const total = segments.reduce((sum, segment) => sum + segment[1], 0);
		for (const [name, value, color] of segments) {
			if (value > 0 && total > 0) {
				const segment = document.createElement("span");
				segment.style.width = `${100 * value / total}%`;
				segment.style.background = color;
				bar.append(segment);
			}
			const row = document.createElement("span"), swatch = document.createElement("i"), amount = document.createElement("strong");
			swatch.style.background = color;
			amount.textContent = costValue(value);
			row.append(swatch, `${name} `, amount);
			legend.append(row);
		}
	}

	function renderModels() {
		const models = rankedModels(data.models || [], metric);
		const cost = metric === "cost", total = cost ? data.cost_usd : data.processed_tokens;
		const rows = expandedModels ? models : models.slice(0, 5);
		find("model-share").textContent = cost ? "Cost share" : "Token share";
		find("model-caption").textContent = models.length ? `${rows.length} of ${models.length} models · ranked by ${cost ? "cost" : "tokens"}` : "";
		find("model-empty").hidden = models.length > 0;
		find("model-rows").replaceChildren(...rows.map((model, index) => {
			const row = document.createElement("tr");
			const value = cost ? model.cost_usd : model.processed_tokens;
			const share = total > 0 && (!cost || model.cost_known) ? value / total : null;
			const cells = [String(index + 1), model.name, model.cost_known ? usd.format(model.cost_usd) : "—", share === null ? "—" : share > 0 && share < 0.001 ? "<0.1%" : `${(share * 100).toFixed(1)}%`, compact.format(model.processed_tokens)];
			for (const [position, text] of cells.entries()) {
				const cell = document.createElement(position === 1 ? "th" : "td");
				cell.textContent = text;
				if (position === 1) {
					cell.scope = "row";
					const bar = document.createElement("span");
					bar.className = "usage-model-bar";
					bar.setAttribute("aria-hidden", "true");
					bar.style.width = `${Math.min(100, Math.max(0, (share || 0) * 100))}%`;
					cell.append(bar);
				}
				row.append(cell);
			}
			return row;
		}));
		const more = find("model-more");
		more.hidden = models.length <= 5;
		more.textContent = expandedModels ? "Show less" : `Show more (${models.length - 5})`;
		more.setAttribute("aria-expanded", String(expandedModels));
	}

	function render() {
		for (const node of panel.querySelectorAll("[data-usage-total]")) {
			const key = node.dataset.usageTotal;
			node.textContent = key === "cache_savings_usd" ? costValue(data[key]) : compact.format(data[key]);
		}
		const category = data.category, speed = data.speed;
		find("costs").hidden = Boolean(data.ccusage);
		const savings = find("cache-savings");
		savings.querySelector("dd").textContent = data.ccusage ? "—" : costValue(data.cache_savings_usd);
		savings.title = data.ccusage ? "ccusage does not report dollar cache savings or the pricing detail needed to derive them reliably." : "Estimated cost avoided by reading cached input at the cached rate.";
		const cacheRate = cachedInputShare(data);
		find("cache-rate").textContent = cacheRate === null ? "—" : `${(cacheRate * 100).toFixed(1)}%`;
		find("cache-rate").title = "Cached input / (cached + uncached input, including cache writes). This is a token share, not a dollar savings rate.";
		renderBar("type", [["Input", category.input, "var(--usage-input)"], ["Cache read", category.cache_read, "var(--usage-cache)"], ["Output", category.output, "var(--ink)"], ["Other", category.other, "var(--line-strong)"]]);
		renderBar("speed", [["Standard", speed.standard, "var(--usage-cache)"], ["Fast", speed.fast, "var(--usage-input)"], ...(speed.ultrafast > 0 ? [["Ultrafast", speed.ultrafast, "var(--ink)"]] : [])]);
		find("premium").textContent = costValue(speed.premium);

		status.textContent = (data.responses || data.reports) ? "" : "No recorded usage in this period. Set up identities and devices in Settings to collect client usage.";
		content.hidden = false;
		renderChart();
		renderModels();
	}

	async function load(nextAccount = account) {
		account = nextAccount;
		controller?.abort();
		const current = new AbortController();
		controller = current;
		content.hidden = true;
		panel.setAttribute("aria-busy", "true");
		status.textContent = "Loading recorded usage…";
		try {
			const query = new URLSearchParams({ account, days: period.value });
			if (provider) query.set("provider", provider.value);
			if (model) query.set("model", model.value);
			if (identity?.value) { query.set("identity", identity.value); query.set("device", device.value); }
			const response = await fetch(appPath(`/api/internal/v1/usage?${query}`), { signal: current.signal });
			if (!response.ok) throw new Error("Usage could not be loaded. Try refreshing usage.");
			const result = await response.json();
			if (current.signal.aborted) return;
			data = result;
			if (model) {
				const selected = model.value;
				model.replaceChildren(...["all", ...(data.available_models || [])].map(name => { const option = document.createElement("option"); option.value = name; option.textContent = name === "all" ? "All models" : name; return option; }));
				model.value = (data.available_models || []).includes(selected) ? selected : "all";
				if (selected !== model.value) { return await load(); }
			}
			render();
		} catch (error) {
			if (!current.signal.aborted) status.textContent = error.message;
		} finally {
			if (!current.signal.aborted) panel.setAttribute("aria-busy", "false");
		}
	}
	for (const button of panel.querySelectorAll("[data-usage-metric]")) button.addEventListener("click", () => {
		metric = button.dataset.usageMetric;
		for (const option of panel.querySelectorAll("[data-usage-metric]")) option.setAttribute("aria-pressed", String(option === button));
		if (data) { renderChart(); renderModels(); }
	});
	find("model-more").addEventListener("click", () => { expandedModels = !expandedModels; renderModels(); });
	provider?.addEventListener("change", () => { model.value = "all"; expandedModels = false; load(); });
	model?.addEventListener("change", () => { expandedModels = false; load(); });
	period.addEventListener("change", () => load());
	identity?.addEventListener("change", () => {
		device.value = "all";
		device.disabled = !identity.value;
		for (const option of device.options) option.hidden = Boolean(option.dataset.identity && identity.value !== "all" && option.dataset.identity !== identity.value);
		load();
	});
	device?.addEventListener("change", () => load());
	find("refresh").addEventListener("click", () => load());
	return load;
}
