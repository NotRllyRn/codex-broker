const compact = new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 2 });
const usd = new Intl.NumberFormat(undefined, { style: "currency", currency: "USD" });
const dateLabel = (day) => new Date(`${day}T00:00:00Z`).toLocaleDateString(undefined, { month: "short", day: "numeric", timeZone: "UTC" });

export function initUsageDashboard(panel, appPath) {
	const find = (name) => panel.querySelector(`[data-usage-${name}]`);
	const content = find("content");
	const status = find("status");
	const period = find("days");
	let account = "all", metric = "cost", data, controller;

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
		find("caption").textContent = `${data.responses.toLocaleString()} responses · ${cost ? "API estimate" : "processed tokens"}`;
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
		find("chart-detail").textContent = unpriced ? "No cost estimates are available for this period. Switch to Tokens to see recorded usage." : data.responses ? "Hover or focus a point to see a day." : "No recorded proxy responses in this period.";
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

	function render() {
		for (const node of panel.querySelectorAll("[data-usage-total]")) {
			const key = node.dataset.usageTotal;
			node.textContent = key === "cache_savings_usd" ? costValue(data[key]) : compact.format(data[key]);
		}
		const category = data.category, speed = data.speed;
		renderBar("type", [["Input", category.input, "var(--usage-input)"], ["Cache read", category.cache_read, "var(--usage-cache)"], ["Output", category.output, "var(--ink)"], ["Other", category.other, "var(--line-strong)"]]);
		renderBar("speed", [["Standard", speed.standard, "var(--usage-cache)"], ["Fast", speed.fast, "var(--usage-input)"], ...(speed.ultrafast > 0 ? [["Ultrafast", speed.ultrafast, "var(--ink)"]] : [])]);
		find("premium").textContent = costValue(speed.premium);
		const notices = ["Recorded proxy traffic only; direct leases and historical profile tokens are excluded."];
		if (data.history_since) notices.push(`Daily history available from ${data.history_since}.`);
		if (data.unpriced_responses) notices.push(`${data.unpriced_responses.toLocaleString()} responses have no model/tier price and are excluded from costs.`);
		if (data.missing_usage_responses) notices.push(`${data.missing_usage_responses.toLocaleString()} responses have incomplete token usage and are excluded from costs.`);
		if (data.pricing.status === "unavailable") notices.push("Model pricing is unavailable; unknown costs are shown as —.");
		else notices.push(`LiteLLM ${data.pricing.status === "cached" ? "cached " : ""}list rates from ${new Date(data.pricing.fetched_at).toLocaleDateString()}.`);
		find("coverage").textContent = notices.join(" ");
		status.textContent = data.responses ? "" : "No recorded proxy usage in this period. Account-wide profile activity is shown below.";
		content.hidden = false;
		renderChart();
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
			const response = await fetch(appPath(`/api/internal/v1/usage?${query}`), { signal: current.signal });
			if (!response.ok) throw new Error("Usage could not be loaded. Try refreshing usage.");
			const result = await response.json();
			if (current.signal.aborted) return;
			data = result;
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
		if (data) renderChart();
	});
	period.addEventListener("change", () => load());
	find("refresh").addEventListener("click", () => load());
	return load;
}
