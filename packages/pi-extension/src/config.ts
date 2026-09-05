import { chmodSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";

export interface BrokerConfig {
  url: string;
  clientKey: string;
  caCert?: string;
}

export const configPath = (): string =>
  join(
    process.env.PI_CODING_AGENT_DIR ?? join(homedir(), ".pi", "agent"),
    "codex-broker.json",
  );

export function loadConfig(): Partial<BrokerConfig> {
  let saved: Partial<BrokerConfig> = {};
  try {
    saved = JSON.parse(
      readFileSync(configPath(), "utf8"),
    ) as Partial<BrokerConfig>;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
  return {
    url: saved.url ?? process.env.CODEX_BROKER_URL,
    clientKey: saved.clientKey ?? process.env.CODEX_BROKER_CLIENT_KEY,
    caCert: saved.caCert ?? process.env.CODEX_BROKER_CA_CERT,
  };
}

export function saveConfig(config: Partial<BrokerConfig>): void {
  const path = configPath();
  mkdirSync(dirname(path), { recursive: true, mode: 0o700 });
  writeFileSync(path, `${JSON.stringify(config, null, 2)}\n`, { mode: 0o600 });
  chmodSync(path, 0o600);
}
