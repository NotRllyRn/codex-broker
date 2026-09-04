import hashlib
import hmac
import secrets
import sqlite3
from collections.abc import Callable
from dataclasses import dataclass
from typing import Protocol, TypeVar

from argon2 import PasswordHasher  # pyright: ignore[reportMissingImports]
from argon2.exceptions import (  # pyright: ignore[reportMissingImports]
    InvalidHashError,
    VerifyMismatchError,
)

from .clock import SystemClock
from .errors import WindowkeeperError

T = TypeVar("T")


class DatabasePort(Protocol):
    async def call(self, job: Callable[[sqlite3.Connection], T]) -> T: ...
    async def transaction(self, work: Callable[[sqlite3.Connection], T]) -> T: ...


class SessionSettings(Protocol):
    session_idle_minutes: int
    session_absolute_hours: int


_hasher = PasswordHasher(time_cost=3, memory_cost=65_536, parallelism=1, hash_len=32, salt_len=16)


def digest(value: str) -> bytes:
    return hashlib.sha256(value.encode()).digest()


@dataclass(frozen=True, slots=True)
class Session:
    token: str
    csrf_token: str
    created_at_ms: int
    idle_expires_at_ms: int
    absolute_expires_at_ms: int


class AdminSecurity:
    def __init__(self, database: DatabasePort, settings: SessionSettings) -> None:
        self.database = database
        self.settings = settings
        self.clock = SystemClock()

    async def configured(self) -> bool:
        return await self.database.call(
            lambda connection: (
                connection.execute(
                    "SELECT 1 FROM admin_credentials WHERE singleton_id=1 AND bootstrap_complete=1"
                ).fetchone()
                is not None
            )
        )

    async def bootstrap(self, password: str) -> None:
        if len(password) < 15 or len(password) > 128:
            raise ValueError("administrator password must contain 15-128 characters")
        password_hash = _hasher.hash(password)
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "INSERT INTO admin_credentials VALUES(1,?,?,1) ON CONFLICT(singleton_id) DO NOTHING",
                (password_hash, now),
            )

        await self.database.transaction(work)

    async def set_password(self, password: str) -> None:
        if len(password) < 15 or len(password) > 128:
            raise ValueError("administrator password must contain 15-128 characters")
        password_hash = _hasher.hash(password)
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "INSERT INTO admin_credentials VALUES(1,?,?,1) ON CONFLICT(singleton_id) DO UPDATE SET password_hash=excluded.password_hash,password_changed_at_ms=excluded.password_changed_at_ms,bootstrap_complete=1",
                (password_hash, now),
            )
            connection.execute(
                "UPDATE admin_sessions SET revoked_at_ms=? WHERE revoked_at_ms IS NULL", (now,)
            )

        await self.database.transaction(work)

    async def verify_password(self, password: str) -> bool:
        def work(connection: sqlite3.Connection) -> str | None:
            row = connection.execute(
                "SELECT password_hash FROM admin_credentials WHERE singleton_id=1"
            ).fetchone()
            return str(row[0]) if row else None

        stored = await self.database.call(work)
        if not stored:
            return False
        try:
            return _hasher.verify(stored, password)
        except (VerifyMismatchError, InvalidHashError):
            return False

    async def login(self, password: str, fingerprint: str = "") -> Session:
        if not await self.verify_password(password):
            raise WindowkeeperError("LOGIN_FAILED", "The password was not accepted", 401)
        now = self.clock.now_ms()
        token = secrets.token_urlsafe(32)
        csrf = secrets.token_urlsafe(32)
        session = Session(
            token,
            csrf,
            now,
            now + self.settings.session_idle_minutes * 60_000,
            now + self.settings.session_absolute_hours * 3_600_000,
        )

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "INSERT INTO admin_sessions VALUES(?,?,?,?,?,?,?,?,?)",
                (
                    digest(token),
                    digest(csrf),
                    now,
                    now,
                    session.idle_expires_at_ms,
                    session.absolute_expires_at_ms,
                    now,
                    None,
                    digest(fingerprint) if fingerprint else None,
                ),
            )

        await self.database.transaction(work)
        return session

    async def session(self, token: str | None) -> dict[str, object] | None:
        if not token:
            return None
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> dict[str, object] | None:
            row = connection.execute(
                "SELECT * FROM admin_sessions WHERE session_id_hash=? AND revoked_at_ms IS NULL AND idle_expires_at_ms>? AND absolute_expires_at_ms>?",
                (digest(token), now, now),
            ).fetchone()
            if not row:
                return None
            try:
                last_seen = int(row["last_seen_at_ms"])
                absolute_expiry = int(row["absolute_expires_at_ms"])
            except (TypeError, ValueError) as error:
                raise RuntimeError("stored session timestamp is invalid") from error
            if now - last_seen > 60_000:
                connection.execute(
                    "UPDATE admin_sessions SET last_seen_at_ms=?,idle_expires_at_ms=? WHERE session_id_hash=?",
                    (
                        now,
                        min(
                            now + self.settings.session_idle_minutes * 60_000,
                            absolute_expiry,
                        ),
                        digest(token),
                    ),
                )
            return dict(row)

        return await self.database.call(work)

    async def require_csrf(self, token: str, supplied: str | None) -> None:
        current = await self.session(token)
        stored = current.get("csrf_token_hash") if current else None
        if (
            not supplied
            or not isinstance(stored, bytes)
            or not hmac.compare_digest(stored, digest(supplied))
        ):
            raise WindowkeeperError("CSRF_INVALID", "The request could not be verified", 403)

    async def logout(self, token: str) -> None:
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "UPDATE admin_sessions SET revoked_at_ms=? WHERE session_id_hash=?",
                (now, digest(token)),
            )

        await self.database.transaction(work)
