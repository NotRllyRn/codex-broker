import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { configPath, loadConfig, saveConfig } from "../src/config.js";

test("persists broker settings privately and prefers them over environment", () => {
  const directory = mkdtempSync(join(tmpdir(), "codex-broker-pi-"));
  process.env.PI_CODING_AGENT_DIR = directory;
  process.env.CODEX_BROKER_URL = "https://environment";
  try {
    saveConfig({
      url: "https://saved",
      clientKey: "cbk_saved",
      caCert: "/ca.crt",
    });
    assert.deepEqual(loadConfig(), {
      url: "https://saved",
      clientKey: "cbk_saved",
      caCert: "/ca.crt",
    });
    assert.equal(configPath(), join(directory, "codex-broker.json"));
    assert.equal(statSync(configPath()).mode & 0o777, 0o600);
    assert.match(readFileSync(configPath(), "utf8"), /cbk_saved/);
  } finally {
    delete process.env.PI_CODING_AGENT_DIR;
    delete process.env.CODEX_BROKER_URL;
  }
});
