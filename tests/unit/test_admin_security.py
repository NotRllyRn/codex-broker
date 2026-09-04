from pathlib import Path

import pytest

from codex_broker.config import Settings
from codex_broker.database import Database
from codex_broker.security import AdminSecurity, digest

PASSWORD = "correct horse battery staple"  # noqa: S105
NEW_PASSWORD = "another correct horse battery staple"  # noqa: S105


@pytest.mark.asyncio
async def test_idle_touch_never_exceeds_absolute_expiry(tmp_path: Path) -> None:
    database = Database(tmp_path / "broker.db")
    database.start()
    settings = Settings(
        data_dir=tmp_path / "data",
        runtime_dir=tmp_path / "run",
        session_idle_minutes=31 * 24 * 60,
        session_absolute_hours=1,
    )
    security = AdminSecurity(database, settings)
    token = "session-token"  # noqa: S105
    absolute = 3_600_000
    try:
        await database.transaction(
            lambda connection: connection.execute(
                "INSERT INTO admin_sessions VALUES(?,?,?,?,?,?,?,?,?)",
                (digest(token), digest("csrf"), 0, 1, absolute, absolute, 1, None, None),
            )
        )
        security.clock.now_ms = lambda: 1_000_000  # type: ignore[method-assign]
        assert await security.session(token)
        idle, stored_absolute = await database.call(
            lambda connection: tuple(
                connection.execute(
                    "SELECT idle_expires_at_ms,absolute_expires_at_ms FROM admin_sessions"
                ).fetchone()
            )
        )
        assert idle == stored_absolute == absolute
    finally:
        await database.close()


@pytest.mark.asyncio
async def test_logout_and_password_change_revoke_sessions(tmp_path: Path) -> None:
    database = Database(tmp_path / "broker.db")
    database.start()
    settings = Settings(data_dir=tmp_path / "data", runtime_dir=tmp_path / "run")
    security = AdminSecurity(database, settings)
    try:
        await security.bootstrap(PASSWORD)
        logged_out = await security.login(PASSWORD)
        await security.logout(logged_out.token)
        assert await security.session(logged_out.token) is None

        revoked = await security.login(PASSWORD)
        await security.set_password(NEW_PASSWORD)
        assert await security.session(revoked.token) is None
        assert not await security.verify_password(PASSWORD)
        assert await security.verify_password(NEW_PASSWORD)
    finally:
        await database.close()
