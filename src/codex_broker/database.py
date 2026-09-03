import asyncio
import hashlib
import queue
import sqlite3
import threading
from collections.abc import Callable
from concurrent.futures import Future
from pathlib import Path
from typing import Any, TypeVar

from codex_broker.clock import SystemClock
from codex_broker.ids import new_id

T = TypeVar("T")
DbJob = tuple[Callable[[sqlite3.Connection], Any], Future[Any]] | None


def _configure(connection: sqlite3.Connection) -> None:
    connection.row_factory = sqlite3.Row
    connection.execute("PRAGMA foreign_keys=ON")
    connection.execute("PRAGMA busy_timeout=5000")
    connection.execute("PRAGMA synchronous=FULL")
    connection.execute("PRAGMA trusted_schema=OFF")


def _statements(script: str) -> list[str]:
    statements: list[str] = []
    current = ""
    for line in script.splitlines():
        current += line + "\n"
        if sqlite3.complete_statement(current):
            if current.strip():
                statements.append(current)
            current = ""
    if current.strip():
        raise ValueError("incomplete migration statement")
    return statements


class Database:
    """One connection-owning thread and a small async interface."""

    def __init__(self, path: Path, migrations_dir: Path | None = None) -> None:
        self.path = path
        self.migrations_dir = migrations_dir or Path(__file__).parent / "migrations"
        self._jobs: queue.Queue[DbJob] = queue.Queue()
        self._thread: threading.Thread | None = None
        self._ready = threading.Event()
        self._startup_error: BaseException | None = None

    def start(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        self._migrate()
        self._thread = threading.Thread(target=self._run, name="windowkeeper-db", daemon=True)
        self._thread.start()
        self._ready.wait(timeout=10)
        if self._startup_error:
            raise self._startup_error

    def _migration_connection(self) -> sqlite3.Connection:
        connection = sqlite3.connect(self.path, isolation_level=None)
        _configure(connection)
        return connection

    def _migrate(self) -> None:
        connection = self._migration_connection()
        try:
            current = 0
            try:
                row = connection.execute("SELECT max(version) FROM schema_migrations").fetchone()
                current = int(row[0] or 0)
            except sqlite3.OperationalError as error:
                if "no such table" not in str(error):
                    raise
            files = sorted(self.migrations_dir.glob("[0-9][0-9][0-9]_*.sql"))
            for migration in files:
                version = int(migration.name.split("_", 1)[0])
                if version <= current:
                    continue
                script = migration.read_text(encoding="utf-8")
                checksum = hashlib.sha256(script.encode()).hexdigest()
                backup = self.path.with_suffix(f".pre-v{version}.db")
                if self.path.exists() and self.path.stat().st_size:
                    copy = sqlite3.connect(backup)
                    try:
                        connection.backup(copy)
                    finally:
                        copy.close()
                _statements(script)
                try:
                    connection.executescript(script)
                    connection.execute(
                        "INSERT INTO schema_migrations VALUES(?,?,?,?)",
                        (version, migration.stem, checksum, SystemClock().now_ms()),
                    )
                except (sqlite3.OperationalError, sqlite3.IntegrityError):
                    if backup.exists():
                        connection.close()
                        backup.replace(self.path)
                        connection = self._migration_connection()
                    raise
            self._seed_metadata(connection)
            result = connection.execute("PRAGMA quick_check").fetchone()
            if not result or result[0] != "ok":
                raise RuntimeError("database integrity check failed")
        finally:
            connection.close()

    def _seed_metadata(self, connection: sqlite3.Connection) -> None:
        now = SystemClock().now_ms()
        connection.execute(
            "INSERT OR IGNORE INTO instance_metadata VALUES(1,?,?,?)",
            (new_id(), now, "0.1.0"),
        )

    def _run(self) -> None:
        try:
            connection = sqlite3.connect(self.path, isolation_level=None, check_same_thread=True)
            _configure(connection)
            self._ready.set()
            while True:
                item = self._jobs.get()
                if item is None:
                    break
                job, future = item
                if not future.set_running_or_notify_cancel():
                    continue
                try:
                    future.set_result(job(connection))
                except BaseException as error:
                    future.set_exception(error)
            connection.close()
        except BaseException as error:
            self._startup_error = error
            self._ready.set()

    async def call(self, job: Callable[[sqlite3.Connection], T]) -> T:
        future: Future[T] = Future()
        self._jobs.put((job, future))
        return await asyncio.wrap_future(future)

    async def transaction(self, work: Callable[[sqlite3.Connection], T]) -> T:
        def job(connection: sqlite3.Connection) -> T:
            connection.execute("BEGIN IMMEDIATE")
            try:
                value = work(connection)
                connection.commit()
                return value
            except BaseException:
                connection.rollback()
                raise

        return await self.call(job)

    async def close(self) -> None:
        self._jobs.put(None)
        if self._thread:
            await asyncio.to_thread(self._thread.join, 10)
            self._thread = None
