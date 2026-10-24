import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { runInNewContext } from "node:vm";

const source = readFileSync(new URL("../internal/webassets/public_static/enroll.js", import.meta.url), "utf8");

for (const [path, labels] of [
  ["/", []],
  ["/cindy/", ["cindy"]],
  ["/cindy", ["cindy"]],
  ["/Cindy%20Team/", ["Cindy Team"]],
  ["/?labels=cindy,team", ["cindy,team"]],
  ["/cindy/?labels=team&labels=other&unrelated=ignored", ["team", "other", "cindy"]],
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
