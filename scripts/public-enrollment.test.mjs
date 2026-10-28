import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { runInNewContext } from "node:vm";

const source = readFileSync(new URL("../internal/webassets/public_static/enroll.js", import.meta.url), "utf8");

for (const [path, labels] of [
  ["/", []],
  ["/label/", ["label"]],
  ["/label", ["label"]],
  ["/Label%20Another-label/", ["Label Another-label"]],
  ["/?labels=label,another-label", ["label,another-label"]],
  ["/label/?labels=another-label&labels=other&unrelated=ignored", ["another-label", "other", "label"]],
]) {
  test(`enrollment link ${path}`, () => {
    const location = new URL(path, "https://enroll.example.test");
    const root = { querySelector: () => ({ addEventListener() {} }) };
    let requested;
    runInNewContext(source, {
      document: { querySelector: () => root },
      location,
      URLSearchParams,
      fetch: (url, options) => {
        requested = new URL(url, location);
        assert.equal(options.method, "POST");
        return new Promise(() => {});
      },
    });
    assert.equal(requested.pathname, "/start");
    assert.deepEqual(requested.searchParams.getAll("labels"), labels);
    assert.equal(requested.searchParams.has("unrelated"), false);
  });
}

test("duplicate enrollment displays the specific error", async () => {
  const title = {}, message = {}, detail = {};
  const error = { querySelector: (selector) => selector === "h2" ? title : selector === "p" ? message : detail };
  const nodes = { "[data-error]": error };
  const root = { querySelector: (selector) => nodes[selector] ??= { addEventListener() {} } };
  await runInNewContext(`${source}\npoll()`, {
    document: { querySelector: () => root },
    location: new URL("https://enroll.example.test/"),
    URLSearchParams,
    fetch: async (url) => url.startsWith("/start")
      ? new Promise(() => {})
      : { json: async () => ({ state: "FAILED", error_code: "PUBLIC_ACCOUNT_EXISTS" }) },
  });
  assert.equal(error.hidden, false);
  assert.equal(title.textContent, "Account already exists");
  assert.equal(message.textContent, "This ChatGPT account is already connected to Codex Broker or is being added.");
});

for (const summary of ["device auth failed with status 429 Too Many Requests", "Token exchange failed", "<script>alert(1)</script>", ""]) {
  test(`generic enrollment error: ${summary}`, async () => {
    const title = {}, message = {}, detail = {};
    Object.defineProperty(detail, "innerHTML", { set() { throw new Error("HTML must never be used"); } });
    const error = { querySelector: (selector) => selector === "h2" ? title : selector === "p" ? message : detail };
    const nodes = { "[data-error]": error };
    const root = { querySelector: (selector) => nodes[selector] ??= { addEventListener() {} } };
    await runInNewContext(`${source}\npoll()`, {
      document: { querySelector: () => root }, location: new URL("https://enroll.example.test/"), URLSearchParams,
      fetch: async (url) => url.startsWith("/start") ? new Promise(() => {}) : { json: async () => ({ state: "FAILED", error_summary: summary, error_code: "LOGIN_DENIED" }) },
    });
    assert.equal(error.hidden, false);
    assert.equal(title.textContent, "Sign-in did not complete");
    assert.equal(detail.textContent, summary || "LOGIN_DENIED");
    assert.equal(detail.hidden, false);
  });
}

test("processing replaces sign-in instructions until enrollment completes", async () => {
  const message = {};
  const loading = { hidden: true, querySelector: () => message };
  const steps = { hidden: false }, success = { hidden: true }, email = {};
  const nodes = { "[data-loading]": loading, "[data-steps]": steps, "[data-success]": success, "[data-email]": email };
  const root = { querySelector: (selector) => nodes[selector] ??= { addEventListener() {} } };
  let nextPoll;
  const states = [{ state: "PROCESSING" }, { state: "COMPLETED", email: "user@example.com" }];
  await runInNewContext(`${source}\npoll()`, {
    document: { querySelector: () => root }, location: new URL("https://enroll.example.test/"), URLSearchParams,
    fetch: async (url) => url.startsWith("/start") ? new Promise(() => {}) : { json: async () => states.shift() },
    setTimeout: (callback) => { nextPoll = callback; },
  });
  assert.equal(steps.hidden, true);
  assert.equal(loading.hidden, false);
  assert.equal(success.hidden, true);
  assert.equal(message.textContent, "Sign-in received—finishing enrollment…");
  await nextPoll();
  assert.equal(loading.hidden, true);
  assert.equal(success.hidden, false);
  assert.equal(email.textContent, "user@example.com");
});
