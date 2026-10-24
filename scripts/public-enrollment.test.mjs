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
