/** Trusted Node TypeScript modules; import this API using `import type`. */
export type Headers = Record<string, string[]>;
export interface Context {
  readonly requestId: string;
  readonly clientId: string;
  readonly threadId?: string;
  readonly turnId?: string;
}
export interface Body {
  /** UTF-8 text. For binary content only bodyBase64 is supplied. */
  body?: string;
  /** Takes precedence over body. Return null to clear a prior binary body. */
  bodyBase64?: string | null;
}
export interface Response extends Body { status: number; headers: Headers }
export interface Request extends Body {
  readonly path: string;
  readonly method: string;
  headers: Headers;
}
export interface Attempt extends Request { readonly attempt: number; readonly accountId: string }
export interface Events {
  settings_loaded: { readonly settings: Record<string, any> };
  settings_changed: { readonly settings: Record<string, any>; readonly previous: Record<string, any> };
  request: Request;
  prepared: Request;
  route: { readonly attempt: number; readonly failureKind: string };
  attempt: Attempt;
  upstream: { readonly attempt: number; readonly accountId: string; readonly status: number; readonly headers: Headers };
  retry: { readonly attempt: number; readonly accountId: string; readonly failureKind: string };
  response: { status: number; headers: Headers; buffer: boolean };
  upstream_event: { event: Record<string, any> | null };
  client_event: { event: Record<string, any> | null };
  body: Response;
  finish: { readonly outcome: "complete" | "local" | "error" | "cancelled" };
}
export interface Results {
  settings_loaded: void;
  settings_changed: void;
  request: Partial<Body> & { headers?: Headers; response?: Response };
  prepared: Results["request"];
  attempt: Results["request"];
  route: void;
  upstream: void;
  retry: void;
  response: Partial<Events["response"]>;
  upstream_event: Events["upstream_event"];
  client_event: Events["client_event"];
  body: Partial<Response>;
  finish: void;
}
export type LogLevel = "debug" | "info" | "warning" | "warn" | "error";
export interface ExtensionAPI {
  /** Stable absolute per-extension directory, created lazily. Normal Node fs and
   * database APIs may use it. This is an organizational helper, not a sandbox. */
  getDataDir(): string;
  /** Persistent, attributed service log. Fields must be JSON. Password settings
   * and recognized secret fields are redacted; avoid logging transcripts/tokens. */
  log(level: LogLevel, message: string, fields?: Record<string, unknown>): void;
  /** A cloned snapshot for this invocation. Changes arrive between hooks. */
  getSettings<T = Record<string, unknown>>(): T;
  on<K extends keyof Events>(name: K, handler: (event: Events[K], context: Context) => Results[K] | void | Promise<Results[K] | void>): void;
  /** Atomic, process-local JSON state scoped by extension/client/native thread.
   * Null deletes state. No native identity => throws. No I/O in the callback. */
  updateThread<T>(update: (previous: T | null) => T | null): T | null;
}
export type Extension = (api: ExtensionAPI) => void | Promise<void>;

export interface SettingOption { value: string; label: string }
export interface SettingBase<T> {
  key: string;
  label: string;
  default: T;
  description?: string;
  placeholder?: string;
  group?: string;
  unit?: string;
  required?: boolean;
  /** Only affects dashboard visibility; hidden values are still validated. */
  visibleWhen?: { key: string; equals: unknown };
}
export type SettingField =
  | (SettingBase<string> & {
      type: "text" | "textarea" | "password" | "email" | "url" | "tel" | "path" | "color" | "date" | "time" | "datetime";
      minLength?: number; maxLength?: number; /** Go regular expression. */ pattern?: string;
    })
  | (SettingBase<number> & {
      /** Duration uses milliseconds; percentage 0–100; ratio 0–1. */
      type: "number" | "integer" | "slider" | "percentage" | "ratio" | "duration";
      min?: number; max?: number; step?: number;
    })
  | (SettingBase<boolean> & { type: "boolean" })
  | (SettingBase<string> & { type: "select" | "radio"; options: SettingOption[] })
  | (SettingBase<string[]> & { type: "multiselect"; options: SettingOption[] })
  | (SettingBase<string[]> & { type: "string-list" })
  | (SettingBase<Record<string, string>> & { type: "key-value" })
  | (SettingBase<unknown> & { type: "json" });
/** Export a named `settings` constant satisfying this schema alongside the factory. */
export interface SettingsSchema { title?: string; description?: string; fields: SettingField[] }
