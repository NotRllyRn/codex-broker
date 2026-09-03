import { randomUUID } from "node:crypto";
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";

import { BrokerClient, failureKind, type Lease, type RouteInput } from "./client.js";

const STATUS_ID = "codex-broker";

function clientFromEnvironment(): BrokerClient {
  const required = [
    "CODEX_BROKER_URL",
    "CODEX_BROKER_CLIENT_KEY",
    "CODEX_BROKER_CA_CERT",
    "CODEX_BROKER_CLIENT_CERT",
    "CODEX_BROKER_CLIENT_KEY_FILE",
  ] as const;
  const missing = required.filter((name) => !process.env[name]);
  if (missing.length) throw new Error(`Missing ${missing.join(", ")}`);
  return new BrokerClient(
    process.env.CODEX_BROKER_URL!,
    process.env.CODEX_BROKER_CLIENT_KEY!,
    process.env.CODEX_BROKER_CA_CERT!,
    process.env.CODEX_BROKER_CLIENT_CERT!,
    process.env.CODEX_BROKER_CLIENT_KEY_FILE!,
  );
}

function sleep(seconds: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(resolve, Math.max(1, Math.min(seconds, 60)) * 1_000);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        reject(signal.reason);
      },
      { once: true },
    );
  });
}

export default function codexBroker(pi: ExtensionAPI): void {
  let client: BrokerClient | undefined;
  let lease: Lease | undefined;
  let codexActive = false;
  let retried = false;
  let retryTurn = false;
  let retryQueued = false;

  const broker = (): BrokerClient => (client ??= clientFromEnvironment());
  const show = (ctx: ExtensionContext): void => {
    const label = lease ? `broker: ${lease.account_id}` : "broker: waiting";
    ctx.ui.setStatus(STATUS_ID, ctx.ui.theme.fg(lease ? "success" : "warning", label));
  };

  const route = async (
    ctx: ExtensionContext,
    input: RouteInput,
    wait: boolean,
  ): Promise<Lease | undefined> => {
    while (true) {
      const result = await broker().route(input, ctx.signal);
      if (result.status === "ok") {
        lease = result;
        show(ctx);
        return result;
      }
      lease = undefined;
      show(ctx);
      if (!wait) return undefined;
      await sleep(result.retry_after_seconds, ctx.signal);
    }
  };

  const reroute = async (ctx: ExtensionContext, kind: string): Promise<boolean> => {
    if (!lease || retried) return false;
    retried = true;
    const replacement = await route(
      ctx,
      {
        session_id: ctx.sessionManager.getSessionId(),
        turn_id: randomUUID(),
        preferred_account_id: lease.account_id,
        failed_account_id: lease.account_id,
        failure_kind: kind,
      },
      false,
    );
    return Boolean(replacement);
  };

  pi.on("input", () => {
    retried = false;
    retryQueued = false;
  });

  pi.on("before_agent_start", async (_event, ctx) => {
    codexActive = ctx.model?.provider === "openai-codex";
    if (!codexActive) return;
    if (retryTurn) {
      retryTurn = false;
      return;
    }
    await route(
      ctx,
      {
        session_id: ctx.sessionManager.getSessionId(),
        turn_id: randomUUID(),
        preferred_account_id: lease?.account_id,
      },
      true,
    );
  });

  pi.on("before_provider_headers", (event) => {
    if (!codexActive || !lease) return;
    event.headers.authorization = `Bearer ${lease.access_token}`;
    event.headers["chatgpt-account-id"] = lease.chatgpt_account_id;
  });

  pi.on("after_provider_response", async (event, ctx) => {
    const kind = codexActive ? failureKind(event.status) : undefined;
    if (kind && (await reroute(ctx, kind))) retryQueued = true;
  });

  pi.on("tool_result", async (event, ctx) => {
    if (!codexActive || !event.isError) return;
    const text = event.content
      .filter((part) => part.type === "text")
      .map((part) => part.text)
      .join("\n");
    const kind = failureKind(0, text);
    if (kind) await reroute(ctx, kind);
  });

  pi.on("agent_end", () => {
    if (!retryQueued) return;
    retryQueued = false;
    retryTurn = true;
    pi.sendMessage(
      {
        customType: "codex-broker-retry",
        content: "The broker changed accounts. Retry the interrupted request once.",
        display: true,
      },
      { deliverAs: "followUp", triggerTurn: true },
    );
  });

  pi.on("agent_settled", () => {
    codexActive = false;
  });

  pi.registerCommand("broker-status", {
    description: "Show the current Codex Broker route",
    handler: async (_args, ctx) => {
      ctx.ui.notify(
        lease ? `Codex Broker account: ${lease.account_id}` : "Codex Broker has no active route",
        "info",
      );
    },
  });
}
