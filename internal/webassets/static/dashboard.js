import { initApp } from "./app.js";
import { cachedRequest, configureCache } from "./cache.js";

const root = document.documentElement;
const rootPath = root.dataset.rootPath || "";
const shell = document.querySelector("[data-dashboard-shell]");
configureCache(shell.dataset.cacheScope);
let dispose, loading = false;

async function load({ force = false, toggle = false } = {}) {
	if (loading) return;
	loading = true;
	const previous = root.dataset.streamerMode;
	const mode = toggle ? previous === "on" ? "off" : "on" : previous;
	const selections = [...document.querySelectorAll("select")].map((node) => [node.getAttributeNames().find((name) => name.startsWith("data-")), node.value]).filter(([name]) => name);
	// Hide the current view before fetching a private view, including on errors.
	if (toggle && mode === "on") {
		document.querySelector("#main").hidden = true;
		const params = new URLSearchParams(location.search);
		params.delete("q");
		history.replaceState(null, "", `${location.pathname}${params.size ? `?${params}` : ""}`);
	}
	document.cookie = `codex-broker.streamer=${mode}; Path=${rootPath || "/"}; Max-Age=31536000; SameSite=Strict${location.protocol === "https:" ? "; Secure" : ""}`;
	try {
		const html = await cachedRequest(`${rootPath}/api/internal/v1/dashboard/view?state_filter=&mode=${mode}`, { type: "text", ttl: 5000, force });
		const page = new DOMParser().parseFromString(html, "text/html");
		if (!page.querySelector("[data-account-filters]")) throw new Error("Dashboard unavailable. Try again.");
		dispose?.();
		root.dataset.streamerMode = mode;
		document.body.replaceWith(page.body);
		for (const [name, value] of selections) {
			const select = document.querySelector(`[${name}]`);
			if (select && [...select.options].some((option) => option.value === value)) select.value = value;
		}
		dispose = initApp({ refreshDashboard: () => load({ force: true }), toggleStreamer: () => load({ toggle: true }), forceRefresh: force });
	} catch (error) {
		if (error.status === 401) { dispose?.(); location.assign(`${rootPath}/login`); return; }
		// Keep private mode hidden if loading it failed; retry stays available.
		if (!toggle || mode !== "on") document.cookie = `codex-broker.streamer=${previous}; Path=${rootPath || "/"}; SameSite=Strict`;
		const message = document.createElement("p");
		message.setAttribute("role", "alert");
		message.textContent = error.message;
		const retry = document.createElement("button");
		retry.textContent = "Try again";
		retry.addEventListener("click", () => { message.remove(); retry.remove(); load({ force: true, toggle }); });
		document.body.append(message, retry);
	} finally { loading = false; }
}

load();
