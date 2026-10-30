// Tab-local snapshots never cross login sessions or application versions.
const storageKey = "codex-broker.dashboard.cache";
let scope, entries = {};

export function configureCache(value) {
	scope = value;
	try {
		const saved = JSON.parse(sessionStorage.getItem(storageKey) || "null");
		entries = saved?.scope === scope && saved.entries && typeof saved.entries === "object" ? saved.entries : {};
	} catch { entries = {}; }
}

function save() {
	while (Object.keys(entries).length > 32 || JSON.stringify(entries).length * 2 > 4 * 1024 * 1024)
		delete entries[Object.keys(entries)[0]];
	try { sessionStorage.setItem(storageKey, JSON.stringify({ scope, entries })); } catch { /* Storage can be disabled or full. */ }
}

export function clearCache() {
	entries = {};
	try { sessionStorage.removeItem(storageKey); } catch { /* Storage can be disabled. */ }
}

export function invalidateCache() {
	for (const entry of Object.values(entries)) entry.at = 0;
	if (scope) save();
}

export async function cachedRequest(url, { ttl = 5000, force = false, signal, type = "json" } = {}) {
	const key = `${type}:${url}`;
	const entry = scope ? entries[key] : undefined;
	if (!force && entry?.at > 0 && Date.now() - entry.at < ttl && Date.now() < entry.expires) return entry.data;
	const headers = {};
	if (entry?.etag) headers["If-None-Match"] = entry.etag;
	if (force) headers["X-Dashboard-Refresh"] = "true";
	const response = await fetch(url, { signal, headers, cache: "no-store" });
	if (response.status === 401 || response.redirected) {
		clearCache();
		throw Object.assign(new Error("Your session expired. Reload to sign in."), { status: 401 });
	}
	if (!response.ok && response.status !== 304) throw new Error("Data could not be loaded. Try refreshing.");
	const data = response.status === 304 ? entry?.data : await response[type]();
	if (data === undefined) throw new Error("Cached data unavailable. Try refreshing.");
	if (scope) {
		delete entries[key];
		const remaining = Number(response.headers.get("X-Dashboard-TTL") ?? ttl);
		entries[key] = { data, etag: response.headers.get("ETag") || entry?.etag, at: Date.now(), expires: Date.now() + Math.min(ttl, remaining) };
		save();
	}
	return data;
}
