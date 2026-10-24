import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sqlite3
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("usage_sync", Path(__file__).resolve().parents[1] / "internal/webassets/static/usage-sync.py")
sync = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sync)

WHEN = "2026-09-01T10:00:00Z"


def line(kind, payload, when=WHEN):
    return dict(type=kind, timestamp=when, payload=payload)


def modern(response="response1", output=5):
    return line("token_usage_record", dict(response_id=response,
                usage=dict(input_tokens=100, cached_input_tokens=80, output_tokens=output)))


def codex_prefix():
    return [line("session_meta", dict(id="session1")), line("turn_context", dict(model="gpt-test"))]


def pi(identifier="entry1", output=7):
    return dict(type="message", id=identifier, timestamp=WHEN,
                message=dict(role="assistant", provider="openai", model="gpt-test",
                             usage=dict(input=10, cacheRead=30, output=output)))


def write(path, records, mode="w"):
    with path.open(mode) as stream:
        for record in records:
            stream.write(json.dumps(record) + "\n")


class Parsing(unittest.TestCase):
    def test_modern_codex_ignores_mirrors_and_keeps_compaction(self):
        state = {}
        for row in codex_prefix():
            sync.codex_record(row, state)
        first = sync.codex_record(modern(), state)[0]
        self.assertEqual(first["output_tokens"], 5)
        mirror = line("event_msg", dict(type="token_count", info=dict(last_token_usage=dict(input_tokens=100, output_tokens=5))))
        self.assertEqual(sync.codex_record(mirror, state), [])
        compaction = modern("compaction-response", 2)
        compaction["payload"]["usage"]["input_tokens"] = 20
        compaction["payload"]["usage"]["cached_input_tokens"] = 0
        self.assertEqual(sync.codex_record(compaction, state)[0]["output_tokens"], 2)
        self.assertEqual(sync.codex_record(modern(), state)[0]["id"], first["id"])

    def test_legacy_uses_advancing_cumulative_totals(self):
        state = {}
        for row in codex_prefix():
            sync.codex_record(row, state)
        def usage(total, latest):
            return line("event_msg", dict(type="token_count", info=dict(
                total_token_usage=dict(input_tokens=total*10, output_tokens=total),
                last_token_usage=dict(input_tokens=latest*10, output_tokens=latest))))
        self.assertEqual(sync.codex_record(usage(5, 5), state)[0]["output_tokens"], 5)
        self.assertEqual(sync.codex_record(usage(5, 5), state), [])
        self.assertEqual(sync.codex_record(usage(12, 99), state)[0]["output_tokens"], 7)

    def test_legacy_fork_prefix_does_not_charge_parent(self):
        state = {}
        sync.codex_record(line("session_meta", dict(id="child", forked_from_id="parent")), state)
        sync.codex_record(line("turn_context", dict(model="gpt-test")), state)
        copied = line("event_msg", dict(type="token_count", info=dict(
            total_token_usage=dict(input_tokens=100, output_tokens=10),
            last_token_usage=dict(input_tokens=100, output_tokens=10))))
        self.assertEqual(sync.codex_record(copied, state), [])
        actual = line("event_msg", dict(type="token_count", info=dict(
            total_token_usage=dict(input_tokens=150, output_tokens=13),
            last_token_usage=dict(input_tokens=50, output_tokens=3))), "2026-09-01T10:01:00Z")
        self.assertEqual(sync.codex_record(actual, state)[0]["output_tokens"], 3)

    def test_pi_includes_tools_and_compaction_without_adding_cache_twice(self):
        value = sync.pi_record(pi(), {})[0]
        self.assertEqual(value["input_tokens"], 40)
        self.assertEqual(value["cached_input_tokens"], 30)
        tool = pi("tool"); tool["message"]["role"] = "toolResult"
        self.assertEqual(sync.pi_record(tool, {})[0]["output_tokens"], 7)
        compact = dict(type="compaction", id="compact", timestamp=WHEN, usage=dict(input=10, output=3))
        self.assertEqual(sync.pi_record(compact, {})[0]["output_tokens"], 3)
        malformed = pi(); malformed["message"]["usage"]["input"] = -1
        with self.assertRaises(ValueError):
            sync.pi_record(malformed, {})


class DurableSync(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.logs = self.root / "logs"
        self.logs.mkdir()
        self.state = self.root / "state"
        self.config = dict(url="https://broker.example", name="Laptop", token="cbu_token",
                           sources=dict(codex=[str(self.logs)]))
        sync.save_config(self.state, self.config)

    def test_partial_line_appends_and_rewrites(self):
        path = self.logs / "session.jsonl"
        write(path, codex_prefix() + [modern()])
        db = sync.database(self.state)
        self.addCleanup(db.close)
        sync.scan_file(db, path, "codex")
        self.assertEqual(db.execute("SELECT count(*) FROM outbox").fetchone()[0], 1)
        with db:
            db.execute("DELETE FROM outbox")
        with path.open("a") as f:
            f.write(json.dumps(modern("second")))
        sync.scan_file(db, path, "codex")
        self.assertEqual(db.execute("SELECT count(*) FROM outbox").fetchone()[0], 0)
        with path.open("a") as f:
            f.write("\n")
        sync.scan_file(db, path, "codex")
        self.assertEqual(db.execute("SELECT count(*) FROM outbox").fetchone()[0], 1)
        write(path, codex_prefix() + [modern("replacement")])
        sync.scan_file(db, path, "codex")
        self.assertEqual(db.execute("SELECT count(*) FROM outbox").fetchone()[0], 2)

    def test_outage_lost_ack_and_weeks_of_history(self):
        write(self.logs / "old.jsonl", codex_prefix() + [modern("old")])
        def offline(*args):
            raise OSError("offline")
        with self.assertRaises(OSError):
            sync.sync(self.state, offline)
        with contextlib.closing(sync.database(self.state)) as db:
            self.assertEqual(db.execute("SELECT count(*) FROM outbox").fetchone()[0], 1)
        # Even deleting the original after durable scanning does not lose usage.
        (self.logs / "old.jsonl").unlink()
        write(self.logs / "new.jsonl", codex_prefix() + [modern("new")])
        server, lose = {}, [True]
        def send(config, route, body, token=None):
            for row in body["events"]:
                server.setdefault(row["id"], row)
            if lose.pop() if lose else False:
                raise OSError("reply lost after commit")
            return dict(accepted=len(body["events"]))
        with self.assertRaises(OSError):
            sync.sync(self.state, send)
        with contextlib.redirect_stdout(io.StringIO()):
            sync.sync(self.state, send)
            sync.sync(self.state, send)
        self.assertEqual(sum(e["output_tokens"] for e in server.values()), 10)
        with contextlib.closing(sync.database(self.state)) as db:
            self.assertEqual(db.execute("SELECT count(*) FROM outbox").fetchone()[0], 0)

    def test_enrollment_retries_same_secret_then_removes_identity_code(self):
        self.config.update(enrollment_code="cbi_private", installation_id="installation")
        sync.save_config(self.state, self.config)
        enrollments = []
        def send(config, route, body, token=None):
            if route.endswith("devices"):
                enrollments.append(body.copy())
                if len(enrollments) == 1:
                    raise OSError("reply lost")
                return dict(id="device")
            return dict(accepted=0)
        with self.assertRaises(OSError):
            sync.sync(self.state, send)
        with contextlib.redirect_stdout(io.StringIO()):
            sync.sync(self.state, send)
        self.assertEqual(enrollments[0], enrollments[1])
        saved = json.loads((self.state / "config.json").read_text())
        self.assertNotIn("enrollment_code", saved)
        if os.name != "nt":
            self.assertEqual((self.state / "config.json").stat().st_mode & 0o777, 0o600)

    def test_invalid_file_does_not_advance_checkpoint(self):
        path = self.logs / "bad.jsonl"
        write(path, codex_prefix() + [modern()])
        with path.open("a") as stream:
            stream.write('{"type":"token_usage_record",bad}\n')
        db = sync.database(self.state)
        self.addCleanup(db.close)
        with self.assertRaises(ValueError):
            sync.scan_file(db, path, "codex")
        self.assertEqual(db.execute("SELECT count(*) FROM outbox").fetchone()[0], 0)
        self.assertEqual(db.execute("SELECT count(*) FROM files").fetchone()[0], 0)

    def test_overlap_and_changed_copy_does_not_duplicate(self):
        write(self.logs / "one.jsonl", codex_prefix() + [modern()])
        copy = modern(); copy["timestamp"] = "2026-09-02T10:00:00Z"
        write(self.logs / "copy.jsonl", codex_prefix() + [copy])
        db = sync.database(self.state)
        self.addCleanup(db.close)
        for path in self.logs.glob("*.jsonl"):
            sync.scan_file(db, path, "codex")
        self.assertEqual(db.execute("SELECT count(*) FROM outbox").fetchone()[0], 1)

    def test_no_redirect_or_unacknowledged_batch_is_discarded(self):
        with self.assertRaises(ValueError):
            sync.NoRedirect().redirect_request(None, None, 302, "", {}, "https://other.example")
        write(self.logs / "one.jsonl", codex_prefix() + [modern()])
        with self.assertRaises(ValueError):
            sync.sync(self.state, lambda *args: dict(accepted=0))
        with contextlib.closing(sync.database(self.state)) as db:
            self.assertEqual(db.execute("SELECT count(*) FROM outbox").fetchone()[0], 1)

    def test_schedule_commands_contain_no_credentials(self):
        for platform in ("linux", "darwin", "win32"):
            with self.subTest(platform=platform), patch.object(sync.sys, "platform", platform), \
                 patch.object(sync.Path, "home", return_value=self.root), \
                 patch.object(sync.shutil, "which", return_value="systemctl"), \
                 patch.object(sync.subprocess, "run") as run:
                sync.schedule(self.state)
                self.assertTrue(run.called)
                for call in run.call_args_list:
                    self.assertNotIn("cbu_token", str(call))
                    self.assertNotIn("cbi_", str(call))


if __name__ == "__main__":
    unittest.main()
