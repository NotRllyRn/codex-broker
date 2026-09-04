import { randomUUID } from "node:crypto";
import { streamSimple as streamCodex } from "@earendil-works/pi-ai/api/openai-codex-responses";
import type {
  Api,
  Context,
  Model,
  SimpleStreamOptions,
} from "@earendil-works/pi-ai";
import type {
  ExtensionAPI,
  ExtensionContext,
} from "@earendil-works/pi-coding-agent";

import {
  BrokerClient,
  failureKind,
  type Lease,
  type RouteInput,
} from "./client.js";

const STATUS_ID = "codex-broker";

function clientFromEnvironment(): BrokerClient {
  const required = ["CODEX_BROKER_URL", "CODEX_BROKER_CLIENT_KEY"] as const;
  const missing = required.filter((name) => !process.env[name]);
  if (missing.length) throw new Error(`Missing ${missing.join(", ")}`);
  return new BrokerClient(
    process.env.CODEX_BROKER_URL!,
    process.env.CODEX_BROKER_CLIENT_KEY!,
    process.env.CODEX_BROKER_CA_CERT,
  );
}

function sleep(seconds: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(resolve, Math.max(1, seconds) * 1_000);
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
  let meaningfulOutput = false;
  let retried = false;
  let retryTurn = false;
  let retryQueued = false;
  let turnId = "";

  const broker = (): BrokerClient => (client ??= clientFromEnvironment());
  const percent = (value: number | null): string =>
    value === null ? "?" : `${value}%`;
  const show = (ctx: ExtensionContext): void => {
    const label = lease
      ? `broker: ${lease.account_label} · 5h ${percent(lease.short_remaining_percent)} · week ${percent(lease.weekly_remaining_percent)}`
      : "broker: waiting";
    ctx.ui.setStatus(
      STATUS_ID,
      ctx.ui.theme.fg(lease ? "success" : "warning", label),
    );
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

  const reroute = async (
    ctx: ExtensionContext,
    kind: string,
  ): Promise<boolean> => {
    if (!lease || retried || meaningfulOutput) return false;
    retried = true;
    const replacement = await route(
      ctx,
      {
        session_id: ctx.sessionManager.getSessionId(),
        turn_id: turnId,
        preferred_account_id: lease.account_id,
        failed_account_id: lease.account_id,
        failure_kind: kind,
      },
      true,
    );
    return Boolean(replacement);
  };

  pi.registerProvider("openai-codex", {
    api: "openai-codex-responses",
    apiKey: "broker-managed",
    streamSimple: (
      model: Model<Api>,
      context: Context,
      options?: SimpleStreamOptions,
    ) => {
      if (!lease) throw new Error("Codex Broker did not issue a lease");
      return streamCodex(model as Model<"openai-codex-responses">, context, {
        ...options,
        apiKey: lease.access_token,
        maxRetries: 0,
      });
    },
  });

  pi.on("before_agent_start", async (_event, ctx) => {
    codexActive = ctx.model?.provider === "openai-codex";
    if (!codexActive) return;
    if (retryTurn) {
      retryTurn = false;
      return;
    }
    meaningfulOutput = false;
    retried = false;
    retryQueued = false;
    turnId = randomUUID();
    await route(
      ctx,
      {
        session_id: ctx.sessionManager.getSessionId(),
        turn_id: turnId,
        preferred_account_id: lease?.account_id,
      },
      true,
    );
  });

  pi.on("after_provider_response", async (event, ctx) => {
    const kind = codexActive ? failureKind(event.status) : undefined;
    if (kind && (await reroute(ctx, kind))) retryQueued = true;
  });

  pi.on("message_update", (event) => {
    if (codexActive && event.assistantMessageEvent.type.endsWith("_delta")) {
      meaningfulOutput = true;
    }
  });

  pi.on("message_end", async (event, ctx) => {
    if (!codexActive || event.message.role !== "assistant" || meaningfulOutput)
      return;
    const kind = failureKind(0, event.message.errorMessage ?? "");
    if (kind && (await reroute(ctx, kind))) retryQueued = true;
  });

  pi.on("agent_end", () => {
    if (!retryQueued) return;
    retryQueued = false;
    retryTurn = true;
    pi.sendMessage(
      {
        customType: "codex-broker-retry",
        content:
          "The broker changed accounts. Retry the interrupted request once.",
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
        lease
          ? `Codex Broker account: ${lease.account_label} · 5h ${percent(lease.short_remaining_percent)} · week ${percent(lease.weekly_remaining_percent)}`
          : "Codex Broker has no active route",
        "info",
      );
    },
  });
}
