import json
import logging
import os
import queue
import threading
import time
from collections import deque
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, TextIO

from .redaction import redact

MAX_FILE_BYTES = 25 * 1024 * 1024
MAX_DIRECTORY_BYTES = 1024 * 1024 * 1024
RETENTION_SECONDS = 30 * 24 * 60 * 60


class LogBook(logging.Handler):
    def __init__(self, directory: Path, maximum_events: int = 2_000) -> None:
        super().__init__()
        self.directory = directory
        self.events: deque[dict[str, Any]] = deque(maxlen=maximum_events)
        self._queue: queue.Queue[dict[str, Any] | None] = queue.Queue(maxsize=10_000)
        self._dropped = 0
        self._thread = threading.Thread(target=self._write, name="codex-broker-log", daemon=True)

    def start(self) -> None:
        self.directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        self._repair(self.directory / "windowkeeper.jsonl")
        self._prune()
        self._thread.start()

    def emit(self, record: logging.LogRecord) -> None:
        event = redact(
            {
                "schema": "codex-broker.log/v1",
                "ts": datetime.now(UTC).isoformat(),
                "level": record.levelname,
                "event": getattr(record, "event", record.name),
                "message": record.getMessage(),
            }
        )
        try:
            self._queue.put_nowait(event)
        except queue.Full as overflow:
            del overflow
            self._dropped += 1

    @staticmethod
    def _repair(path: Path) -> None:
        try:
            descriptor = os.open(path, os.O_RDWR | os.O_CLOEXEC | os.O_NOFOLLOW)
        except FileNotFoundError:
            return
        try:
            size = os.lseek(descriptor, 0, os.SEEK_END)
            if not size:
                return
            os.lseek(descriptor, -1, os.SEEK_END)
            if os.read(descriptor, 1) == b"\n":
                return
            position = size
            while position:
                amount = min(4096, position)
                position -= amount
                os.lseek(descriptor, position, os.SEEK_SET)
                chunk = os.read(descriptor, amount)
                if (newline := chunk.rfind(b"\n")) >= 0:
                    os.ftruncate(descriptor, position + newline + 1)
                    return
            os.ftruncate(descriptor, 0)
        finally:
            os.close(descriptor)

    def _prune(self) -> None:
        now = time.time()
        files: list[tuple[Path, os.stat_result]] = []
        for path in self.directory.glob("windowkeeper-*.jsonl"):
            try:
                metadata = path.stat(follow_symlinks=False)
                if not path.is_symlink() and now - metadata.st_mtime > RETENTION_SECONDS:
                    path.unlink()
                else:
                    files.append((path, metadata))
            except OSError:
                continue
        total = sum(metadata.st_size for _, metadata in files)
        for path, metadata in sorted(files, key=lambda item: item[1].st_mtime):
            if total <= MAX_DIRECTORY_BYTES:
                break
            try:
                path.unlink()
                total -= metadata.st_size
            except OSError:
                continue

    def _open(self, path: Path) -> TextIO:
        descriptor = os.open(
            path,
            os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_CLOEXEC | os.O_NOFOLLOW,
            0o600,
        )
        return os.fdopen(descriptor, "a", encoding="utf-8")

    def _rotate(self, stream: TextIO, path: Path, day: str) -> tuple[TextIO, str, int]:
        stream.flush()
        os.fsync(stream.fileno())
        stream.close()
        rotated = self.directory / f"windowkeeper-{day}-{time.time_ns()}.jsonl"
        os.replace(path, rotated)
        self._prune()
        current_day = datetime.now(UTC).date().isoformat()
        return self._open(path), current_day, 0

    def _write(self) -> None:
        path = self.directory / "windowkeeper.jsonl"
        day = datetime.now(UTC).date().isoformat()
        try:
            if path.exists():
                previous_day = datetime.fromtimestamp(path.stat().st_mtime, UTC).date().isoformat()
                if previous_day != day:
                    os.replace(
                        path,
                        self.directory / f"windowkeeper-{previous_day}-{time.time_ns()}.jsonl",
                    )
        except OSError as error:
            del error
        stream = self._open(path)
        size = path.stat().st_size
        try:
            while (event := self._queue.get()) is not None:
                batch: list[dict[str, Any]] = []
                if self._dropped:
                    batch.append(
                        {
                            "schema": "codex-broker.log/v1",
                            "ts": datetime.now(UTC).isoformat(),
                            "level": "WARNING",
                            "event": "log.queue_overflow",
                            "message": f"Dropped {self._dropped} log events",
                        }
                    )
                    self._dropped = 0
                batch.append(event)
                for item in batch:
                    line = json.dumps(item, separators=(",", ":"), ensure_ascii=False) + "\n"
                    current_day = datetime.now(UTC).date().isoformat()
                    if current_day != day or size + len(line.encode()) > MAX_FILE_BYTES:
                        stream, day, size = self._rotate(stream, path, day)
                    self.events.append(item)
                    stream.write(line)
                    stream.flush()
                    size += len(line.encode())
        finally:
            stream.flush()
            stream.close()

    def recent(
        self, *, level: str | None = None, query: str = "", limit: int = 100
    ) -> list[dict[str, Any]]:
        return [
            event
            for event in reversed(self.events)
            if (not level or event["level"] == level)
            and (not query or query.lower() in json.dumps(event).lower())
        ][: min(limit, self.events.maxlen or 2_000)]

    def close(self) -> None:
        logging.getLogger().removeHandler(self)
        self._queue.put(None)
        if self._thread.is_alive():
            self._thread.join(timeout=5)
        super().close()


def configure_logging(directory: Path, level: str = "INFO") -> LogBook:
    logbook = LogBook(directory)
    logbook.start()
    root = logging.getLogger()
    root.setLevel(level)
    root.addHandler(logbook)
    return logbook
