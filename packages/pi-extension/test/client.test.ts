import assert from "node:assert/strict";
import test from "node:test";

import { BrokerClient, failureKind } from "../src/client.js";

test("requires TLS", () => {
  assert.throws(() => new BrokerClient("http://broker", "key", "ca", "cert", "file"), /HTTPS/);
});

test("classifies only retryable failures", () => {
  assert.equal(failureKind(401), "auth");
  assert.equal(failureKind(403), "auth");
  assert.equal(failureKind(429), "quota");
  assert.equal(failureKind(500, "usage limit reached"), "quota");
  assert.equal(failureKind(500, "upstream unavailable"), undefined);
});
