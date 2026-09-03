import { readFile } from "node:fs/promises";
import type { IncomingMessage } from "node:http";
import { request } from "node:https";

export interface Lease {
  status: "ok";
  account_id: string;
  access_token: string;
  chatgpt_account_id: string;
  expires_at: string;
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

export class BrokerClient {
  readonly url: URL;

  constructor(
    url: string,
    private readonly apiKey: string,
    private readonly caPath: string,
    private readonly certPath: string,
    private readonly keyPath: string,
  ) {
    try {
      this.url = new URL(url);
    } catch {
      throw new Error("CODEX_BROKER_URL must be a valid URL");
    }
    if (this.url.protocol !== "https:") throw new Error("CODEX_BROKER_URL must use HTTPS");
  }

  async route(input: RouteInput, signal?: AbortSignal): Promise<Lease | Wait> {
    const body = JSON.stringify(input);
    const [ca, cert, key] = await Promise.all([
      readFile(this.caPath),
      readFile(this.certPath),
      readFile(this.keyPath),
    ]);
    return new Promise((resolve, reject) => {
      const req = request(
        new URL("/api/v1/route", this.url),
        {
          method: "POST",
          ca,
          cert,
          key,
          signal,
          headers: {
            authorization: `Bearer ${this.apiKey}`,
            "content-type": "application/json",
            "content-length": Buffer.byteLength(body),
          },
        },
        (response: IncomingMessage) => {
          const chunks: Buffer[] = [];
          response.on("data", (chunk: Buffer) => chunks.push(chunk));
          response.on("end", () => {
            try {
              const value = JSON.parse(Buffer.concat(chunks).toString("utf8")) as Lease | Wait;
              if (response.statusCode === 200 || response.statusCode === 429) resolve(value);
              else reject(new Error(`Broker returned ${response.statusCode}: ${JSON.stringify(value)}`));
            } catch (error) {
              reject(error);
            }
          });
        },
      );
      req.on("error", reject);
      req.end(body);
    });
  }
}

export function failureKind(status: number, text = ""): string | undefined {
  if (status === 401 || status === 403) return "auth";
  if (status === 429 || /quota|rate.?limit|usage.?limit/i.test(text)) return "quota";
  return undefined;
}
