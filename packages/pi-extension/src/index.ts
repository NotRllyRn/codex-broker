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
import { loadConfig, saveConfig, type BrokerConfig } from "./config.js";

const STATUS_ID = "codex-broker";

function configuredClient(config: Partial<BrokerConfig>): BrokerClient {
  if (!config.url || !config.clientKey)
    throw new Error("Codex Broker is not configured; run /broker-status");
  return new BrokerClient(config.url, config.clientKey, config.caCert);
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
  let config = loadConfig();
  let client: BrokerClient | undefined;
  let lease: Lease | undefined;
  let connection: "connecting" | "ready" | "unavailable" = "connecting";
  let codexActive = false;
  let meaningfulOutput = false;
  let failureHandled = false;
  const failedAccounts = new Set<string>();
  let retryTurn = false;
  let retryQueued = false;
  let turnId = "";

  const broker = (): BrokerClient => (client ??= configuredClient(config));
  const percent = (value: number | null): string =>
    value === null ? "?" : `${value}%`;
  const window = (remaining: number | null, reset: string | null): string => {
    const minutes = Math.max(
      0,
      Math.ceil((Date.parse(reset ?? "") - Date.now()) / 60_000),
    );
    const duration =
      minutes >= 1_440
        ? `${Math.floor(minutes / 1_440)}d ${Math.floor((minutes % 1_440) / 60)}h`
        : minutes >= 60
          ? `${Math.floor(minutes / 60)}h ${minutes % 60}m`
          : `${minutes}m`;
    return `${percent(remaining)}${reset ? ` (resets ${duration})` : ""}`;
  };
  const status = (value: Lease): string =>
    `${value.account_label} · 5h ${window(value.short_remaining_percent, value.short_resets_at)} · week ${window(value.weekly_remaining_percent, value.weekly_resets_at)}`;
  const show = (ctx: ExtensionContext): void => {
    const label = lease ? `broker: ${status(lease)}` : `broker: ${connection}`;
    ctx.ui.setStatus(
      STATUS_ID,
      ctx.ui.theme.fg(
        lease || connection === "ready"
          ? "success"
          : connection === "unavailable"
            ? "error"
            : "warning",
        label,
      ),
    );
  };

  const checkHealth = async (ctx: ExtensionContext): Promise<boolean> => {
    connection = "connecting";
    show(ctx);
    try {
      await broker().health(ctx.signal);
      connection = "ready";
    } catch {
      connection = "unavailable";
    }
    show(ctx);
    return connection === "ready";
  };

  const route = async (
    ctx: ExtensionContext,
    input: RouteInput,
    wait: boolean,
  ): Promise<Lease | undefined> => {
    while (true) {
      try {
        const result = await broker().route(input, ctx.signal);
        connection = "ready";
        if (result.status === "ok") {
          lease = result;
          show(ctx);
          return result;
        }
        lease = undefined;
        show(ctx);
        if (!wait) return undefined;
        await sleep(result.retry_after_seconds, ctx.signal);
      } catch (error) {
        connection = "unavailable";
        lease = undefined;
        show(ctx);
        throw error;
      }
    }
  };

  const reroute = async (
    ctx: ExtensionContext,
    kind: string,
  ): Promise<boolean> => {
    if (
      !lease ||
      failureHandled ||
      (kind !== "retry" && failedAccounts.has(lease.account_id))
    )
      return false;
    failureHandled = true;
    const current = lease;
    const failed = kind === "retry" ? undefined : current.account_id;
    if (failed) failedAccounts.add(failed);
    let replacement = await route(
      ctx,
      {
        session_id: ctx.sessionManager.getSessionId(),
        turn_id: turnId,
        preferred_account_id: current.account_id,
        failed_account_id: failed,
        failure_kind: failed ? kind : undefined,
      },
      true,
    );
    if (
      kind === "retry" &&
      replacement?.account_id === current.account_id &&
      (replacement.short_remaining_percent === 0 ||
        replacement.weekly_remaining_percent === 0)
    ) {
      failedAccounts.add(current.account_id);
      replacement = await route(
        ctx,
        {
          session_id: ctx.sessionManager.getSessionId(),
          turn_id: turnId,
          preferred_account_id: current.account_id,
          failed_account_id: current.account_id,
          failure_kind: "quota",
        },
        true,
      );
    }
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

  pi.on("session_start", async (_event, ctx) => {
    lease = undefined;
    await checkHealth(ctx);
  });

  pi.on("before_agent_start", async (_event, ctx) => {
    codexActive = ctx.model?.provider === "openai-codex";
    if (!codexActive) return;
    if (retryTurn) {
      retryTurn = false;
      meaningfulOutput = false;
      failureHandled = false;
      return;
    }
    meaningfulOutput = false;
    failureHandled = false;
    failedAccounts.clear();
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
    if (!codexActive || event.message.role !== "assistant") return;
    const error = event.message.errorMessage ?? "";
    if (
      event.message.stopReason === "error" &&
      !error.includes("context_length_exceeded") &&
      /inputs?.*(?:exceeds?|esceeds?).*context window/i.test(error)
    ) {
      return {
        message: {
          ...event.message,
          errorMessage: `context_length_exceeded: ${error}`,
        },
      };
    }
    const kind = failureKind(0, error);
    if (kind && (await reroute(ctx, kind))) retryQueued = true;
  });

  pi.on("agent_end", () => {
    if (!retryQueued) return;
    retryQueued = false;
    retryTurn = true;
    pi.sendMessage(
      {
        customType: "codex-broker-retry",
        content: meaningfulOutput
          ? "Continue exactly where the interrupted response stopped without repeating prior output. Codex Broker verified an available account."
          : "Retry the interrupted request now. Codex Broker verified an available account; do not ask the user to repeat it.",
        display: true,
      },
      { deliverAs: "followUp", triggerTurn: true },
    );
  });

  pi.on("agent_settled", () => {
    codexActive = false;
  });

  pi.registerCommand("broker-status", {
    description: "Configure Codex Broker or show its status",
    handler: async (_args, ctx) => {
      const action = await ctx.ui.select("Codex Broker", [
        "Show status",
        "Set server address",
        "Set API token",
        "Set CA certificate",
      ]);
      if (!action) return;
      if (action === "Show status") {
        ctx.ui.notify(
          lease
            ? `Codex Broker account: ${status(lease)}`
            : (await checkHealth(ctx))
              ? "Codex Broker is ready"
              : "Codex Broker is unavailable",
          "info",
        );
        return;
      }
      const field =
        action === "Set server address"
          ? "url"
          : action === "Set API token"
            ? "clientKey"
            : "caCert";
      const label = {
        url: "Server address",
        clientKey: "API token",
        caCert: "CA certificate path",
      }[field];
      const value = await ctx.ui.input(
        label,
        field === "clientKey" ? "Paste token" : config[field] ?? "",
      );
      if (!value) return;
      config = { ...config, [field]: value.trim() };
      saveConfig(config);
      client = undefined;
      lease = undefined;
      const healthy = await checkHealth(ctx);
      ctx.ui.notify(
        `Codex Broker settings saved; server ${healthy ? "ready" : "unavailable"}`,
        healthy ? "info" : "warning",
      );
    },
  });
}
