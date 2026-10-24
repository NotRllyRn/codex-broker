#!/usr/bin/env python3
"""Sync numerical Codex/Pi usage to Broker; Python 3.9+, standard library only."""
import argparse
import datetime as dt
import getpass
import gzip
import hashlib
import json
import os
from pathlib import Path
import plistlib
import secrets
import shutil
import socket
import sqlite3
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
import ssl
import xml.etree.ElementTree as ET

VERSION = 1
BATCH_SIZE = 500
FIELDS = ("input_tokens", "cached_input_tokens", "cache_write_input_tokens",
          "output_tokens", "reasoning_output_tokens")


def compact(value):
    return json.dumps(value, separators=(",", ":"), sort_keys=True)


def digest(value):
    return hashlib.sha256(compact(value).encode()).hexdigest()


def timestamp(value):
    if not isinstance(value, str):
        raise ValueError("Usage record has no timestamp")
    result = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if result.tzinfo is None:
        raise ValueError("Usage timestamps must include a timezone")
    return int(result.timestamp() * 1000)


def count(value):
    if isinstance(value, bool) or not isinstance(value, int) or not 0 <= value <= 10**12:
        raise ValueError("Invalid token count")
    return value


def event(source, key, when, model, usage, tier="default"):
    values = {k: count(usage.get(k, 0)) for k in FIELDS}
    if values["cached_input_tokens"] + values["cache_write_input_tokens"] > values["input_tokens"]:
        raise ValueError("Cached tokens exceed input tokens")
    if values["reasoning_output_tokens"] > values["output_tokens"]:
        raise ValueError("Reasoning tokens exceed output tokens")
    if not model:
        raise ValueError("Usage record has no model; refusing to guess")
    return dict(id=digest(key), source=source, timestamp_ms=timestamp(when), model=model,
                service_tier=tier, input_tokens=values["input_tokens"],
                cached_input_tokens=values["cached_input_tokens"],
                cache_write_tokens=values["cache_write_input_tokens"],
                output_tokens=values["output_tokens"],
                reasoning_tokens=values["reasoning_output_tokens"], usage_known=True)


def codex_record(record, state):
    """Prefer modern per-response records; never add their token_count mirrors."""
    kind, payload = record.get("type"), record.get("payload") or {}
    if kind == "session_meta" and not state.get("session"):
        state["session"] = payload.get("id") or payload.get("session_id")
        source = payload.get("source")
        spawn = source.get("subagent", {}).get("thread_spawn", {}) if isinstance(source, dict) else {}
        if payload.get("forked_from_id") or spawn.get("parent_thread_id"):
            state["fork_anchor"] = timestamp(record["timestamp"])
    if kind == "turn_context":
        state["model"] = payload.get("model", state.get("model"))
    if payload.get("type") == "thread_settings_applied":
        state["tier"] = payload.get("thread_settings", {}).get("service_tier") or "default"
    if kind == "token_usage_record":
        response = payload.get("response_id")
        usage = payload.get("usage")
        if not response or not isinstance(usage, dict):
            raise ValueError("Incomplete per-response Codex usage")
        state["modern"] = True
        return [event("codex", ["codex-response", response], record["timestamp"],
                      payload.get("model") or state.get("model"), usage, state.get("tier", "default"))]
    if payload.get("type") != "token_count" or state.get("modern"):
        return []
    info = payload.get("info")
    if not isinstance(info, dict) or not isinstance(info.get("last_token_usage"), dict):
        return []
    latest, total = info["last_token_usage"], info.get("total_token_usage")
    signature = compact(total if isinstance(total, dict) else latest)
    if signature == state.get("signature"):
        return []
    state["signature"] = signature
    previous = state.get("total")
    if isinstance(total, dict):
        state["total"] = total
    anchor = state.get("fork_anchor")
    when = timestamp(record["timestamp"])
    if anchor is not None:
        if when - anchor < 1000:
            state["fork_anchor"] = when
            return []
        del state["fork_anchor"]
    usage = latest
    if isinstance(total, dict) and isinstance(previous, dict):
        delta = {k: count(total.get(k, 0)) - count(previous.get(k, 0)) for k in FIELDS}
        if all(v >= 0 for v in delta.values()):
            usage = delta
    if not any(usage.get(k, 0) for k in FIELDS):
        return []
    if not state.get("session"):
        raise ValueError("Codex usage has no session identity")
    return [event("codex", ["codex-legacy", state["session"], record["timestamp"], signature],
                  record["timestamp"], state.get("model"), usage, state.get("tier", "default"))]


def pi_record(record, state):
    kind = record.get("type")
    if kind == "session":
        return []
    message = record.get("message", {}) if kind == "message" else record
    usage = message.get("usage")
    if not isinstance(usage, dict):
        return []
    if kind == "message" and message.get("role") not in ("assistant", "toolResult"):
        return []
    if kind not in ("message", "compaction", "branch_summary", "usage"):
        return []
    if not record.get("id"):
        raise ValueError("Pi usage has no event identity")
    model = message.get("responseModel") or message.get("model") or "Tools/summaries"
    # Pi's input/cache fields are disjoint; Broker's input includes the cache.
    cached, write = count(usage.get("cacheRead", 0)), count(usage.get("cacheWrite", 0))
    values = dict(input_tokens=count(usage.get("input", 0)) + cached + write,
                  cached_input_tokens=cached, cache_write_input_tokens=write,
                  output_tokens=count(usage.get("output", 0)),
                  reasoning_output_tokens=count(usage.get("reasoning", 0)))
    if not any(values.values()):
        return []
    key = ["pi", record["id"], record.get("timestamp"), message.get("provider"), model]
    return [event("pi", key, record.get("timestamp"), model, values)]


def database(directory):
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(directory, 0o700)
    db = sqlite3.connect(directory / "sync.sqlite", timeout=5)
    os.chmod(directory / "sync.sqlite", 0o600)
    db.execute("PRAGMA synchronous=FULL")
    db.executescript("""
      CREATE TABLE IF NOT EXISTS files(path TEXT PRIMARY KEY, inode TEXT, size INTEGER,
        mtime INTEGER, offset INTEGER, guard TEXT, state TEXT);
      CREATE TABLE IF NOT EXISTS outbox(id TEXT PRIMARY KEY, payload TEXT NOT NULL);
    """)
    return db


def guard(stream, offset):
    stream.seek(max(0, offset - 64))
    return hashlib.sha256(stream.read(min(offset, 64))).hexdigest()


def scan_file(db, path, source):
    stat = path.stat()
    inode = f"{stat.st_dev}:{stat.st_ino}"
    prior = db.execute("SELECT inode,size,mtime,offset,guard,state FROM files WHERE path=?",
                       (str(path),)).fetchone()
    if prior and (inode, stat.st_size, stat.st_mtime_ns) == prior[:3]:
        return 0
    offset, state, added = 0, {}, 0
    with path.open("rb") as stream, db:
        if prior and prior[0] == inode and stat.st_size >= prior[3] and guard(stream, prior[3]) == prior[4]:
            offset, state = prior[3], json.loads(prior[5])
        stream.seek(offset)
        while True:
            line = stream.readline()
            if not line or not line.endswith(b"\n"):
                break  # Never checkpoint an incomplete JSONL record.
            offset = stream.tell()
            if source == "codex" and not any(marker in line for marker in (
                    b'"token_usage_record"', b'"token_count"', b'"session_meta"',
                    b'"turn_context"', b'"thread_settings_applied"')):
                continue
            if source == "pi" and b'"usage"' not in line:
                continue
            record = json.loads(line)
            events = codex_record(record, state) if source == "codex" else pi_record(record, state)
            for value in events:
                queued = db.execute("SELECT payload FROM outbox WHERE id=?", (value["id"],)).fetchone()
                if queued:
                    original = json.loads(queued[0])
                    if any(original[k] != v for k, v in value.items() if k != "timestamp_ms"):
                        raise ValueError("Conflicting usage for the same provider event")
                    value["timestamp_ms"] = min(value["timestamp_ms"], original["timestamp_ms"])
                db.execute("INSERT OR REPLACE INTO outbox VALUES(?,?)", (value["id"], compact(value)))
                added += 1
        db.execute("INSERT OR REPLACE INTO files VALUES(?,?,?,?,?,?,?)",
                   (str(path), inode, stat.st_size, stat.st_mtime_ns, offset, guard(stream, offset), compact(state)))
    return added


def source_paths(config):
    for source, roots in config["sources"].items():
        for root in roots:
            path = Path(root).expanduser()
            if path.is_dir():
                for entry in sorted(path.rglob("*.jsonl")):
                    yield source, entry.resolve()


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError("Broker redirected the upload; use its direct HTTPS URL")


def post(config, route, body, token=None):
    context = ssl.create_default_context(cafile=config.get("ca") or None)
    opener = urllib.request.build_opener(urllib.request.HTTPSHandler(context=context), NoRedirect())
    data = gzip.compress(compact(body).encode(), mtime=0) if token else compact(body).encode()
    headers = {"Content-Type": "application/json"}
    if token:
        headers.update(Authorization="Bearer " + token, **{"Content-Encoding": "gzip"})
    request = urllib.request.Request(config["url"] + route, data=data, headers=headers)
    try:
        with opener.open(request, timeout=30) as response:
            return json.loads(response.read(1 << 20))
    except urllib.error.HTTPError as error:
        # Do not echo request bodies, identity codes, or device credentials.
        try:
            detail = json.loads(error.read(4096)).get("detail", "Upload rejected")
        except (ValueError, AttributeError):
            detail = "Upload rejected"
        raise ValueError(f"Broker HTTP {error.code}: {detail}") from None


def save_config(directory, config):
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(directory, 0o700)
    temporary = directory / "config.tmp"
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as stream:
        stream.write(compact(config))
        stream.flush()
        os.fsync(stream.fileno())
    temporary.replace(directory / "config.json")


def sync(directory, send=post):
    directory = Path(directory)
    config = json.loads((directory / "config.json").read_text())
    db = database(directory)
    # A database-backed lease serializes scheduled/manual sync processes and is
    # released by the OS after a crash; all upload checkpoints remain durable.
    lock = sqlite3.connect(directory / "lock.sqlite", timeout=1)
    try:
        lock.execute("BEGIN EXCLUSIVE")
        errors = []
        for source, path in source_paths(config):
            try:
                scan_file(db, path, source)
            except (OSError, ValueError, TypeError, AttributeError, sqlite3.Error) as error:
                db.rollback()
                errors.append(f"{source} file {path.name}: {type(error).__name__}; checkpoint unchanged")
        if config.get("enrollment_code"):
            device = send(config, "/api/v1/usage/devices", dict(
                code=config["enrollment_code"], installation_id=config["installation_id"],
                name=config["name"], token=config["token"]))
            config["device_id"] = device["id"]
            del config["enrollment_code"]
            save_config(directory, config)
        uploaded = 0
        while True:
            rows = db.execute("SELECT id,payload FROM outbox ORDER BY rowid LIMIT ?", (BATCH_SIZE,)).fetchall()
            if not rows:
                # Heartbeat makes an idle device's last-sync time meaningful.
                if uploaded == 0:
                    send(config, "/api/v1/usage/batches", dict(version=VERSION, events=[]), config["token"])
                break
            reply = send(config, "/api/v1/usage/batches",
                         dict(version=VERSION, events=[json.loads(row[1]) for row in rows]), config["token"])
            if reply.get("accepted") != len(rows):
                raise ValueError("Broker did not acknowledge the entire batch; retained for retry")
            with db:
                db.executemany("DELETE FROM outbox WHERE id=?", [(row[0],) for row in rows])
            uploaded += len(rows)
        if errors:
            raise ValueError("\n".join(errors))
        print(f"Usage sync complete: {uploaded} events acknowledged.")
    finally:
        lock.close()
        db.close()


def systemd_quote(value):
    return '"' + str(value).replace('\\', '\\\\').replace('"', '\\"').replace('%', '%%').replace('$', '$$') + '"'


def schedule(directory, remove=False):
    directory = Path(directory).resolve()
    script = directory / "usage-sync.py"
    python = str(Path(sys.executable).resolve())
    # Separate state directories get separate schedules for multiple identities.
    suffix = hashlib.sha256(str(directory).encode()).hexdigest()[:12]
    label = "dev.codex-broker.usage-" + suffix
    args = [python, str(script), "sync", "--state-dir", str(directory)]
    if sys.platform == "darwin":
        plist = Path.home() / "Library/LaunchAgents" / (label + ".plist")
        domain = "gui/" + str(os.getuid())
        subprocess.run(["launchctl", "bootout", domain + "/" + label], capture_output=True)
        if remove:
            plist.unlink(missing_ok=True)
            return
        plist.parent.mkdir(parents=True, exist_ok=True)
        plist.write_bytes(plistlib.dumps(dict(Label=label, ProgramArguments=args, RunAtLoad=True,
                                             StartInterval=3600, StandardOutPath=str(directory / "sync.log"),
                                             StandardErrorPath=str(directory / "sync.log"))))
        os.chmod(plist, 0o600)
        subprocess.run(["launchctl", "bootstrap", domain, str(plist)], check=True)
    elif sys.platform == "win32":
        name = "CodexBrokerUsage-" + suffix
        if remove:
            subprocess.run(["schtasks", "/Delete", "/TN", name, "/F"], check=True)
        else:
            task = ET.Element("Task", version="1.2", xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task")
            triggers = ET.SubElement(task, "Triggers")
            logon = ET.SubElement(triggers, "LogonTrigger")
            ET.SubElement(logon, "Enabled").text = "true"
            calendar = ET.SubElement(triggers, "CalendarTrigger")
            repetition = ET.SubElement(calendar, "Repetition")
            ET.SubElement(repetition, "Interval").text = "PT1H"
            ET.SubElement(calendar, "StartBoundary").text = dt.datetime.now().replace(hour=0, minute=0, second=0, microsecond=0).isoformat()
            ET.SubElement(calendar, "Enabled").text = "true"
            ET.SubElement(ET.SubElement(calendar, "ScheduleByDay"), "DaysInterval").text = "1"
            principal = ET.SubElement(ET.SubElement(task, "Principals"), "Principal", id="Author")
            user = os.environ.get("USERDOMAIN", ".") + "\\" + getpass.getuser()
            ET.SubElement(principal, "UserId").text = user
            ET.SubElement(logon, "UserId").text = user
            ET.SubElement(principal, "LogonType").text = "InteractiveToken"
            ET.SubElement(principal, "RunLevel").text = "LeastPrivilege"
            settings = ET.SubElement(task, "Settings")
            for key, value in dict(MultipleInstancesPolicy="IgnoreNew", DisallowStartIfOnBatteries="false",
                                   StopIfGoingOnBatteries="false", StartWhenAvailable="true", ExecutionTimeLimit="PT30M").items():
                ET.SubElement(settings, key).text = value
            execute = ET.SubElement(ET.SubElement(task, "Actions", Context="Author"), "Exec")
            ET.SubElement(execute, "Command").text = python
            ET.SubElement(execute, "Arguments").text = subprocess.list2cmdline(args[1:])
            xml = directory / "schedule.xml"
            xml.write_bytes(ET.tostring(task, encoding="utf-16", xml_declaration=True))
            try:
                subprocess.run(["schtasks", "/Create", "/TN", name, "/XML", str(xml), "/F"], check=True)
            finally:
                xml.unlink(missing_ok=True)
    elif shutil.which("systemctl"):
        unit = "codex-broker-usage-" + suffix
        units = Path.home() / ".config/systemd/user"
        if remove:
            subprocess.run(["systemctl", "--user", "disable", "--now", unit + ".timer"], check=True)
            for extension in (".timer", ".service"):
                (units / (unit + extension)).unlink(missing_ok=True)
        else:
            units.mkdir(parents=True, exist_ok=True)
            (units / (unit + ".service")).write_text("[Unit]\nDescription=Codex Broker usage sync\n[Service]\nType=oneshot\nExecStart=" + " ".join(map(systemd_quote, args)) + "\n")
            (units / (unit + ".timer")).write_text("[Unit]\nDescription=Retry Codex Broker usage uploads hourly\n[Timer]\nOnStartupSec=2min\nOnCalendar=hourly\nPersistent=true\nRandomizedDelaySec=120\n[Install]\nWantedBy=timers.target\n")
            subprocess.run(["systemctl", "--user", "daemon-reload"], check=True)
            subprocess.run(["systemctl", "--user", "enable", "--now", unit + ".timer"], check=True)
    else:
        raise ValueError("No supported scheduler. Run sync from cron hourly, or install with --no-schedule.")


def install(args):
    directory = args.state_dir.expanduser().resolve()
    target = directory / "usage-sync.py"
    if not (directory / "config.json").exists():
        address = args.broker_url or input("Broker HTTPS URL: ").strip()
        parts = urllib.parse.urlsplit(address)
        if parts.scheme != "https" or not parts.hostname or parts.username or parts.password or parts.query or parts.fragment:
            raise ValueError("Use a direct HTTPS Broker URL, optionally including its root path")
        code = sys.stdin.readline().strip() if args.code_stdin else getpass.getpass("Identity code (hidden): ").strip()
        if not code.startswith("cbi_") or len(code) != 47:
            raise ValueError("Enter the generated identity code, not its name")
        sources = {}
        print("Only direct sources are uploaded. Assign Broker proxy keys to this identity in Settings.")
        for agent in ("codex", "pi"):
            mode = getattr(args, agent + "_mode")
            if mode is None:
                mode = input(f"{agent} logs use [direct/broker/off]: ").strip().lower()
            if mode not in ("direct", "broker", "off"):
                raise ValueError("Choose direct, broker, or off for each agent; mixed routes require separate directories")
            if mode == "direct":
                roots = getattr(args, agent + "_path")
                if not roots:
                    if agent == "codex":
                        homes = os.environ.get("CODEX_HOME", str(Path.home() / ".codex")).split(",")
                        roots = [str(Path(h).expanduser() / child) for h in homes for child in ("sessions", "archived_sessions")]
                    else:
                        agent_root = Path(os.environ.get("PI_CODING_AGENT_DIR", str(Path.home() / ".pi/agent")))
                        roots = [os.environ.get("PI_CODING_AGENT_SESSION_DIR", str(agent_root / "sessions"))]
                sources[agent] = [str(Path(root).expanduser().resolve()) for root in roots]
        config = dict(url=address.rstrip("/"), name=args.name or socket.gethostname(),
                      enrollment_code=code, installation_id=secrets.token_hex(16),
                      token="cbu_" + secrets.token_urlsafe(32), sources=sources)
        if args.ca:
            directory.mkdir(parents=True, exist_ok=True, mode=0o700)
            ca = directory / "broker-ca.pem"
            shutil.copyfile(args.ca.expanduser(), ca)
            config["ca"] = str(ca)
        save_config(directory, config)
    else:
        print("Reusing this installation's identity, device credential, and source configuration.")
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    if Path(__file__).resolve() != target:
        shutil.copyfile(__file__, target)
    os.chmod(target, 0o700)
    # Set up retries before making a network call; initial enrollment may be offline.
    if not args.no_schedule:
        schedule(directory)
    print(f"Installed in {directory}. Initial sync follows; failures are retained for retry.")
    sync(directory)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("install", "sync", "rescan", "status", "uninstall"))
    parser.add_argument("--state-dir", type=Path, default=Path.home() / ".codex-broker-usage")
    parser.add_argument("--broker-url")
    parser.add_argument("--name")
    parser.add_argument("--ca", type=Path, help="Trusted private CA PEM; TLS verification is never disabled")
    parser.add_argument("--code-stdin", action="store_true", help="Read the identity code from stdin instead of prompting")
    parser.add_argument("--no-schedule", action="store_true", help="Use a scheduler you manage yourself")
    for agent in ("codex", "pi"):
        parser.add_argument("--" + agent + "-mode", choices=("direct", "broker", "off"))
        parser.add_argument("--" + agent + "-path", action="append", help="Direct-only session directory; may be repeated")
    args = parser.parse_args()
    os.umask(0o077)
    try:
        if args.command == "install":
            install(args)
        elif args.command == "sync":
            sync(args.state_dir)
        elif args.command == "rescan":
            with database(args.state_dir) as db:
                db.execute("DELETE FROM files")
            print("File checkpoints cleared; the next sync reimports retained logs with server deduplication.")
        elif args.command == "uninstall":
            schedule(args.state_dir, remove=True)
            print("Schedule removed. Local credentials, checkpoints, and queued events were retained.")
            print("Revoke the device in Broker Settings to stop future uploads.")
        else:
            with database(args.state_dir) as db:
                pending = db.execute("SELECT count(*) FROM outbox").fetchone()[0]
                files = db.execute("SELECT count(*) FROM files").fetchone()[0]
            print(f"{files} files tracked; {pending} events waiting to upload.")
    except (OSError, ValueError, sqlite3.Error, subprocess.SubprocessError) as error:
        print(f"Usage sync: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
