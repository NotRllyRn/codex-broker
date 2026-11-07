const [major, minor] = process.versions.node.split(".").map(Number);
if (major < 22 || (major === 22 && minor < 13))
  throw Error("Node 22.13 or newer required");
// Embedded Node host. Protocol uses dedicated descriptors so console/stdout cannot
// corrupt it. Extensions are trusted code with the service's OS permissions.
const fs = require("node:fs");
const Module = require("node:module");
const { promisify } = require("node:util");
const read = promisify(fs.read);
const MAX = 128 * 1024 * 1024;
const sessions = new Map();
const clone = (value) => JSON.parse(JSON.stringify(value));
function send(value) {
  if (value.kind === "result") send({ kind: "serializing" });
  const body = Buffer.from(JSON.stringify(value));
  if (body.length > MAX) throw Error("hook output exceeds limit");
  // Serialization has its own bounded budget because getters can run code.
  if (value.kind === "result") send({ kind: "sending" });
  const size = Buffer.alloc(4);
  size.writeUInt32BE(body.length);
  for (const buffer of [size, body]) {
    let offset = 0;
    while (offset < buffer.length) offset += fs.writeSync(4, buffer, offset);
  }
}
async function receive() {
  async function exact(n) {
    const b = Buffer.alloc(n);
    let offset = 0;
    while (offset < n) {
      const { bytesRead } = await read(3, b, offset, n - offset, null);
      if (!bytesRead) process.exit(0);
      offset += bytesRead;
    }
    return b;
  }
  const size = (await exact(4)).readUInt32BE();
  if (size > MAX) throw Error("hook input exceeds limit");
  return JSON.parse((await exact(size)).toString());
}
function receiveSync() {
  function exact(n) {
    const b = Buffer.alloc(n);
    let offset = 0;
    while (offset < n) {
      const count = fs.readSync(3, b, offset, n - offset);
      if (!count) process.exit(0);
      offset += count;
    }
    return b;
  }
  const size = exact(4).readUInt32BE();
  if (size > MAX) throw Error("protocol input exceeds limit");
  return JSON.parse(exact(size).toString());
}
function load(command) {
  const mod = new Module(command.filename);
  mod.filename = command.filename;
  mod.paths = Module._nodeModulePaths(
    require("node:path").dirname(command.filename),
  );
  mod._compile(command.code, command.filename);
  return mod.exports;
}
const scope = new (require("node:async_hooks").AsyncLocalStorage)();
function invocation() {
  const c = scope.getStore();
  if (!c?.active) throw Error("extension API requires an active invocation");
  return c;
}
function log(level, message, fields) {
  const current = scope.getStore();
  if (!current?.active) return;
  if (typeof level === "string") level = level.toLowerCase();
  const raw = JSON.stringify({ level, message, fields });
  if (Buffer.byteLength(raw) > 16384) throw Error("log entry exceeds 16 KiB");
  if (!["debug", "info", "warning", "warn", "error"].includes(level))
    throw Error("invalid log level");
  if (
    typeof message !== "string" ||
    (fields != null && (typeof fields !== "object" || Array.isArray(fields)))
  )
    throw Error("invalid log entry");
  // Host owns the shared request quota; avoid flooding IPC after it is exhausted.
  if (++current.logs <= 101) send({ kind: "log", entry: JSON.parse(raw) });
}
async function dispatch(c) {
  c.logs = 0;
  if (c.op === "schema") return { schema: load(c).settings ?? { fields: [] } };
  if (c.op === "start") {
    if (sessions.size >= 1024)
      throw Error("extension request capacity reached");
    const s = { handlers: {}, settings: c.settings, context: c.identity };
    sessions.set(c.session, s);
    const api = {
      on(name, handler) {
        if (
          !c.hooks.includes(name) ||
          typeof handler !== "function" ||
          (s.handlers[name]?.length ?? 0) >= 32
        )
          throw Error("invalid hook registration");
        (s.handlers[name] ??= []).push(handler);
      },
      getSettings: () => clone(s.settings),
      getDataDir: () => {
        fs.mkdirSync(c.dataDir, { recursive: true, mode: 0o700 });
        return c.dataDir;
      },
      log,
      updateThread(update) {
        const current = invocation();
        if (current.session !== c.session)
          throw Error("wrong request invocation");
        if (typeof update !== "function")
          throw Error("updateThread requires a callback");
        if (!s.context.threadId)
          throw Error("native thread identity unavailable");
        send({ kind: "state_read" });
        const previous = receiveSync().value;
        const next = update(previous);
        if (next?.then) {
          Promise.resolve(next).catch(() => {});
          throw Error("async state updates are unsupported");
        }
        const raw = JSON.stringify(next);
        if (raw === undefined || Buffer.byteLength(raw) > 65536)
          throw Error("thread state must be JSON under 64 KiB");
        send({ kind: "state", value: JSON.parse(raw) });
        const reply = receiveSync();
        if (reply.error) throw Error(reply.error);
        return next;
      },
    };
    const factory = load(c).default;
    if (typeof factory !== "function")
      throw Error("default export must be a factory");
    await factory(api);
    return {};
  }
  if (c.op === "release") {
    sessions.delete(c.session);
    return {};
  }
  const s = sessions.get(c.session);
  if (!s) throw Error("extension worker session unavailable");
  s.settings = c.settings;
  const handler = s.handlers[c.hook]?.[c.index];
  if (!handler) throw Error("extension handler unavailable");
  const result = await handler(c.event, clone(s.context));
  return {
    patch:
      c.hook === "settings_loaded" || c.hook === "settings_changed"
        ? null
        : (result ?? null),
  };
}
function handlerCounts(c) {
  const s = sessions.get(c.session);
  return s
    ? Object.fromEntries(
        Object.entries(s.handlers).map(([k, v]) => [k, v.length]),
      )
    : undefined;
}
send({ ready: true });
(async () => {
  while (true) {
    const c = await receive();
    send({ kind: "running" });
    c.active = true;
    try {
      await scope.run(c, async () => {
        const result = await dispatch(c);
        send({ kind: "result", ...result, handlers: handlerCounts(c) });
      });
    } catch (error) {
      if (c.op === "start") sessions.delete(c.session);
      send({
        kind: "result",
        handlers: handlerCounts(c),
        error: String(error?.stack ?? error).slice(0, 8192),
      });
    } finally {
      c.active = false;
    }
  }
})().catch(() => process.exit(1));
