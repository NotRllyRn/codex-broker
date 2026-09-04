# pyright: reportMissingImports=false

import hashlib
import hmac
import secrets
import sqlite3
from collections.abc import Callable
from dataclasses import dataclass
from typing import Protocol, TypeVar

from codex_broker.clock import SystemClock
from codex_broker.errors import BrokerError
from codex_broker.ids import new_id

T = TypeVar("T")
PREFIX = "cbk_"


class DatabasePort(Protocol):
    async def call(self, job: Callable[[sqlite3.Connection], T]) -> T: ...
    async def transaction(self, work: Callable[[sqlite3.Connection], T]) -> T: ...


def key_digest(value: str) -> bytes:
    return hashlib.sha256(value.encode()).digest()


@dataclass(frozen=True, slots=True)
class IssuedClientKey:
    key_id: str
    name: str
    key_prefix: str
    token: str
    created_at_ms: int


class ClientKeyService:
    def __init__(self, database: DatabasePort) -> None:
        self.database = database
        self.clock = SystemClock()

    async def create(self, display_name: str) -> IssuedClientKey:
        name = " ".join(display_name.split())
        if not name or len(name) > 80:
            raise ValueError("client key name must contain 1-80 characters")
        now, key_id = self.clock.now_ms(), new_id()
        token = PREFIX + secrets.token_urlsafe(32)
        prefix = token[:12]
        await self.database.transaction(
            lambda connection: connection.execute(
                "INSERT INTO client_api_keys VALUES(?,?,?,?,?,NULL,NULL)",
                (key_id, name, prefix, key_digest(token), now),
            )
        )
        return IssuedClientKey(key_id, name, prefix, token, now)

    async def authenticate(self, token: str) -> dict[str, object]:
        now, supplied = self.clock.now_ms(), key_digest(token)

        def work(connection: sqlite3.Connection) -> dict[str, object] | None:
            rows = connection.execute(
                "SELECT * FROM client_api_keys WHERE revoked_at_ms IS NULL"
            ).fetchall()
            row = next(
                (
                    item
                    for item in rows
                    if hmac.compare_digest(bytes(item["secret_hash"]), supplied)
                ),
                None,
            )
            if row:
                connection.execute(
                    "UPDATE client_api_keys SET last_used_at_ms=? WHERE key_id=?",
                    (now, row["key_id"]),
                )
                return dict(row)
            return None

        key = await self.database.transaction(work)
        if not key:
            raise BrokerError("CLIENT_KEY_INVALID", "Client authentication failed", 401)
        return key

    async def revoke(self, key_id: str) -> bool:
        now = self.clock.now_ms()
        return await self.database.transaction(
            lambda connection: bool(
                connection.execute(
                    "UPDATE client_api_keys SET revoked_at_ms=? WHERE key_id=? AND revoked_at_ms IS NULL",
                    (now, key_id),
                ).rowcount
            )
        )

    async def delete_revoked(self, key_id: str) -> bool:
        return await self.database.transaction(
            lambda connection: bool(
                connection.execute(
                    "DELETE FROM client_api_keys WHERE key_id=? AND revoked_at_ms IS NOT NULL",
                    (key_id,),
                ).rowcount
            )
        )

    async def list(self) -> list[dict[str, object]]:
        return await self.database.call(
            lambda connection: [
                dict(row)
                for row in connection.execute(
                    "SELECT key_id,name,key_prefix,created_at_ms,last_used_at_ms,revoked_at_ms FROM client_api_keys ORDER BY created_at_ms,key_id"
                )
            ]
        )
