# pyright: reportMissingImports=false

import asyncio
import base64
import hashlib
import json
import os
import shutil
import time
from pathlib import Path
from typing import Any, cast

from fastapi.testclient import TestClient

from codex_broker.clock import iso_time
from codex_broker.config import Settings
from codex_broker.database import Database
from codex_broker.router import RouteLease, RouteRequest
from codex_broker.vault import Vault, decode_key, generate_key
from codex_broker.web.app import create_app

PASSWORD = "correct horse battery staple"  # noqa: S105
ACCOUNT_CLAIM = "https://api.openai.com/auth"


def jwt(claims: dict[str, Any]) -> str:
    payload = base64.urlsafe_b64encode(json.dumps(claims).encode()).rstrip(b"=").decode()
    return f"header.{payload}.signature"


def credential(access_token: str) -> dict[str, Any]:
    content = json.dumps(
        {"tokens": {"access_token": access_token, "refresh_token": "not-returned"}}
    ).encode()
    return {
        "schema_version": 1,
        "files": [
            {
                "relative_path": "auth.json",
                "content_base64": base64.b64encode(content).decode(),
                "sha256": hashlib.sha256(content).hexdigest(),
            }
        ],
    }


def seed_account(connection: Any, vault: Vault, access_token: str) -> None:
    envelope = vault.encrypt("internal", credential(access_token))
    connection.execute(
        "INSERT INTO accounts VALUES('internal','public','Primary','chatgpt','CHATGPT_DEVICE_CODE','CHATGPT_DEVICE_CODE',NULL,1,'ACTIVE',1,1,NULL)"
    )
    connection.execute(
        "INSERT INTO account_state VALUES('internal','VERIFIED','STOPPED','HEALTHY','FRESH',NULL,NULL,1,1,NULL,NULL,NULL,1,1)"
    )
    connection.execute(
        "INSERT INTO usage_current(account_id,short_used_percent_raw,weekly_used_percent_raw) VALUES('internal',10,40)"
    )
    connection.execute(
        "INSERT INTO credential_bundles VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
        (
            envelope.bundle_id,
            envelope.account_id,
            "ACTIVE",
            1,
            1,
            envelope.key_id,
            envelope.nonce,
            envelope.ciphertext,
            envelope.aad,
            "test",
            1,
            1,
            None,
        ),
    )


def test_machine_api_authenticates_and_returns_access_only_lease(tmp_path: Path) -> None:
    executable = Path(__file__).parents[1] / "fake_codex.py"
    os.chmod(executable, 0o700)
    settings = Settings(
        data_dir=tmp_path / "data",
        runtime_dir=tmp_path / "run",
        vault_key=generate_key(),
        admin_password=PASSWORD,
        codex_executable=str(executable),
    )
    app = create_app(settings)
    with TestClient(app) as client:
        state = app.state.broker
        portal = client.portal
        assert portal is not None
        issued = portal.call(state.client_keys.create, "Pi")
        access = jwt(
            {
                "exp": int(time.time()) + 3600,
                ACCOUNT_CLAIM: {"chatgpt_account_id": "upstream"},
            }
        )
        portal.call(
            state.database.transaction,
            lambda connection: seed_account(connection, state.services.vault, access),
        )
        assert client.get("/api/v1/health").status_code == 401
        response = client.post(
            "/api/v1/route",
            headers={"Authorization": f"Bearer {issued.token}"},
            json={"session_id": "session", "turn_id": "turn"},
        )
        assert response.status_code == 200
        assert response.headers["cache-control"] == "no-store, max-age=0"
        assert response.json() == {
            "status": "ok",
            "account_id": "public",
            "account_label": "Primary",
            "access_token": access,
            "chatgpt_account_id": "upstream",
            "expires_at": response.json()["expires_at"],
            "short_remaining_percent": 90,
            "weekly_remaining_percent": 60,
            "short_resets_at": None,
            "weekly_resets_at": None,
        }
        assert response.json()["expires_at"].endswith("Z")
        assert "refresh" not in response.text
        reset = int(time.time()) + 30
        portal.call(
            state.database.transaction,
            lambda connection: connection.execute(
                "UPDATE usage_current SET short_used_percent_raw=100,short_resets_at_s=?",
                (reset,),
            ),
        )
        waiting = client.post(
            "/api/v1/route",
            headers={"Authorization": f"Bearer {issued.token}"},
            json={"session_id": "session", "turn_id": "waiting"},
        )
        assert waiting.status_code == 429
        assert waiting.json()["next_retry_at"] == iso_time(reset * 1000 + 10_000)
        assert int(waiting.headers["retry-after"]) == waiting.json()["retry_after_seconds"]

        client.cookies.clear()
        assert (
            client.get(
                "/api/internal/v1/dashboard",
                headers={"Authorization": f"Bearer {issued.token}"},
                follow_redirects=False,
            ).status_code
            == 401
        )
        portal.call(state.client_keys.revoke, issued.key_id)
        assert (
            client.post(
                "/api/v1/route",
                headers={"Authorization": f"Bearer {issued.token}"},
                json={"session_id": "session", "turn_id": "revoked"},
            ).status_code
            == 401
        )


def test_concurrent_near_expiry_routes_refresh_once(tmp_path: Path) -> None:
    executable = Path(__file__).parents[1] / "fake_codex.py"
    marker = executable.with_suffix(".lease-refresh")
    marker.touch()
    os.chmod(executable, 0o700)
    settings = Settings(
        data_dir=tmp_path / "data",
        runtime_dir=tmp_path / "run",
        vault_key=generate_key(),
        admin_password=PASSWORD,
        codex_executable=str(executable),
    )
    try:
        app = create_app(settings)
        with TestClient(app) as client:
            state = app.state.broker
            assert client.portal is not None
            issued = client.portal.call(state.client_keys.create, "Pi")
            access = jwt(
                {
                    "exp": int(time.time()) + 30,
                    ACCOUNT_CLAIM: {"chatgpt_account_id": "upstream"},
                }
            )
            client.portal.call(
                state.database.transaction,
                lambda connection: seed_account(connection, state.services.vault, access),
            )

            async def routes() -> list[RouteLease]:
                values = await asyncio.gather(
                    *(
                        state.router.route(issued.key_id, RouteRequest("session", f"turn-{index}"))
                        for index in range(50)
                    )
                )
                return [value for value in values if isinstance(value, RouteLease)]

            leases = client.portal.call(routes)
            assert len(leases) == 50
            assert len({lease.access_token for lease in leases}) == 1
            traces = list((tmp_path / "run" / "accounts").glob("*/.fake-lease-refreshes"))
            assert len(traces) == 1
            assert traces[0].read_text().splitlines() == ["refresh"]
            counts = client.portal.call(
                state.database.call,
                lambda connection: tuple(
                    tuple(row)
                    for row in connection.execute(
                        "SELECT state,count(*) FROM credential_bundles GROUP BY state ORDER BY state"
                    )
                ),
            )
            assert counts == (("ACTIVE", 1), ("RETIRED", 1))
    finally:
        marker.unlink(missing_ok=True)


def test_pre_broker_database_upgrades_without_relogin(tmp_path: Path) -> None:
    migrations = Path(__file__).parents[2] / "src" / "codex_broker" / "migrations"
    old_migrations = tmp_path / "old-migrations"
    old_migrations.mkdir()
    for migration in sorted(migrations.glob("00[1-6]_*.sql")):
        shutil.copy2(migration, old_migrations)

    key = generate_key()
    database_path = tmp_path / "data" / "windowkeeper.db"
    old = Database(database_path, old_migrations)
    old.start()
    instance = asyncio.run(
        old.call(
            lambda connection: str(
                connection.execute("SELECT instance_uuid FROM instance_metadata").fetchone()[0]
            )
        )
    )
    vault = Vault(decode_key(key), instance)
    access = jwt(
        {
            "exp": int(time.time()) + 3600,
            ACCOUNT_CLAIM: {"chatgpt_account_id": "upstream-old"},
        }
    )

    def seed(connection: Any) -> None:
        envelope = vault.encrypt("internal-old", credential(access))
        connection.execute(
            "INSERT INTO vault_state VALUES(1,?,?,?,1)",
            (
                vault.key_id,
                b"legacy",
                vault.seal_text("vault-sentinel", f"windowkeeper:{instance}"),
            ),
        )
        connection.execute(
            "INSERT INTO accounts VALUES('internal-old','public-old','Existing','chatgpt','CHATGPT_DEVICE_CODE','CHATGPT_DEVICE_CODE',NULL,1,'ACTIVE',1,1,NULL)"
        )
        connection.execute(
            "INSERT INTO account_state VALUES('internal-old','VERIFIED','STOPPED','HEALTHY','FRESH','UNSCHEDULED',NULL,NULL,1,1,NULL,NULL,NULL,1,1)"
        )
        connection.execute("INSERT INTO usage_current(account_id) VALUES('internal-old')")
        connection.execute(
            "INSERT INTO credential_bundles VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (
                envelope.bundle_id,
                envelope.account_id,
                "ACTIVE",
                1,
                1,
                envelope.key_id,
                envelope.nonce,
                envelope.ciphertext,
                envelope.aad,
                "test",
                1,
                1,
                None,
            ),
        )

    asyncio.run(old.transaction(seed))
    asyncio.run(old.close())

    settings = Settings(
        data_dir=tmp_path / "data",
        runtime_dir=tmp_path / "run",
        vault_key=key,
        admin_password=PASSWORD,
        codex_executable=str(Path(__file__).parents[1] / "fake_codex.py"),
    )
    app = create_app(settings)
    with TestClient(app) as client:
        login = client.post("/login", data={"password": PASSWORD})
        client.cookies.update(login.cookies)
        dashboard = client.get("/api/internal/v1/dashboard").json()["data"]
        assert dashboard[0]["display_name"] == "Existing"
        state = app.state.broker
        assert client.portal is not None
        issued = client.portal.call(state.client_keys.create, "Migration test")
        response = client.post(
            "/api/v1/route",
            headers={"Authorization": f"Bearer {issued.token}"},
            json={"session_id": "session", "turn_id": "turn"},
        )
        assert response.json()["access_token"] == access
        assert dashboard[0]["auth_state"] == "VERIFIED"

        marker = Path(settings.codex_executable).with_suffix(".rotate-on-rate-limits")
        marker.touch()

        async def checkpoint() -> dict[str, Any]:
            account = await state.services._account_row("public-old")
            await state.services._run_managed(
                account, lambda runtime: runtime.adapter.rate_limits()
            )
            payload = await state.services._credential_payload("internal-old")
            return cast(dict[str, Any], json.loads(state.services.vault.auth_json(payload)))

        try:
            assert client.portal.call(checkpoint)["checkpoint"] == "rate-limits"
        finally:
            marker.unlink(missing_ok=True)

        columns = client.portal.call(
            state.database.call,
            lambda connection: [
                row[1] for row in connection.execute("PRAGMA table_info(account_state)")
            ],
        )
        assert "activation_state" not in columns
