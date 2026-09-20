const root = document.documentElement;
const rootPath = root.dataset.rootPath || "";
const appPath = (path) => `${rootPath}${path}`;
document.querySelectorAll('[href^="/"], [action^="/"]').forEach((element) => {
	const attribute = element.hasAttribute("href") ? "href" : "action";
	element.setAttribute(attribute, appPath(element.getAttribute(attribute)));
});
const themeButton = document.querySelector("[data-theme-toggle]");
const themes = ["system", "light", "dark"];
const storedTheme = localStorage.getItem("codex-broker.theme");
root.dataset.theme = themes.includes(storedTheme) ? storedTheme : "system";

function updateThemeLabel() {
	if (themeButton) themeButton.textContent = `Theme: ${root.dataset.theme}`;
}
updateThemeLabel();

themeButton?.addEventListener("click", () => {
	const next = themes[(themes.indexOf(root.dataset.theme) + 1) % themes.length];
	root.dataset.theme = next;
	localStorage.setItem("codex-broker.theme", next);
	updateThemeLabel();
});

const timestampPattern =
	/\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})|\b(?:\d{13}|\d{10})\b/g;
const timestampNodes = [];
const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
while (walker.nextNode()) {
	if (walker.currentNode.parentElement?.closest("script, style")) continue;
	timestampPattern.lastIndex = 0;
	if (timestampPattern.test(walker.currentNode.data))
		timestampNodes.push([walker.currentNode, walker.currentNode.data]);
}

function relativeTime(value) {
	const milliseconds = /^\d{10}$/.test(value)
		? Number(value) * 1000
		: /^\d{13}$/.test(value)
			? Number(value)
			: Date.parse(value);
	if (!Number.isFinite(milliseconds)) return value;
	const difference = milliseconds - Date.now();
	const duration = Math.abs(difference);
	const [amount, unit] =
		duration >= 86_400_000
			? [Math.ceil(duration / 86_400_000), "day"]
			: duration >= 3_600_000
				? [Math.ceil(duration / 3_600_000), "hour"]
				: [Math.max(1, Math.ceil(duration / 60_000)), "minute"];
	return `${amount} ${unit}${amount === 1 ? "" : "s"} ${difference >= 0 ? "left" : "ago"}`;
}

function updateRelativeTimes() {
	for (const [node, source] of timestampNodes)
		node.data = source.replace(timestampPattern, relativeTime);
}
updateRelativeTimes();
setInterval(updateRelativeTimes, 60_000);

document
	.querySelector("[data-refresh-page]")
	?.addEventListener("click", () => location.reload());

const profileDashboard = document.querySelector("[data-profile-dashboard]");
if (profileDashboard) {
	const profiles = JSON.parse(
		profileDashboard.querySelector("[data-profile-data]").textContent || "[]",
	);
	const select = profileDashboard.querySelector("[data-profile-select]");
	const compact = new Intl.NumberFormat(undefined, {
		notation: "compact",
		maximumFractionDigits: 1,
	});
	const integer = new Intl.NumberFormat();
	const colors = ["#2678d9", "#36a58b", "#8b6fd6", "#e18b38", "#d65370", "#6b8e23", "#2b9eb3", "#b76e9b"];
	const value = (number, suffix = "") =>
		number == null ? "—" : `${compact.format(number)}${suffix}`;
	const duration = (seconds) => {
		if (seconds == null) return "—";
		const hours = Math.floor(seconds / 3600);
		const minutes = Math.floor((seconds % 3600) / 60);
		return hours ? `${hours}h ${minutes}m` : `${minutes}m`;
	};
	const percentage = (number) =>
		number == null ? "—" : `${Number(number).toFixed(number % 1 ? 1 : 0)}%`;

	function renderActivity(profile) {
		const grid = profileDashboard.querySelector("[data-activity-grid]");
		const months = profileDashboard.querySelector("[data-activity-months]");
		grid.replaceChildren();
		months.replaceChildren();
		const byDate = new Map(profile.daily_usage_buckets.map((item) => [item.start_date, item.tokens]));
		const end = new Date();
		end.setUTCHours(0, 0, 0, 0);
		const start = new Date(end);
		start.setUTCDate(start.getUTCDate() - 181);
		start.setUTCDate(start.getUTCDate() - start.getUTCDay());
		const values = [...byDate.values()];
		const max = Math.max(1, ...values);
		let lastMonth = -1;
		for (let date = new Date(start); date <= end; date.setUTCDate(date.getUTCDate() + 1)) {
			const key = date.toISOString().slice(0, 10);
			const tokens = byDate.get(key) || 0;
			const cell = document.createElement("span");
			const level = tokens ? Math.max(1, Math.ceil((tokens / max) * 4)) : 0;
			cell.dataset.level = String(level);
			cell.title = `${key}: ${integer.format(tokens)} tokens`;
			grid.append(cell);
			if (date.getUTCDay() === 0 && date.getUTCMonth() !== lastMonth) {
				const month = document.createElement("span");
				month.textContent = date.toLocaleString(undefined, { month: "short", timeZone: "UTC" });
				month.style.gridColumn = String(Math.floor((date - start) / 604800000) + 1);
				months.append(month);
				lastMonth = date.getUTCMonth();
			}
		}
	}

	function renderTokenShare(profile) {
		const chart = profileDashboard.querySelector("[data-token-share-chart]");
		const legend = profileDashboard.querySelector("[data-token-share-legend]");
		const shares = profile.token_shares.filter((item) => item.tokens > 0);
		const total = shares.reduce((sum, item) => sum + item.tokens, 0);
		let cursor = 0;
		const segments = shares.map((item, index) => {
			const start = cursor;
			cursor += (item.tokens / total) * 100;
			return `${colors[index % colors.length]} ${start}% ${cursor}%`;
		});
		chart.style.background = segments.length ? `conic-gradient(${segments.join(",")})` : "var(--surface-2)";
		chart.querySelector("[data-token-share-total]").textContent = value(total);
		legend.replaceChildren();
		shares.forEach((item, index) => {
			const row = document.createElement("div");
			const swatch = document.createElement("i");
			const label = document.createElement("span");
			const amount = document.createElement("strong");
			swatch.style.background = colors[index % colors.length];
			label.textContent = item.label;
			amount.textContent = value(item.tokens);
			row.append(swatch, label, amount);
			legend.append(row);
		});
	}

	function renderProfile() {
		const profile = profiles.find((item) => item.id === select.value) || profiles[0];
		if (!profile) return;
		profileDashboard.querySelector("[data-profile-subtitle]").textContent = profile.username
			? `@${profile.username}`
			: profile.id === "all"
				? "Combined statistics for every cached account."
				: "Cached account statistics";
		const stats = {
			lifetime_tokens: value(profile.lifetime_tokens),
			peak_daily_tokens: value(profile.peak_daily_tokens),
			current_streak_days: value(profile.current_streak_days, " days"),
			longest_streak_days: value(profile.longest_streak_days, " days"),
			longest_running_turn_sec: duration(profile.longest_running_turn_sec),
			fast_mode_usage_percentage: percentage(profile.fast_mode_usage_percentage),
			most_used_reasoning_effort: profile.most_used_reasoning_effort
				? `${profile.most_used_reasoning_effort} · ${percentage(profile.most_used_reasoning_effort_percentage)}`
				: "—",
			unique_skills_used: value(profile.unique_skills_used),
			total_skills_used: value(profile.total_skills_used),
			total_threads: value(profile.total_threads),
		};
		for (const [key, formatted] of Object.entries(stats))
			profileDashboard.querySelectorAll(`[data-profile-stat="${key}"]`).forEach((node) => (node.textContent = formatted));
		const freshness = profileDashboard.querySelector("[data-profile-freshness]");
		freshness.textContent = profile.updated_at_ms
			? `${profile.stale ? "Cached" : "Updated"} ${relativeTime(String(profile.updated_at_ms))}`
			: "No cached profile data yet.";
		renderActivity(profile);
		renderTokenShare(profile);
	}

	select?.addEventListener("change", renderProfile);
	renderProfile();
}

document.querySelectorAll("form[data-confirm]").forEach((form) => {
	form.addEventListener("submit", (event) => {
		if (!window.confirm(form.dataset.confirm)) event.preventDefault();
	});
});

function toast(message) {
	const region = document.querySelector(".toast-region");
	if (!region) return;
	const item = document.createElement("div");
	item.className = "toast";
	item.textContent = message;
	region.append(item);
	setTimeout(() => item.remove(), 5000);
}

if (document.querySelector(".topbar")) {
	const stream = new EventSource(appPath("/api/internal/v1/events/state"));
	[
		"account.updated",
		"operation.updated",
		"incident.updated",
		"login.updated",
	].forEach((name) => {
		stream.addEventListener(name, () =>
			toast("Committed state changed. Refresh when ready."),
		);
	});
	stream.addEventListener("gap", () =>
		toast("Live updates skipped. Refresh for current state."),
	);
}

const operation = document.querySelector("[data-operation]");
if (operation) {
	const id = operation.dataset.operation;
	const poll = async () => {
		const response = await fetch(appPath(`/api/internal/v1/operations/${id}`), {
			headers: { Accept: "application/json" },
		});
		if (!response.ok) return;
		const payload = await response.json();
		if (
			["SUCCEEDED", "FAILED", "CANCELLED", "AMBIGUOUS"].includes(
				payload.data.state,
			)
		) {
			location.reload();
		} else {
			setTimeout(poll, 1200);
		}
	};
	setTimeout(poll, 1200);
}

const loginProgress = document.querySelector("[data-login-progress]");
if (loginProgress) {
	const attempt = loginProgress.dataset.attempt;
	const account = loginProgress.dataset.account;
	const nonce = loginProgress.dataset.nonce;
	const csrf = loginProgress.dataset.csrf;
	const status = loginProgress.querySelector(".interaction-status");
	const loading = loginProgress.querySelector("[data-loading]");
	let interaction;

	const showInteraction = (data) => {
		interaction = data;
		loading.hidden = true;
		if (data.method === "CHATGPT_DEVICE_CODE") {
			const panel = loginProgress.querySelector("[data-device]");
			panel.hidden = false;
			panel.querySelector("[data-verification]").href = data.verification_url;
			panel.querySelector("[data-code]").textContent = data.user_code;
			status.textContent = "Enter the one-time code. Codex Broker never logs it.";
		} else {
			const panel = loginProgress.querySelector("[data-browser]");
			panel.hidden = false;
			panel.querySelector("[data-authorization]").href = data.authorization_url;
			status.textContent = "Authorize the account in a separate browser tab.";
		}
	};

	const pollInteraction = async () => {
		const response = await fetch(
			appPath(`/api/internal/v1/login-attempts/${attempt}/interaction`),
			{
				headers: { "X-Interaction-Nonce": nonce, Accept: "application/json" },
				cache: "no-store",
			},
		);
		if (response.status === 404) {
			setTimeout(pollInteraction, 800);
			return;
		}
		if (!response.ok) {
			status.textContent =
				"Sign-in could not start. Return to the dashboard and try again.";
			loading.hidden = true;
			return;
		}
		const payload = await response.json();
		showInteraction(payload.data);
	};

	loginProgress
		.querySelector("[data-copy-code]")
		?.addEventListener("click", async () => {
			await navigator.clipboard.writeText(interaction.user_code);
			toast("Code copied.");
		});

	loginProgress
		.querySelector("[data-forward]")
		?.addEventListener("click", async (event) => {
			const button = event.currentTarget;
			const callbackUrl = loginProgress
				.querySelector("#callback_url")
				.value.trim();
			if (!callbackUrl) {
				status.textContent = "Paste the complete localhost callback URL first.";
				return;
			}
			button.disabled = true;
			const response = await fetch(
				appPath(`/api/internal/v1/login-attempts/${attempt}/browser-callback`),
				{
					method: "POST",
					headers: {
						"Content-Type": "application/json",
						"X-CSRF-Token": csrf,
						"X-Interaction-Nonce": nonce,
					},
					body: JSON.stringify({ callback_url: callbackUrl }),
				},
			);
			loginProgress.querySelector("#callback_url").value = "";
			if (response.ok) {
				status.textContent =
					"Callback forwarded. Verifying the account and credential bundle.";
			} else {
				const payload = await response.json();
				status.textContent =
					payload.detail || "The callback could not be forwarded.";
				button.disabled = false;
			}
		});

	loginProgress
		.querySelector("[data-cancel-login]")
		?.addEventListener("click", async (event) => {
			event.currentTarget.disabled = true;
			await fetch(appPath(`/api/internal/v1/login-attempts/${attempt}/cancel`), {
				method: "POST",
				headers: { "X-CSRF-Token": csrf },
			});
			location.assign(appPath("/"));
		});

	const events = new EventSource(appPath("/api/internal/v1/events/state"));
	events.addEventListener("login.updated", (event) => {
		let update;
		try {
			update = JSON.parse(event.data);
		} catch {
			return;
		}
		if (update.attempt_id !== attempt) return;
		if (update.state === "FORKING_CREDENTIALS") {
			status.textContent = "Creating managed and downloadable credentials.";
			loginProgress
				.querySelectorAll("[data-device], [data-browser]")
				.forEach((panel) => (panel.hidden = true));
			loading.hidden = false;
		}
		if (update.state === "COMPLETED")
			location.assign(appPath(`/accounts/${account}`));
		if (
			[
				"FAILED_RETRYABLE",
				"FAILED_ACTION_REQUIRED",
				"RESTART_REQUIRED",
				"CANCELLED",
			].includes(update.state)
		) {
			status.textContent = `Sign-in stopped: ${update.error_code || update.state}.`;
			loading.hidden = true;
			events.close();
		}
	});
	pollInteraction();
}
