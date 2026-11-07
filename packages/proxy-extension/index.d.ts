/** Self-contained TypeScript modules; import this API using `import type`. */
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
export interface ExtensionAPI {
  on<K extends keyof Events>(name: K, handler: (event: Events[K], context: Context) => Results[K] | void): void;
  /** Atomic, process-local JSON state scoped by extension/client/native thread.
   * Null deletes state. No native identity => throws. No I/O in the callback. */
  updateThread<T>(update: (previous: T | null) => T | null): T | null;
}
export type Extension = (api: ExtensionAPI) => void;
