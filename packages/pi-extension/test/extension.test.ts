import assert from "node:assert/strict";
import test from "node:test";

import type {
  ExtensionAPI,
  ExtensionContext,
} from "@earendil-works/pi-coding-agent";

import { BrokerClient, type Lease, type RouteInput } from "../src/client.js";
import codexBroker from "../src/index.js";

const LEASE: Lease = {
  status: "ok",
  account_id: "public",
  account_label: "Personal",
  access_token: "access",
  chatgpt_account_id: "upstream",
  expires_at: "2099-01-01T00:00:00Z",
  short_remaining_percent: 80,
  weekly_remaining_percent: 60,
  short_resets_at: new Date(Date.now() + 121 * 60_000).toISOString(),
  weekly_resets_at: new Date(Date.now() + 51 * 60 * 60_000).toISOString(),
};

test("waits for pool reset and resumes automatically", async () => {
  const originalRoute = BrokerClient.prototype.route;
  let calls = 0;
  BrokerClient.prototype.route = async () => {
    calls += 1;
    return calls === 1
      ? {
          status: "wait",
          code: "POOL_EXHAUSTED",
          next_retry_at: "2099-01-01T00:00:00Z",
          retry_after_seconds: 0,
        }
      : LEASE;
  };
  process.env.CODEX_BROKER_URL = "https://broker.test";
  process.env.CODEX_BROKER_CLIENT_KEY = "cbk_test";
  const handlers = new Map<string, (...args: unknown[]) => unknown>();
  const pi = {
    registerProvider: () => undefined,
    registerCommand: () => undefined,
    on: (name: string, handler: (...args: unknown[]) => unknown) => {
      handlers.set(name, handler);
    },
  } as unknown as ExtensionAPI;
  const ctx = {
    model: { provider: "openai-codex" },
    signal: new AbortController().signal,
    sessionManager: { getSessionId: () => "session" },
    ui: {
      setStatus: () => undefined,
      theme: { fg: (_color: string, value: string) => value },
    },
  } as unknown as ExtensionContext;
  try {
    codexBroker(pi);
    await handlers.get("before_agent_start")?.({}, ctx);
    assert.equal(calls, 2);
  } finally {
    BrokerClient.prototype.route = originalRoute;
    delete process.env.CODEX_BROKER_URL;
    delete process.env.CODEX_BROKER_CLIENT_KEY;
  }
});

test("keeps replacing exhausted accounts before output", async () => {
  const originalRoute = BrokerClient.prototype.route;
  const calls: RouteInput[] = [];
  BrokerClient.prototype.route = async (input) => {
    calls.push(input);
    return {
      ...LEASE,
      account_id:
        ["public", "replacement", "third"][calls.length - 1] ?? "last",
    };
  };
  process.env.CODEX_BROKER_URL = "https://broker.test";
  process.env.CODEX_BROKER_CLIENT_KEY = "cbk_test";

  const handlers = new Map<string, (...args: unknown[]) => unknown>();
  const sent: unknown[] = [];
  const statuses: string[] = [];
  const pi = {
    registerProvider: () => undefined,
    registerCommand: () => undefined,
    on: (name: string, handler: (...args: unknown[]) => unknown) => {
      handlers.set(name, handler);
    },
    sendMessage: (...args: unknown[]) => sent.push(args),
  } as unknown as ExtensionAPI;
  const ctx = {
    model: { provider: "openai-codex" },
    signal: new AbortController().signal,
    sessionManager: { getSessionId: () => "session" },
    ui: {
      setStatus: (_id: string, value: string) => statuses.push(value),
      theme: { fg: (_color: string, value: string) => value },
    },
  } as unknown as ExtensionContext;

  try {
    codexBroker(pi);
    await handlers.get("before_agent_start")?.({}, ctx);
    assert.equal(calls.length, 1);
    assert.equal(
      statuses.at(-1),
      "broker: Personal · 5h 80% (resets 2h 1m) · week 60% (resets 2d 3h)",
    );
    await handlers.get("after_provider_response")?.({ status: 429 }, ctx);
    assert.equal(calls.length, 2);
    assert.equal(calls[1].failed_account_id, "public");
    await handlers.get("after_provider_response")?.({ status: 429 }, ctx);
    assert.equal(calls.length, 2);
    handlers.get("agent_end")?.({}, ctx);
    assert.equal(sent.length, 1);
    await handlers.get("before_agent_start")?.({}, ctx);
    assert.equal(calls.length, 2);
    await handlers.get("after_provider_response")?.({ status: 429 }, ctx);
    assert.equal(calls.length, 3);
    assert.equal(calls[2].failed_account_id, "replacement");
    handlers.get("agent_end")?.({}, ctx);
    assert.equal(sent.length, 2);
    await handlers.get("before_agent_start")?.({}, ctx);

    handlers.get("message_update")?.(
      { assistantMessageEvent: { type: "text_delta" } },
      ctx,
    );
    await handlers.get("after_provider_response")?.({ status: 429 }, ctx);
    assert.equal(calls.length, 3);
  } finally {
    BrokerClient.prototype.route = originalRoute;
    delete process.env.CODEX_BROKER_URL;
    delete process.env.CODEX_BROKER_CLIENT_KEY;
  }
});
