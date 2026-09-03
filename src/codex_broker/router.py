# pyright: reportMissingImports=false

import math
import sqlite3
from dataclasses import dataclass
from typing import Any, Literal, Protocol

from codex_broker.clock import SystemClock
from codex_broker.credential_authority import CredentialAuthority
from codex_broker.database import Database
from codex_broker.errors import WindowkeeperError

FailureKind = Literal["quota", "auth", "rate_limit"]


class UsageServices(Protocol):
    async def refresh(self, public: str, trigger: str = "USER") -> str: ...


@dataclass(frozen=True, slots=True)
class RouteRequest:
    session_id: str
    turn_id: str
    preferred_account_id: str | None = None
    failed_account_id: str | None = None
    failure_kind: FailureKind | None = None


@dataclass(frozen=True, slots=True)
class RouteLease:
    account_id: str
    access_token: str
    chatgpt_account_id: str
    expires_at_ms: int


@dataclass(frozen=True, slots=True)
class PoolWait:
    next_retry_at_ms: int
    retry_after_seconds: int


class Router:
    def __init__(
        self,
        database: Database,
        services: UsageServices,
        authority: CredentialAuthority,
        reset_padding_seconds: int = 10,
    ) -> None:
        self.database = database
        self.services = services
        self.authority = authority
        self.reset_padding_seconds = reset_padding_seconds
        self.clock = SystemClock()

    async def route(
        self, key_id: str, request: RouteRequest
    ) -> RouteLease | PoolWait:
        now = self.clock.now_ms()
        rows = await self._accounts(key_id, now)
        failed = next(
            (row for row in rows if row["public_token"] == request.failed_account_id), None
        )
        if request.failed_account_id and not failed:
            raise WindowkeeperError("FAILED_ACCOUNT_INVALID", "Failed account is not routable", 422)
        if failed and request.failure_kind:
            if request.failure_kind == "quota":
                await self.services.refresh(str(failed["public_token"]), "CLIENT_FAILURE")
            if request.failure_kind == "auth":
                try:
                    lease = await self.authority.lease(failed, now, force_refresh=True)
                    return RouteLease(
                        str(failed["public_token"]),
                        lease.access_token,
                        lease.account_id,
                        lease.expires_at_ms,
                    )
                except Exception:
                    await self._mark_auth_required(str(failed["account_id"]), now)
            await self._exclude(key_id, failed, request.failure_kind, now)
            rows = await self._accounts(key_id, now)

        usable = [row for row in rows if not self._exhausted(row) and not row["excluded_until"]]
        selected = self._select(usable, request.preferred_account_id, request.failed_account_id, rows)
        if selected:
            lease = await self.authority.lease(selected, now)
            return RouteLease(
                str(selected["public_token"]),
                lease.access_token,
                lease.account_id,
                lease.expires_at_ms,
            )
        next_retry = self._next_retry(rows, now)
        if next_retry is None:
            raise WindowkeeperError(
                "POOL_RESET_UNKNOWN", "No routable account has a reliable retry time", 503
            )
        retry_after = max(0, math.ceil((next_retry - now) / 1000))
        return PoolWait(next_retry, retry_after)

    async def _accounts(self, key_id: str, now: int) -> list[dict[str, Any]]:
        def read(connection: sqlite3.Connection) -> list[dict[str, Any]]:
            connection.execute("DELETE FROM account_exclusions WHERE expires_at_ms<=?", (now,))
            return [
                dict(row)
                for row in connection.execute(
                    """SELECT a.*,s.auth_state,s.worker_state,u.short_used_percent_raw,
                    u.short_resets_at_s,u.weekly_used_percent_raw,u.weekly_resets_at_s,
                    e.expires_at_ms AS excluded_until
                    FROM accounts a JOIN account_state s USING(account_id)
                    JOIN credential_bundles b ON b.account_id=a.account_id AND b.state='ACTIVE'
                    LEFT JOIN usage_current u USING(account_id)
                    LEFT JOIN account_exclusions e ON e.account_id=a.account_id AND e.key_id=?
                    WHERE a.deleted_at_ms IS NULL AND a.enabled=1 AND s.auth_state='VERIFIED'
                    AND s.worker_state='STOPPED'
                    ORDER BY a.created_at_ms,a.account_id""",
                    (key_id,),
                )
            ]

        return await self.database.transaction(read)

    @staticmethod
    def _exhausted(row: dict[str, Any]) -> bool:
        return any(
            isinstance(value, int) and value >= 100
            for value in (row.get("short_used_percent_raw"), row.get("weekly_used_percent_raw"))
        )

    @staticmethod
    def _select(
        usable: list[dict[str, Any]],
        preferred: str | None,
        failed: str | None,
        all_rows: list[dict[str, Any]],
    ) -> dict[str, Any] | None:
        if preferred and not failed:
            match = next((row for row in usable if row["public_token"] == preferred), None)
            if match:
                return match
        if failed and usable:
            order = [str(row["public_token"]) for row in all_rows]
            start = (order.index(failed) + 1) % len(order)
            return min(usable, key=lambda row: (order.index(str(row["public_token"])) - start) % len(order))
        return usable[0] if usable else None

    @staticmethod
    def _account_reset(row: dict[str, Any], now: int) -> int | None:
        resets = [
            reset * 1000
            for used, reset in (
                (row.get("short_used_percent_raw"), row.get("short_resets_at_s")),
                (row.get("weekly_used_percent_raw"), row.get("weekly_resets_at_s")),
            )
            if isinstance(used, int)
            and used >= 100
            and isinstance(reset, int)
            and reset * 1000 > now
        ]
        return max(resets) if resets else None

    def _next_retry(self, rows: list[dict[str, Any]], now: int) -> int | None:
        candidates = [
            value
            for row in rows
            for value in (self._account_reset(row, now), row.get("excluded_until"))
            if isinstance(value, int) and value > now
        ]
        return min(candidates) + self.reset_padding_seconds * 1000 if candidates else None

    async def _exclude(
        self, key_id: str, account: dict[str, Any], failure: FailureKind, now: int
    ) -> None:
        reset = self._account_reset(account, now)
        expires = reset or now + (60_000 if failure == "rate_limit" else 300_000)
        await self.database.transaction(
            lambda connection: connection.execute(
                "INSERT INTO account_exclusions VALUES(?,?,?,?,?) ON CONFLICT(key_id,account_id) DO UPDATE SET failure_kind=excluded.failure_kind,expires_at_ms=excluded.expires_at_ms,created_at_ms=excluded.created_at_ms",
                (key_id, account["account_id"], failure, expires, now),
            )
        )

    async def _mark_auth_required(self, account_id: str, now: int) -> None:
        await self.database.transaction(
            lambda connection: connection.execute(
                "UPDATE account_state SET auth_state='AUTH_REQUIRED',overall_state='ACTION_REQUIRED',last_error_code='CODEX_AUTH_REQUIRED',last_error_summary='Client request authentication failed',updated_at_ms=?,state_version=state_version+1 WHERE account_id=?",
                (now, account_id),
            )
        )
