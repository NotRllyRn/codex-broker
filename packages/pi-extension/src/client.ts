import { readFile } from "node:fs/promises";
import type { IncomingMessage } from "node:http";
import { request } from "node:https";

const MAX_RESPONSE_BYTES = 64 * 1024;
const REQUEST_TIMEOUT_MS = 60_000;

export interface Lease {
  status: "ok";
  account_id: string;
  account_label: string;
  access_token: string;
  chatgpt_account_id: string;
  expires_at: string;
  short_remaining_percent: number | null;
  weekly_remaining_percent: number | null;
}

export interface Wait {
  status: "wait";
  code: string;
  next_retry_at: string | null;
  retry_after_seconds: number;
}

export interface RouteInput {
  session_id: string;
  turn_id: string;
  preferred_account_id?: string;
  failed_account_id?: string;
  failure_kind?: string;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function isLease(value: unknown): value is Lease {
  return (
    isRecord(value) &&
    value.status === "ok" &&
    typeof value.account_id === "string" &&
    typeof value.account_label === "string" &&
    typeof value.access_token === "string" &&
    typeof value.chatgpt_account_id === "string" &&
    typeof value.expires_at === "string" &&
    (typeof value.short_remaining_percent === "number" ||
      value.short_remaining_percent === null) &&
    (typeof value.weekly_remaining_percent === "number" ||
      value.weekly_remaining_percent === null)
  );
}

function isWait(value: unknown): value is Wait {
  return (
    isRecord(value) &&
    value.status === "wait" &&
    typeof value.code === "string" &&
    (typeof value.next_retry_at === "string" || value.next_retry_at === null) &&
    typeof value.retry_after_seconds === "number"
  );
}

export class BrokerClient {
  readonly url: URL;

  constructor(
    url: string,
    private readonly apiKey: string,
    private readonly caPath?: string,
  ) {
    try {
      this.url = new URL(url);
    } catch {
      throw new Error("CODEX_BROKER_URL must be a valid URL");
    }
    if (this.url.protocol !== "https:")
      throw new Error("CODEX_BROKER_URL must use HTTPS");
  }

  async route(input: RouteInput, signal?: AbortSignal): Promise<Lease | Wait> {
    const body = JSON.stringify(input);
    const ca = this.caPath ? await readFile(this.caPath) : undefined;
    return new Promise((resolve, reject) => {
      const req = request(
        new URL("/api/v1/route", this.url),
        {
          method: "POST",
          ca,
          signal,
          headers: {
            authorization: `Bearer ${this.apiKey}`,
            "content-type": "application/json",
            "content-length": Buffer.byteLength(body),
          },
        },
        (response: IncomingMessage) => {
          const chunks: Buffer[] = [];
          let size = 0;
          response.on("data", (chunk: Buffer) => {
            size += chunk.length;
            if (size > MAX_RESPONSE_BYTES) {
              response.destroy(new Error("Broker response is too large"));
              return;
            }
            chunks.push(chunk);
          });
          response.on("error", reject);
          response.on("end", () => {
            try {
              const value: unknown = JSON.parse(
                Buffer.concat(chunks).toString("utf8"),
              );
              if (response.statusCode === 200 && isLease(value)) resolve(value);
              else if (response.statusCode === 429 && isWait(value))
                resolve(value);
              else
                reject(
                  new Error(
                    `Invalid broker response (${response.statusCode ?? "unknown"})`,
                  ),
                );
            } catch (error) {
              reject(error);
            }
          });
        },
      );
      req.setTimeout(REQUEST_TIMEOUT_MS, () =>
        req.destroy(new Error("Broker request timed out")),
      );
      req.on("error", reject);
      req.end(body);
    });
  }
}

export function failureKind(status: number, text = ""): string | undefined {
  if (status === 401 || status === 403) return "auth";
  if (status === 429 || /quota|rate.?limit|usage.?limit/i.test(text))
    return "quota";
  return undefined;
}
