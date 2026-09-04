import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { createServer } from "node:https";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { BrokerClient, failureKind } from "../src/client.js";

test("requires TLS", () => {
  assert.throws(() => new BrokerClient("http://broker", "key"), /HTTPS/);
});

test("classifies only retryable failures", () => {
  assert.equal(failureKind(401), "auth");
  assert.equal(failureKind(403), "auth");
  assert.equal(failureKind(429), "quota");
  assert.equal(failureKind(500, "usage limit reached"), "quota");
  assert.equal(failureKind(500, "upstream unavailable"), undefined);
});

test("requires a trusted local CA and bounds responses", async () => {
  const directory = await mkdtemp(join(tmpdir(), "codex-broker-test-"));
  const key = join(directory, "server.key");
  const cert = join(directory, "server.crt");
  execFileSync("openssl", [
    "req",
    "-x509",
    "-newkey",
    "rsa:2048",
    "-nodes",
    "-sha256",
    "-days",
    "1",
    "-subj",
    "/CN=127.0.0.1",
    "-addext",
    "subjectAltName=IP:127.0.0.1",
    "-keyout",
    key,
    "-out",
    cert,
  ], { stdio: "ignore" });
  const server = createServer(
    { key: await readFile(key), cert: await readFile(cert) },
    (request, response) => {
      assert.equal(request.url, "/api/v1/route");
      assert.equal(request.headers.authorization, "Bearer cbk_test");
      let body = "";
      request.setEncoding("utf8");
      request.on("data", (chunk) => { body += chunk; });
      request.on("end", () => {
        const input = JSON.parse(body) as { turn_id: string };
        if (input.turn_id === "large") {
          response.writeHead(200, { "content-type": "application/json" });
          response.end(JSON.stringify({ padding: "x".repeat(70_000) }));
          return;
        }
        response.writeHead(200, { "content-type": "application/json" });
        response.end(JSON.stringify({
          status: "ok",
          account_id: "public",
          access_token: "access",
          chatgpt_account_id: "upstream",
          expires_at: "2099-01-01T00:00:00Z",
        }));
      });
    },
  );
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  assert(address && typeof address !== "string");
  const url = `https://127.0.0.1:${address.port}`;
  try {
    await assert.rejects(
      new BrokerClient(url, "cbk_test").route({ session_id: "s", turn_id: "untrusted" }),
      /self-signed certificate/,
    );
    assert.equal(
      (await new BrokerClient(url, "cbk_test", cert).route({ session_id: "s", turn_id: "trusted" })).status,
      "ok",
    );
    await assert.rejects(
      new BrokerClient(url, "cbk_test", cert).route({ session_id: "s", turn_id: "large" }),
      /too large/,
    );
  } finally {
    server.close();
    await once(server, "close");
    await rm(directory, { recursive: true, force: true });
  }
});
