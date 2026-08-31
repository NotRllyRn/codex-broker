const root = document.documentElement;
const rootPath = root.dataset.rootPath || "";
const appPath = (path) => `${rootPath}${path}`;
document.querySelectorAll('[href^="/"], [action^="/"]').forEach((element) => {
	const attribute = element.hasAttribute("href") ? "href" : "action";
	element.setAttribute(attribute, appPath(element.getAttribute(attribute)));
});
const themeButton = document.querySelector("[data-theme-toggle]");
const themes = ["system", "light", "dark"];
const storedTheme = localStorage.getItem("windowkeeper.theme");
root.dataset.theme = themes.includes(storedTheme) ? storedTheme : "system";

function updateThemeLabel() {
	if (themeButton) themeButton.textContent = `Theme: ${root.dataset.theme}`;
}
updateThemeLabel();

themeButton?.addEventListener("click", () => {
	const next = themes[(themes.indexOf(root.dataset.theme) + 1) % themes.length];
	root.dataset.theme = next;
	localStorage.setItem("windowkeeper.theme", next);
	updateThemeLabel();
});

const timestampPattern =
	/\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})|\b(?:\d{13}|\d{10})\b/g;
const timestampNodes = [];
const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
while (walker.nextNode()) {
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

document.querySelectorAll("form[data-confirm]").forEach((form) => {
	form.addEventListener("submit", (event) => {
		if (!window.confirm(form.dataset.confirm)) event.preventDefault();
	});
});

const variantSwitcher = document.querySelector("[data-variant-switcher]");
if (variantSwitcher) {
	const variants = variantSwitcher.dataset.variants.split(",");
	const move = (step) => {
		const current = variants.indexOf(variantSwitcher.dataset.current);
		try {
			const url = new URL(location.href);
			url.searchParams.set(
				"variant",
				variants[(current + step + variants.length) % variants.length],
			);
			location.assign(url);
		} catch {
			location.assign(
				appPath(
					`/?variant=${variants[(current + step + variants.length) % variants.length]}`,
				),
			);
		}
	};
	variantSwitcher
		.querySelector("[data-previous]")
		.addEventListener("click", () => move(-1));
	variantSwitcher
		.querySelector("[data-next]")
		.addEventListener("click", () => move(1));
	document.addEventListener("keydown", (event) => {
		if (event.target.matches("input, textarea, select")) return;
		if (event.key === "[") move(-1);
		if (event.key === "]") move(1);
	});
}

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
			status.textContent = "Enter the one-time code. Windowkeeper never logs it.";
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
