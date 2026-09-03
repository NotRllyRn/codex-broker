import asyncio
import shutil
import sqlite3
import threading
from pathlib import Path

import pytest

from codex_broker.database import Database


@pytest.mark.asyncio
async def test_migrations_are_idempotent_and_foreign_keys_hold(tmp_path: Path) -> None:
    path = tmp_path / "windowkeeper.db"
    database = Database(path)
    database.start()
    version = await database.call(
        lambda connection: connection.execute(
            "SELECT max(version) FROM schema_migrations"
        ).fetchone()[0]
    )
    foreign_keys = await database.call(
        lambda connection: connection.execute("PRAGMA foreign_keys").fetchone()[0]
    )
    await database.close()
    assert version == 8
    assert foreign_keys == 1
    second = Database(path)
    second.start()
    assert (
        await second.call(lambda connection: connection.execute("PRAGMA quick_check").fetchone()[0])
        == "ok"
    )
    await second.close()


@pytest.mark.asyncio
async def test_migration_six_preserves_all_credential_generations(tmp_path: Path) -> None:
    old_migrations = tmp_path / "migrations"
    old_migrations.mkdir()
    source = Path(__file__).parents[2] / "src" / "codex_broker" / "migrations"
    for migration in sorted(source.glob("00[1-5]_*.sql")):
        shutil.copy2(migration, old_migrations)
    path = tmp_path / "windowkeeper.db"
    old = Database(path, old_migrations)
    old.start()

    def seed(connection: sqlite3.Connection) -> None:
        connection.execute(
            "INSERT INTO accounts VALUES('a','p','Account','chatgpt','CHATGPT_DEVICE_CODE',NULL,NULL,1,'ACTIVE',0,0,NULL)"
        )
        for bundle_id, state, nonce in (
            ("active", "ACTIVE", b"a"),
            ("export", "EXPORT", b"e"),
            ("retired", "RETIRED", b"r"),
        ):
            connection.execute(
                "INSERT INTO credential_bundles VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (bundle_id, "a", state, 1, 1, "primary", nonce, b"cipher", b"aad", "test", 0, 0, 0),
            )

    await old.call(seed)
    await old.close()
    migrated = Database(path)
    migrated.start()
    rows = await migrated.call(
        lambda connection: connection.execute(
            "SELECT bundle_id,state FROM credential_bundles ORDER BY bundle_id"
        ).fetchall()
    )
    await migrated.close()
    assert [(row[0], row[1]) for row in rows] == [
        ("active", "ACTIVE"),
        ("export", "EXPORT"),
        ("retired", "RETIRED"),
    ]


@pytest.mark.asyncio
async def test_cancelling_an_executing_job_does_not_stop_database_worker(tmp_path: Path) -> None:
    database = Database(tmp_path / "windowkeeper.db")
    database.start()
    started = threading.Event()
    release = threading.Event()

    def slow_job(connection: object) -> int:
        del connection
        started.set()
        release.wait(5)
        return 1

    task = asyncio.create_task(database.call(slow_job))
    assert await asyncio.to_thread(started.wait, 5)
    task.cancel()
    release.set()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert await database.call(lambda connection: connection.execute("SELECT 1").fetchone()[0]) == 1
    await database.close()
