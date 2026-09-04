import asyncio
import json
import os
import re
import shutil
import sqlite3
import time
from contextlib import closing
from pathlib import Path
from typing import Any, cast

from fastapi.testclient import TestClient

from codex_broker.config import Settings
from codex_broker.database import Database
from codex_broker.vault import Envelope, Vault, decode_key, generate_key
from codex_broker.web.app import create_app

PASSWORD = "correct horse battery staple"  # noqa: S105


def bundle_auth(settings: Settings, state: str) -> dict[str, Any]:
    with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
        connection.row_factory = sqlite3.Row
        instance = connection.execute(
            "SELECT instance_uuid FROM instance_metadata WHERE singleton_id=1"
        ).fetchone()[0]
        row = connection.execute(
            "SELECT * FROM credential_bundles WHERE state=?", (state,)
        ).fetchone()
    assert settings.vault_key and row
    payload = Vault(decode_key(settings.vault_key), instance).decrypt(
        Envelope(
            row["bundle_id"],
            row["account_id"],
            row["key_id"],
            row["nonce"],
            row["ciphertext"],
            row["aad"],
            row["payload_schema_version"],
            row["envelope_version"],
        )
    )
    value = json.loads(Vault(decode_key(settings.vault_key), instance).auth_json(payload))
    return cast(dict[str, Any], value)


def wait_for(client: TestClient, text: str, path: str = "/", timeout: float = 8) -> str:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        response = client.get(path)
        if text in response.text:
            return str(response.text)
        time.sleep(0.1)
    raise AssertionError(f"{text!r} did not appear at {path}")


def test_enrollment_refresh_and_dashboard(tmp_path: Path) -> None:
    executable = Path(__file__).parents[1] / "fake_codex.py"
    os.chmod(executable, 0o700)
    settings = Settings(
        data_dir=tmp_path / "data",
        runtime_dir=tmp_path / "run",
        vault_key=generate_key(),
        admin_password=PASSWORD,
        codex_executable=str(executable),
        codex_idle_seconds=0,
    )
    with TestClient(create_app(settings)) as client:
        ready = client.get("/health/ready")
        assert ready.status_code == 200
        assert ready.json() == {"status": "ok"}
        login_page = client.get("/login")
        assert "default-src 'self'" in login_page.headers["content-security-policy"]
        assert login_page.headers["x-content-type-options"] == "nosniff"
        assert login_page.headers["referrer-policy"] == "no-referrer"
        login = client.post(
            "/login",
            data={"password": PASSWORD},
            headers={"Origin": "https://external.example"},
            follow_redirects=False,
        )
        assert login.status_code == 303
        assert login.headers.get_list("set-cookie")[0].count("Max-Age=7776000") == 1
        assert login.headers.get_list("set-cookie")[1].count("Max-Age=7776000") == 1
        client.cookies.update(login.cookies)
        csrf = client.cookies["wk_csrf"]
        created = client.post(
            "/accounts",
            data={
                "display_name": "Primary",
                "labels": "team, critical",
                "workspace": "workspace-1",
                "login_method": "CHATGPT_DEVICE_CODE",
                "admin_password": PASSWORD,
                "csrf_token": csrf,
            },
        )
        assert created.status_code == 200
        wait_for(client, "Primary")
        wait_for(client, "Healthy")
        wait_for(client, "22%")
        response = client.get("/")
        assert response.status_code == 200
        assert "orbit-layout" in response.text
        assert "data-variant-switcher" not in response.text
        dashboard = client.get("/api/internal/v1/dashboard").json()
        account = dashboard["data"][0]
        assert account["short_percent"] == 22
        retired_manual = client.post(
            f"/accounts/{account['public_token']}/reauthenticate",
            data={
                "login_method": "MANUAL_TOKENS",
                "admin_password": PASSWORD,
                "csrf_token": csrf,
            },
        )
        assert retired_manual.status_code == 409
        assert retired_manual.json()["code"] == "LOGIN_METHOD_UNAVAILABLE"
        login_traces = list((tmp_path / "run" / "accounts").glob("*/.fake-logins"))
        refresh_traces = list((tmp_path / "run" / "accounts").glob("*/.fake-refreshes"))
        assert len(login_traces) == len(refresh_traces) == 1
        assert login_traces[0].read_text(encoding="utf-8").splitlines() == ["login-1"]
        assert refresh_traces[0].read_text(encoding="utf-8").splitlines() == [
            "refresh-1",
            "refresh-2",
        ]
        for path in (
            f"/accounts/{account['public_token']}",
            "/accounts/new",
            "/settings",
            "/logs",
        ):
            assert client.get(path).status_code == 200
        exported_logs = client.get("/logs/export")
        assert exported_logs.status_code == 200
        assert exported_logs.headers["content-type"].startswith("application/x-ndjson")
        export_path = f"/accounts/{account['public_token']}/auth-export"
        assert (
            client.post(
                export_path,
                data={"admin_password": PASSWORD, "csrf_token": "invalid"},
            ).status_code
            == 403
        )
        assert client.post(export_path, data={"csrf_token": csrf}).status_code == 200
        auth_export = client.post(
            export_path,
            data={"admin_password": PASSWORD, "csrf_token": csrf},
        )
        assert auth_export.status_code == 200
        assert auth_export.headers["content-disposition"] == 'attachment; filename="auth.json"'
        assert auth_export.headers["cache-control"] == "no-store, max-age=0"
        assert auth_export.headers["content-type"] == "application/json"
        assert (
            json.loads(auth_export.content)["tokens"]["refresh_token"] == "fork-refresh-2"  # noqa: S105
        )
        with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
            export_bundle_id = connection.execute(
                "SELECT bundle_id FROM credential_bundles WHERE state='EXPORT'"
            ).fetchone()[0]
        reauthenticated = client.post(
            f"/accounts/{account['public_token']}/reauthenticate",
            data={
                "login_method": "CHATGPT_DEVICE_CODE",
                "admin_password": PASSWORD,
                "csrf_token": csrf,
            },
        )
        assert reauthenticated.status_code == 200
        deadline = time.monotonic() + 8
        while len(login_traces[0].read_text(encoding="utf-8").splitlines()) < 2:
            assert time.monotonic() < deadline
            time.sleep(0.1)
        while len(refresh_traces[0].read_text(encoding="utf-8").splitlines()) < 3:
            assert time.monotonic() < deadline
            time.sleep(0.1)
        while True:
            with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
                worker_state = connection.execute(
                    "SELECT worker_state FROM account_state"
                ).fetchone()[0]
            if worker_state == "STOPPED":
                break
            assert time.monotonic() < deadline
            time.sleep(0.1)
        rotated_export = client.post(
            export_path,
            data={"admin_password": PASSWORD, "csrf_token": csrf},
        )
        assert (
            json.loads(rotated_export.content)["tokens"]["refresh_token"] == "fork-refresh-2"  # noqa: S105
        )
        with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
            assert (
                connection.execute(
                    "SELECT bundle_id FROM credential_bundles WHERE state='EXPORT'"
                ).fetchone()[0]
                == export_bundle_id
            )
        assert (
            client.post(
                f"/accounts/{account['public_token']}/refresh",
                data={"csrf_token": "invalid"},
            ).status_code
            == 403
        )
        refresh = client.post(
            f"/accounts/{account['public_token']}/refresh",
            data={"csrf_token": csrf},
            follow_redirects=False,
        )
        assert refresh.status_code == 303
        wait_for(client, "Succeeded", refresh.headers["location"])
        latest_export = client.post(
            export_path,
            data={"admin_password": PASSWORD, "csrf_token": csrf},
        )
        assert (
            json.loads(latest_export.content)["tokens"]["refresh_token"] == "fork-refresh-2"  # noqa: S105
        )
        assert refresh_traces[0].read_text(encoding="utf-8").splitlines() == [
            "refresh-1",
            "refresh-2",
            "refresh-3",
        ]
        rotate_error_marker = executable.with_suffix(".rotate-then-rate-error")
        rotate_error_marker.touch()
        try:
            rotated_failure = client.post(
                f"/accounts/{account['public_token']}/refresh",
                data={"csrf_token": csrf},
                follow_redirects=False,
            )
            wait_for(client, "Failed", rotated_failure.headers["location"])
            assert bundle_auth(settings, "ACTIVE")["checkpoint"] == "rate-limits"
            assert bundle_auth(settings, "EXPORT")["tokens"]["refresh_token"] == "fork-refresh-2"  # noqa: S105
            with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
                assert (
                    connection.execute("SELECT last_error_code FROM usage_current").fetchone()[0]
                    == "CODEX_RPC_REJECTED"
                )
                assert (
                    connection.execute(
                        "SELECT count(*) FROM credential_bundles WHERE state='ACTIVE'"
                    ).fetchone()[0]
                    == 1
                )
                assert (
                    connection.execute(
                        "SELECT count(*) FROM credential_bundles WHERE state='RETIRED'"
                    ).fetchone()[0]
                    >= 1
                )
        finally:
            rotate_error_marker.unlink(missing_ok=True)

        transport_marker = executable.with_suffix(".transport-exit-on-rate-limits")
        transport_marker.touch()
        try:
            transport_failure = client.post(
                f"/accounts/{account['public_token']}/refresh",
                data={"csrf_token": csrf},
                follow_redirects=False,
            )
            wait_for(client, "Failed", transport_failure.headers["location"])
            with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
                assert (
                    connection.execute("SELECT last_error_code FROM usage_current").fetchone()[0]
                    == "CODEX_TRANSPORT_CLOSED"
                )
        finally:
            transport_marker.unlink(missing_ok=True)

        auth_error_marker = executable.with_suffix(".auth-error")
        auth_error_marker.touch()
        try:
            failed_refresh = client.post(
                f"/accounts/{account['public_token']}/refresh",
                data={"csrf_token": csrf},
                follow_redirects=False,
            )
            assert failed_refresh.status_code == 303
            wait_for(client, "Failed", failed_refresh.headers["location"])
            wait_for(client, "Action Required")
            wait_for(client, "authentication must be renewed", "/incidents")
            retained_export = client.post(
                export_path,
                data={"admin_password": PASSWORD, "csrf_token": csrf},
            )
            assert (
                json.loads(retained_export.content)["tokens"]["refresh_token"] == "fork-refresh-2"  # noqa: S105
            )
        finally:
            auth_error_marker.unlink(missing_ok=True)


def test_checkpoint_failure_remains_blocked_after_restart(tmp_path: Path) -> None:
    source = Path(__file__).parents[1] / "fake_codex.py"
    executable = tmp_path / "fake_codex.py"
    executable.write_bytes(source.read_bytes())
    executable.chmod(0o700)
    settings = Settings(
        data_dir=tmp_path / "data",
        runtime_dir=tmp_path / "run",
        vault_key=generate_key(),
        admin_password=PASSWORD,
        codex_executable=str(executable),
        codex_idle_seconds=0,
    )
    with TestClient(create_app(settings)) as client:
        login = client.post("/login", data={"password": PASSWORD})
        client.cookies.update(login.cookies)
        csrf = client.cookies["wk_csrf"]
        client.post(
            "/accounts",
            data={
                "display_name": "Checkpoint failure",
                "login_method": "CHATGPT_DEVICE_CODE",
                "admin_password": PASSWORD,
                "csrf_token": csrf,
            },
        )
        wait_for(client, "22%")
        account = client.get("/api/internal/v1/dashboard").json()["data"][0]
        executable.with_suffix(".corrupt-on-rate-limits").touch()
        checkpoint_failure = client.post(
            f"/accounts/{account['public_token']}/refresh",
            data={"csrf_token": csrf},
            follow_redirects=False,
        )
        wait_for(client, "Failed", checkpoint_failure.headers["location"])
        assert list((tmp_path / "run" / "accounts").glob("*/*/codex-home/auth.json"))
        assert "Credential Checkpoint" in client.get("/incidents").text
        blocked = client.post(
            f"/accounts/{account['public_token']}/refresh",
            data={"csrf_token": csrf},
        )
        assert blocked.status_code == 409

    executable.with_suffix(".corrupt-on-rate-limits").unlink(missing_ok=True)
    with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
        connection.execute(
            "UPDATE account_state SET worker_state='CREDENTIAL_IN_USE',auth_state='VERIFIED',overall_state='HEALTHY'"
        )
        connection.commit()

    with TestClient(create_app(settings)) as restarted:
        login = restarted.post("/login", data={"password": PASSWORD})
        restarted.cookies.update(login.cookies)
        account = restarted.get("/api/internal/v1/dashboard").json()["data"][0]
        assert account["auth_state"] == "AUTH_REQUIRED"
        assert account["overall_state"] == "ERROR"
        blocked = restarted.post(
            f"/accounts/{account['public_token']}/refresh",
            data={"csrf_token": restarted.cookies["wk_csrf"]},
        )
        assert blocked.status_code == 409
        recovery = restarted.post(
            f"/accounts/{account['public_token']}/reauthenticate",
            data={
                "login_method": "CHATGPT_DEVICE_CODE",
                "admin_password": PASSWORD,
                "csrf_token": restarted.cookies["wk_csrf"],
            },
        )
        assert recovery.status_code == 200
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            recovered = restarted.get("/api/internal/v1/dashboard").json()["data"][0]
            if recovered["auth_state"] == "VERIFIED":
                break
            time.sleep(0.05)
        else:
            raise AssertionError("explicit checkpoint recovery did not complete")


def test_export_failure_keeps_managed_account_usable(tmp_path: Path) -> None:
    source = Path(__file__).parents[1] / "fake_codex.py"
    executable = tmp_path / "fake_codex.py"
    executable.write_bytes(source.read_bytes())
    executable.chmod(0o700)
    executable.with_suffix(".export-fail").touch()
    settings = Settings(
        data_dir=tmp_path / "data",
        runtime_dir=tmp_path / "run",
        vault_key=generate_key(),
        admin_password=PASSWORD,
        codex_executable=str(executable),
        codex_idle_seconds=0,
    )
    with TestClient(create_app(settings)) as client:
        login = client.post("/login", data={"password": PASSWORD})
        client.cookies.update(login.cookies)
        csrf = client.cookies["wk_csrf"]
        client.post(
            "/accounts",
            data={
                "display_name": "Managed only",
                "login_method": "CHATGPT_DEVICE_CODE",
                "admin_password": PASSWORD,
                "csrf_token": csrf,
            },
        )
        wait_for(client, "22%")
        account = client.get("/api/internal/v1/dashboard").json()["data"][0]
        assert account["auth_state"] == "VERIFIED"
        executable.with_suffix(".rotate-on-rate-limits").touch()
        refresh = client.post(
            f"/accounts/{account['public_token']}/refresh",
            data={"csrf_token": csrf},
            follow_redirects=False,
        )
        wait_for(client, "Succeeded", refresh.headers["location"])
        assert bundle_auth(settings, "ACTIVE")["checkpoint"] == "rate-limits"
        with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
            assert (
                connection.execute(
                    "SELECT count(*) FROM credential_bundles WHERE state='ACTIVE'"
                ).fetchone()[0]
                == 1
            )
            assert not connection.execute(
                "SELECT 1 FROM credential_bundles WHERE state='EXPORT'"
            ).fetchone()


def test_managed_cancellation_checkpoints_before_returning(tmp_path: Path) -> None:
    executable = Path(__file__).parents[1] / "fake_codex.py"
    executable.chmod(0o700)
    settings = Settings(
        data_dir=tmp_path / "data",
        runtime_dir=tmp_path / "run",
        vault_key=generate_key(),
        admin_password=PASSWORD,
        codex_executable=str(executable),
        codex_idle_seconds=0,
    )
    app = create_app(settings)
    with TestClient(app) as client:
        login = client.post("/login", data={"password": PASSWORD})
        client.cookies.update(login.cookies)
        csrf = client.cookies["wk_csrf"]
        client.post(
            "/accounts",
            data={
                "display_name": "Cancelled operation",
                "login_method": "CHATGPT_DEVICE_CODE",
                "admin_password": PASSWORD,
                "csrf_token": csrf,
            },
        )
        wait_for(client, "22%")
        public = client.get("/api/internal/v1/dashboard").json()["data"][0]["public_token"]

        async def run_cancelled_operation() -> str:
            account = (await app.state.broker.services.account_detail(public))["account"]

            async def mutate_then_cancel(runtime: Any) -> None:
                path = runtime.codex_home / "auth.json"
                value = json.loads(path.read_text(encoding="utf-8"))
                value["checkpoint"] = "cancelled"
                value["tokens"]["refresh_token"] = "cancelled-refresh"  # noqa: S105
                path.write_text(json.dumps(value, separators=(",", ":")), encoding="utf-8")
                raise asyncio.CancelledError

            try:
                await app.state.broker.services._run_managed(account, mutate_then_cancel)
            except asyncio.CancelledError:
                return "cancelled"
            raise AssertionError("managed cancellation was not propagated")

        assert client.portal
        assert client.portal.call(run_cancelled_operation) == "cancelled"
        assert bundle_auth(settings, "ACTIVE")["checkpoint"] == "cancelled"


def test_managed_identity_mismatch_never_promotes_candidate(tmp_path: Path) -> None:
    source = Path(__file__).parents[1] / "fake_codex.py"
    executable = tmp_path / "fake_codex.py"
    executable.write_bytes(source.read_bytes())
    executable.chmod(0o700)
    executable.with_suffix(".managed-email-mismatch").touch()
    settings = Settings(
        data_dir=tmp_path / "data",
        runtime_dir=tmp_path / "run",
        vault_key=generate_key(),
        admin_password=PASSWORD,
        codex_executable=str(executable),
        codex_idle_seconds=0,
    )
    with TestClient(create_app(settings)) as client:
        login = client.post("/login", data={"password": PASSWORD})
        client.cookies.update(login.cookies)
        client.post(
            "/accounts",
            data={
                "display_name": "Mismatch",
                "login_method": "CHATGPT_DEVICE_CODE",
                "admin_password": PASSWORD,
                "csrf_token": client.cookies["wk_csrf"],
            },
        )
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            accounts = client.get("/api/internal/v1/dashboard").json()["data"]
            if accounts and accounts[0]["auth_state"] == "AUTH_REQUIRED":
                break
            time.sleep(0.05)
        else:
            raise AssertionError("managed identity mismatch did not fail enrollment")
        assert bundle_auth(settings, "ACTIVE")["tokens"]["refresh_token"] == "refresh-1"  # noqa: S105
        with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
            assert not connection.execute(
                "SELECT 1 FROM credential_bundles WHERE state='EXPORT'"
            ).fetchone()


def test_login_cancellation_cannot_promote_credentials(tmp_path: Path) -> None:
    source = Path(__file__).parents[1] / "fake_codex.py"
    executable = tmp_path / "fake_codex.py"
    executable.write_bytes(source.read_bytes())
    executable.chmod(0o700)
    executable.with_suffix(".hold-login").touch()
    settings = Settings(
        data_dir=tmp_path / "data",
        runtime_dir=tmp_path / "run",
        vault_key=generate_key(),
        admin_password=PASSWORD,
        codex_executable=str(executable),
        codex_idle_seconds=0,
    )
    with TestClient(create_app(settings)) as client:
        login = client.post("/login", data={"password": PASSWORD})
        client.cookies.update(login.cookies)
        csrf = client.cookies["wk_csrf"]
        created = client.post(
            "/accounts",
            data={
                "display_name": "Cancelled",
                "login_method": "CHATGPT_DEVICE_CODE",
                "admin_password": PASSWORD,
                "csrf_token": csrf,
            },
        )
        attempt_id = re.search(r'data-attempt="([^"]+)"', created.text).group(1)  # type: ignore[union-attr]
        nonce = re.search(r'data-nonce="([^"]+)"', created.text).group(1)  # type: ignore[union-attr]
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            interaction = client.get(
                f"/api/internal/v1/login-attempts/{attempt_id}/interaction",
                headers={"X-Interaction-Nonce": nonce},
            )
            if interaction.status_code == 200:
                break
            time.sleep(0.05)
        else:
            raise AssertionError("login interaction did not become ready")
        cancelled = client.post(
            f"/api/internal/v1/login-attempts/{attempt_id}/cancel",
            headers={"X-CSRF-Token": csrf},
        )
        assert cancelled.status_code == 202
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
                state = connection.execute(
                    "SELECT state FROM login_attempts WHERE login_attempt_id=?", (attempt_id,)
                ).fetchone()[0]
                active = connection.execute(
                    "SELECT 1 FROM credential_bundles WHERE state='ACTIVE'"
                ).fetchone()
            if state == "CANCELLED":
                break
            time.sleep(0.05)
        else:
            raise AssertionError("login cancellation did not complete")
        assert not active


def test_manual_token_migration_recovers_v4_schema_drift(tmp_path: Path) -> None:
    old_migrations = tmp_path / "old-migrations"
    old_migrations.mkdir()
    migrations = Path(__file__).parents[2] / "src" / "codex_broker" / "migrations"
    for migration in sorted(migrations.glob("00[1-3]_*.sql")):
        shutil.copy2(migration, old_migrations)
    database_path = tmp_path / "data" / "windowkeeper.db"
    database = Database(database_path, old_migrations)
    database.start()
    asyncio.run(database.close())
    with closing(sqlite3.connect(database_path)) as connection:
        connection.execute(
            "INSERT INTO schema_migrations VALUES(4,'004_manual_token_login','drifted',0)"
        )

    executable = Path(__file__).parents[1] / "fake_codex.py"
    os.chmod(executable, 0o700)
    settings = Settings(
        data_dir=tmp_path / "data",
        runtime_dir=tmp_path / "run",
        vault_key=generate_key(),
        admin_password=PASSWORD,
        codex_executable=str(executable),
        codex_idle_seconds=0,
    )
    with TestClient(create_app(settings)) as client:
        assert client.get("/health/ready").status_code == 200
    with closing(sqlite3.connect(database_path)) as connection:
        assert connection.execute("SELECT max(version) FROM schema_migrations").fetchone()[0] == 9


def test_manual_token_login_is_retired(tmp_path: Path) -> None:
    executable = Path(__file__).parents[1] / "fake_codex.py"
    os.chmod(executable, 0o700)
    settings = Settings(
        data_dir=tmp_path / "data",
        runtime_dir=tmp_path / "run",
        vault_key=generate_key(),
        admin_password=PASSWORD,
        codex_executable=str(executable),
        codex_idle_seconds=0,
    )
    with TestClient(create_app(settings)) as client:
        login = client.post("/login", data={"password": PASSWORD})
        client.cookies.update(login.cookies)
        csrf = client.cookies["wk_csrf"]
        assert "Paste tokens" not in client.get("/accounts/new").text
        rejected = client.post(
            "/accounts",
            data={
                "display_name": "Rejected import",
                "login_method": "MANUAL_TOKENS",
                "access_token": "source.access.jwt",
                "refresh_token": "source-refresh-token",
                "admin_password": PASSWORD,
                "csrf_token": csrf,
            },
        )
        assert rejected.status_code == 409
        assert rejected.json()["code"] == "LOGIN_METHOD_UNAVAILABLE"
        assert client.get("/api/internal/v1/dashboard").json()["data"] == []


def test_latest_auth_export_survives_restart(tmp_path: Path) -> None:
    executable = Path(__file__).parents[1] / "fake_codex.py"
    os.chmod(executable, 0o700)
    settings = Settings(
        data_dir=tmp_path / "data",
        runtime_dir=tmp_path / "run",
        vault_key=generate_key(),
        admin_password=PASSWORD,
        codex_executable=str(executable),
        codex_idle_seconds=0,
    )
    with TestClient(create_app(settings)) as client:
        login = client.post("/login", data={"password": PASSWORD})
        client.cookies.update(login.cookies)
        csrf = client.cookies["wk_csrf"]
        created = client.post(
            "/accounts",
            data={
                "display_name": "Persisted export",
                "login_method": "CHATGPT_DEVICE_CODE",
                "admin_password": PASSWORD,
                "csrf_token": csrf,
            },
        )
        assert created.status_code == 200
        wait_for(client, "22%")
        account = client.get("/api/internal/v1/dashboard").json()["data"][0]
        refreshed = client.post(
            f"/accounts/{account['public_token']}/refresh",
            data={"csrf_token": csrf},
            follow_redirects=False,
        )
        wait_for(client, "Succeeded", refreshed.headers["location"])

    export_path = f"/accounts/{account['public_token']}/auth-export"
    with TestClient(create_app(settings)) as client:
        login = client.post("/login", data={"password": PASSWORD})
        client.cookies.update(login.cookies)
        csrf = client.cookies["wk_csrf"]
        persisted = client.post(export_path, data={"admin_password": PASSWORD, "csrf_token": csrf})
        assert json.loads(persisted.content)["tokens"]["refresh_token"] == "fork-refresh-2"  # noqa: S105


def test_authentication_csrf_and_readiness_fail_closed(tmp_path: Path) -> None:
    settings = Settings(
        data_dir=tmp_path / "data",
        runtime_dir=tmp_path / "run",
        vault_key=generate_key(),
        admin_password=PASSWORD,
    )
    with TestClient(create_app(settings)) as client:
        assert client.get("/health/ready").status_code == 503
        unauthenticated = client.get("/", follow_redirects=False)
        assert unauthenticated.status_code == 303
        assert unauthenticated.headers["location"] == "/login"
        assert client.post("/login", data={"password": "wrong"}).status_code == 200
        login = client.post("/login", data={"password": PASSWORD}, follow_redirects=False)
        client.cookies.update(login.cookies)
        assert (
            client.post(
                "/settings/webhooks",
                data={
                    "display_name": "x",
                    "url": "https://example.test",
                    "csrf_token": "wrong",
                },
            ).status_code
            == 403
        )
        key_page = client.post(
            "/settings/client-keys",
            data={"name": "Pi desktop", "csrf_token": client.cookies["wk_csrf"]},
        )
        assert key_page.status_code == 200
        raw_key = re.search(r"<code>(cbk_[^<]+)</code>", key_page.text).group(1)  # type: ignore[union-attr]
        assert "Copy this key now" in key_page.text
        assert raw_key not in client.get("/settings").text
        assert raw_key not in client.get("/logs/export").text
        assert (
            client.post(
                "/settings/webhooks",
                data={
                    "display_name": "x",
                    "url": "https://example.test",
                    "csrf_token": client.cookies["wk_csrf"],
                },
                follow_redirects=False,
            ).status_code
            == 303
        )

    rooted = Settings(
        data_dir=tmp_path / "rooted-data",
        runtime_dir=tmp_path / "rooted-run",
        vault_key=generate_key(),
        admin_password=PASSWORD,
        root_path="/windowkeeper",
    )
    with TestClient(create_app(rooted)) as client:
        rooted_redirect = client.get("/", follow_redirects=False)
        assert rooted_redirect.headers["location"] == "/windowkeeper/login"
        login_html = client.get("/login").text
        assert 'data-root-path="/windowkeeper"' in login_html
        assert 'href="/windowkeeper/static/app.css"' in login_html
        rooted_login = client.post("/login", data={"password": PASSWORD}, follow_redirects=False)
        assert rooted_login.headers["location"] == "/windowkeeper/"
        assert rooted_login.cookies["wk_session"]


def test_login_is_rate_limited(tmp_path: Path) -> None:
    settings = Settings(
        data_dir=tmp_path / "data",
        runtime_dir=tmp_path / "run",
        vault_key=generate_key(),
        admin_password=PASSWORD,
    )
    with TestClient(create_app(settings)) as client:
        for _ in range(5):
            assert client.post("/login", data={"password": "wrong"}).status_code == 200
        assert client.post("/login", data={"password": "wrong"}).status_code == 429
