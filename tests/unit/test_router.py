# pyright: reportMissingImports=false

import asyncio
import sqlite3
from pathlib import Path
from typing import Any, cast

import pytest

from codex_broker.credential_authority import Lease
from codex_broker.database import Database
from codex_broker.errors import WindowkeeperError
from codex_broker.router import PoolWait, RouteLease, Router, RouteRequest


class Authority:
    async def lease(
        self, account: dict[str, Any], now: int, *, force_refresh: bool = False
    ) -> Lease:
        del now, force_refresh
        return Lease(f"upstream-{account['public_token']}", "access", 9_999_999_999_000)


class Services:
    def __init__(self) -> None:
        self.refreshed: list[str] = []

    async def refresh(self, public: str, trigger: str = "USER") -> str:
        self.refreshed.append(f"{public}:{trigger}")
        return "operation"


def seed(connection: sqlite3.Connection, *, used: int = 10, reset: int = 9_999_999_999) -> None:
    connection.execute(
        "INSERT INTO client_api_keys VALUES('key','Pi','cbk_prefix',X'00',1,NULL,NULL)"
    )
    for index in range(2):
        account_id, public = f"account-{index}", f"public-{index}"
        connection.execute(
            "INSERT INTO accounts VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
            (
                account_id,
                public,
                public,
                "chatgpt",
                "CHATGPT_DEVICE_CODE",
                "CHATGPT_DEVICE_CODE",
                None,
                1,
                "ACTIVE",
                index + 1,
                index + 1,
                None,
            ),
        )
        connection.execute(
            "INSERT INTO account_state VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (
                account_id,
                "VERIFIED",
                "STOPPED",
                "HEALTHY",
                "FRESH",
                None,
                None,
                1,
                1,
                None,
                None,
                None,
                1,
                1,
            ),
        )
        connection.execute(
            "INSERT INTO usage_current(account_id,short_used_percent_raw,short_resets_at_s) VALUES(?,?,?)",
            (account_id, used, reset),
        )
        connection.execute(
            "INSERT INTO credential_bundles VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (
                f"bundle-{index}",
                account_id,
                "ACTIVE",
                1,
                1,
                "key",
                b"n",
                b"c",
                b"a",
                "v",
                1,
                1,
                None,
            ),
        )


@pytest.mark.asyncio
async def test_router_preserves_preference_and_moves_after_failure(tmp_path: Path) -> None:
    database = Database(tmp_path / "broker.db")
    database.start()
    services = Services()
    try:
        await database.transaction(seed)
        router = Router(database, services, cast(Any, Authority()), reset_padding_seconds=10)
        preferred = await router.route(
            "key", RouteRequest("session", "turn", preferred_account_id="public-1")
        )
        assert isinstance(preferred, RouteLease)
        assert preferred.account_id == "public-1"
        replacement = await router.route(
            "key",
            RouteRequest("session", "retry", failed_account_id="public-0", failure_kind="quota"),
        )
        assert isinstance(replacement, RouteLease)
        assert replacement.account_id == "public-1"
        assert services.refreshed == ["public-0:CLIENT_FAILURE"]
    finally:
        await database.close()


@pytest.mark.asyncio
async def test_concurrent_valid_routes_do_not_mutate_credentials(tmp_path: Path) -> None:
    database = Database(tmp_path / "broker.db")
    database.start()
    try:
        await database.transaction(seed)
        router = Router(database, Services(), cast(Any, Authority()))
        results = await asyncio.gather(
            *(router.route("key", RouteRequest("session", f"turn-{index}")) for index in range(50))
        )
        assert {result.account_id for result in results if isinstance(result, RouteLease)} == {
            "public-0"
        }
        assert await database.call(
            lambda connection: connection.execute(
                "SELECT count(*) FROM credential_bundles"
            ).fetchone()[0]
        ) == 2
    finally:
        await database.close()


@pytest.mark.asyncio
async def test_router_ignores_exhaustion_after_known_reset(tmp_path: Path) -> None:
    database = Database(tmp_path / "broker.db")
    database.start()
    try:
        now = 2_000_000
        await database.transaction(lambda connection: seed(connection, used=100, reset=1_900))
        router = Router(database, Services(), cast(Any, Authority()))
        router.clock.now_ms = lambda: now  # type: ignore[method-assign]
        assert isinstance(await router.route("key", RouteRequest("session", "turn")), RouteLease)
    finally:
        await database.close()


@pytest.mark.asyncio
async def test_router_rejects_exhaustion_without_reset(tmp_path: Path) -> None:
    database = Database(tmp_path / "broker.db")
    database.start()
    try:
        await database.transaction(lambda connection: seed(connection, used=100))
        await database.transaction(
            lambda connection: connection.execute(
                "UPDATE usage_current SET short_resets_at_s=NULL"
            )
        )
        router = Router(database, Services(), cast(Any, Authority()))
        with pytest.raises(WindowkeeperError, match="reliable retry time") as caught:
            await router.route("key", RouteRequest("session", "turn"))
        assert caught.value.code == "POOL_RESET_UNKNOWN"
    finally:
        await database.close()


@pytest.mark.asyncio
async def test_router_returns_earliest_padded_pool_reset(tmp_path: Path) -> None:
    database = Database(tmp_path / "broker.db")
    database.start()
    try:
        now = 2_000_000
        await database.transaction(lambda connection: seed(connection, used=100, reset=2_100))
        router = Router(database, Services(), cast(Any, Authority()), reset_padding_seconds=10)
        router.clock.now_ms = lambda: now  # type: ignore[method-assign]
        result = await router.route("key", RouteRequest("session", "turn"))
        assert result == PoolWait(2_110_000, 110)
    finally:
        await database.close()
