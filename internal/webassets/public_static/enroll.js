const root = document.querySelector("[data-enrollment]");
const loading = root?.querySelector("[data-loading]");
const steps = root?.querySelector("[data-steps]");
const success = root?.querySelector("[data-success]");
const error = root?.querySelector("[data-error]");
let code = "";

function show(node) {
	for (const item of [loading, steps, success, error])
		if (item) item.hidden = item !== node;
}

async function poll() {
	try {
		const response = await fetch("/status", {
			headers: { Accept: "application/json" },
			cache: "no-store",
		});
		const status = await response.json();
		if (status.state === "STARTING") {
			setTimeout(poll, 800);
			return;
		}
		if (status.state === "WAITING_FOR_USER") {
			code = status.user_code;
			root.querySelector("[data-code]").textContent = code;
			const link = root.querySelector("[data-link]");
			link.href = status.verification_url;
			link.textContent = status.verification_url;
			show(steps);
			setTimeout(poll, 1500);
			return;
		}
		if (status.state === "COMPLETED") {
			root.querySelector("[data-email]").textContent = status.email;
			show(success);
			return;
		}
		if (status.state === "UNAVAILABLE") {
			setTimeout(poll, 3000);
			return;
		}
		show(error);
	} catch {
		setTimeout(poll, 3000);
	}
}

root?.querySelector("[data-copy]")?.addEventListener("click", async (event) => {
	await navigator.clipboard.writeText(code);
	event.currentTarget.querySelector("span").textContent = "Copied";
});

async function start() {
	try {
		const response = await fetch("/start", { method: "POST", cache: "no-store" });
		if (response.ok) poll();
		else show(error);
	} catch {
		setTimeout(start, 3000);
	}
}

if (root && loading) start();
