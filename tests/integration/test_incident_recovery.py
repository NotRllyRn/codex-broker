import json
import os
import sqlite3
import time
from contextlib import closing
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from windowkeeper.config import Settings
from windowkeeper.errors import WindowkeeperError
from windowkeeper.vault import generate_key
from windowkeeper.web.app import create_app

PASSWORD = "correct horse battery staple"  # noqa: S105


def test_workspace_failure_opens_and_reauthentication_resolves_incident(
    tmp_path: Path,
) -> None:
    source = Path(__file__).parents[1] / "fake_codex.py"
    executable = tmp_path / "fake_codex.py"
    executable.write_text(
        source.read_text(encoding="utf-8").replace(
            'os.environ.get("FAKE_CODEX_WORKSPACE", "workspace-1")',
            '"wrong-workspace"',
        ),
        encoding="utf-8",
    )
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
        login = client.post("/login", data={"password": PASSWORD}, follow_redirects=False)
        client.cookies.update(login.cookies)
        csrf = client.cookies["wk_csrf"]
        client.post(
            "/accounts",
            data={
                "display_name": "Repairable",
                "workspace": "workspace-1",
                "login_method": "CHATGPT_DEVICE_CODE",
                "admin_password": PASSWORD,
                "csrf_token": csrf,
            },
        )
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            dashboard = client.get("/api/internal/v1/dashboard").json()["data"]
            if dashboard and dashboard[0]["auth_state"] == "AUTH_REQUIRED":
                break
            time.sleep(0.05)
        else:
            raise AssertionError("workspace mismatch did not fail enrollment")
        assert "Authentication Failed" in client.get("/incidents").text
        with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
            opened = json.loads(
                bytes(
                    connection.execute(
                        "SELECT canonical_body FROM webhook_events WHERE event_type='incident.opened' ORDER BY created_at_ms DESC LIMIT 1"
                    ).fetchone()[0]
                )
            )
        assert opened["notification"]["code"] == "WK-101"
        assert opened["data"]["account_name"] == "Repairable"
        assert opened["data"]["cause_code"]
        assert "Replace or repair credentials" in opened["data"]["recommended_action"]

        executable.write_text(
            executable.read_text(encoding="utf-8").replace('"wrong-workspace"', '"workspace-1"'),
            encoding="utf-8",
        )
        public = dashboard[0]["public_token"]
        response = client.post(
            f"/accounts/{public}/reauthenticate",
            data={
                "login_method": "CHATGPT_DEVICE_CODE",
                "admin_password": PASSWORD,
                "csrf_token": csrf,
            },
        )
        assert response.status_code == 200
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if (
                client.get("/api/internal/v1/dashboard").json()["data"][0]["auth_state"]
                == "VERIFIED"
            ):
                break
            time.sleep(0.05)
        else:
            raise AssertionError("reauthentication did not recover")
        assert "Resolved" in client.get("/incidents").text
        with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
            resolved = json.loads(
                bytes(
                    connection.execute(
                        "SELECT canonical_body FROM webhook_events WHERE event_type='incident.resolved' ORDER BY created_at_ms DESC LIMIT 1"
                    ).fetchone()[0]
                )
            )
        assert resolved["notification"]["code"] == "WK-103"
        assert resolved["data"]["incident_status"] == "RESOLVED"
        assert resolved["data"]["occurrence_count"] == 1


@pytest.mark.parametrize(
    "failure_marker", (".turn-failed", ".turn-rejected", ".turn-auth-rejected")
)
def test_terminal_activation_failure_does_not_require_acknowledgment(
    tmp_path: Path, failure_marker: str
) -> None:
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
        login = client.post("/login", data={"password": PASSWORD}, follow_redirects=False)
        client.cookies.update(login.cookies)
        csrf = client.cookies["wk_csrf"]
        client.post(
            "/accounts",
            data={
                "display_name": "Limited",
                "workspace": "workspace-1",
                "login_method": "CHATGPT_DEVICE_CODE",
                "admin_password": PASSWORD,
                "csrf_token": csrf,
            },
        )
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            accounts = client.get("/api/internal/v1/dashboard").json()["data"]
            if accounts and accounts[0]["short_percent"] == 22:
                break
            time.sleep(0.05)
        else:
            raise AssertionError("account enrollment did not complete")
        executable.with_suffix(failure_marker).touch()
        activation = client.post(
            f"/accounts/{accounts[0]['public_token']}/activate",
            data={"csrf_token": csrf},
            follow_redirects=False,
        )
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if "Failed" in client.get(activation.headers["location"]).text:
                break
            time.sleep(0.05)
        else:
            raise AssertionError("activation failure did not complete")
        detail = client.get(f"/accounts/{accounts[0]['public_token']}").text
        assert "Ambiguous activation requires review" not in detail

    with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
        assert (
            connection.execute(
                "SELECT state FROM activation_attempts ORDER BY created_at_ms DESC LIMIT 1"
            ).fetchone()[0]
            == "FAILED_DEFINITE"
        )
        assert not connection.execute(
            "SELECT 1 FROM incidents WHERE problem_type='activation_ambiguous' AND state='OPEN'"
        ).fetchone()


def test_weekly_limit_resumes_planning_without_acknowledgment(tmp_path: Path) -> None:
    source = Path(__file__).parents[1] / "fake_codex.py"
    executable = tmp_path / "fake_codex.py"
    executable.write_bytes(source.read_bytes())
    executable.chmod(0o700)
    executable.with_suffix(".weekly-exhausted").touch()
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
        login = client.post("/login", data={"password": PASSWORD}, follow_redirects=False)
        client.cookies.update(login.cookies)
        csrf = client.cookies["wk_csrf"]
        client.post(
            "/accounts",
            data={
                "display_name": "Weekly limited",
                "workspace": "workspace-1",
                "login_method": "CHATGPT_DEVICE_CODE",
                "admin_password": PASSWORD,
                "csrf_token": csrf,
            },
        )
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            accounts = client.get("/api/internal/v1/dashboard").json()["data"]
            if accounts and accounts[0]["weekly_percent"] == 100:
                break
            time.sleep(0.05)
        else:
            raise AssertionError("weekly exhaustion was not observed")
        with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
            assert not connection.execute(
                "SELECT 1 FROM activation_attempts WHERE state='PLANNED'"
            ).fetchone()

        executable.with_suffix(".weekly-exhausted").unlink()
        refresh = client.post(
            f"/accounts/{accounts[0]['public_token']}/refresh",
            data={"csrf_token": csrf},
            follow_redirects=False,
        )
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if "Succeeded" in client.get(refresh.headers["location"]).text:
                break
            time.sleep(0.05)
        else:
            raise AssertionError("post-reset refresh did not complete")
        with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
            planned_id = connection.execute(
                "SELECT activation_id FROM activation_attempts WHERE state='PLANNED'"
            ).fetchone()[0]
            assert not connection.execute(
                "SELECT 1 FROM incidents WHERE problem_type='activation_ambiguous' AND state='OPEN'"
            ).fetchone()

        executable.with_suffix(".weekly-exhausted").touch()
        exhausted_refresh = client.post(
            f"/accounts/{accounts[0]['public_token']}/refresh",
            data={"csrf_token": csrf},
            follow_redirects=False,
        )
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if "Succeeded" in client.get(exhausted_refresh.headers["location"]).text:
                break
            time.sleep(0.05)
        else:
            raise AssertionError("exhausted refresh did not complete")
        race_operation = "exhaustion-race"
        with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
            account_id = connection.execute("SELECT account_id FROM accounts").fetchone()[0]
            assert (
                connection.execute(
                    "SELECT state FROM activation_attempts WHERE activation_id=?", (planned_id,)
                ).fetchone()[0]
                == "PLANNED"
            )
            connection.execute(
                "INSERT INTO operations(operation_id,account_id,kind,trigger,state,created_at_ms,state_version) VALUES(?,?,'activation.run','SCHEDULED','RUNNING',0,1)",
                (race_operation, account_id),
            )
            connection.execute(
                "UPDATE activation_attempts SET state='QUEUED' WHERE activation_id=?",
                (planned_id,),
            )
            connection.commit()

        async def simulate_exhaustion_race() -> None:
            await app.state.windowkeeper.services._handle_activation_error(
                {"account_id": account_id, "public_token": accounts[0]["public_token"]},
                planned_id,
                race_operation,
                WindowkeeperError("ACTIVATION_NOT_ELIGIBLE", "usage became exhausted", 409),
            )

        assert client.portal
        client.portal.call(simulate_exhaustion_race)
        with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
            assert (
                connection.execute(
                    "SELECT state FROM activation_attempts WHERE activation_id=?", (planned_id,)
                ).fetchone()[0]
                == "PLANNED"
            )
            assert (
                connection.execute(
                    "SELECT state FROM operations WHERE operation_id=?", (race_operation,)
                ).fetchone()[0]
                == "CANCELLED"
            )

        executable.with_suffix(".weekly-exhausted").unlink()
        resumed_refresh = client.post(
            f"/accounts/{accounts[0]['public_token']}/refresh",
            data={"csrf_token": csrf},
            follow_redirects=False,
        )
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if "Succeeded" in client.get(resumed_refresh.headers["location"]).text:
                break
            time.sleep(0.05)
        else:
            raise AssertionError("resumed refresh did not complete")
        with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
            assert (
                connection.execute(
                    "SELECT state FROM activation_attempts WHERE activation_id=?", (planned_id,)
                ).fetchone()[0]
                == "PLANNED"
            )


def test_startup_reconciles_a_completed_upstream_turn(tmp_path: Path) -> None:
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
        activation_safety_delay_seconds=1,
        activation_jitter_max_seconds=0,
    )
    with TestClient(create_app(settings)) as client:
        login = client.post("/login", data={"password": PASSWORD}, follow_redirects=False)
        client.cookies.update(login.cookies)
        csrf = client.cookies["wk_csrf"]
        client.post(
            "/accounts",
            data={
                "display_name": "Reconciled",
                "workspace": "workspace-1",
                "login_method": "CHATGPT_DEVICE_CODE",
                "admin_password": PASSWORD,
                "csrf_token": csrf,
            },
        )
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            accounts = client.get("/api/internal/v1/dashboard").json()["data"]
            if accounts and accounts[0]["short_percent"] == 22:
                break
            time.sleep(0.05)
        else:
            raise AssertionError("account enrollment did not complete")
        public = accounts[0]["public_token"]
        activation = client.post(
            f"/accounts/{public}/activate",
            data={"csrf_token": csrf},
            follow_redirects=False,
        )
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if "Succeeded" in client.get(activation.headers["location"]).text:
                break
            time.sleep(0.05)
        else:
            raise AssertionError("activation did not complete")

    with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
        activation_id = connection.execute(
            "SELECT activation_id FROM activation_attempts ORDER BY created_at_ms DESC LIMIT 1"
        ).fetchone()[0]
        active_before = connection.execute(
            "SELECT bundle_id FROM credential_bundles WHERE state='ACTIVE'"
        ).fetchone()[0]
        export_before = connection.execute(
            "SELECT bundle_id FROM credential_bundles WHERE state='EXPORT'"
        ).fetchone()[0]
        connection.execute(
            "UPDATE activation_attempts SET state='TURN_ACCEPTED',normalized_result=NULL,terminal_status=NULL,completed_at_ms=NULL WHERE activation_id=?",
            (activation_id,),
        )
        connection.execute(
            "UPDATE activation_operations SET state='AWAITING_RESPONSE',completed_at_ms=NULL WHERE activation_id=?",
            (activation_id,),
        )
        connection.execute(
            "UPDATE operations SET state='RUNNING',completed_at_ms=NULL WHERE json_extract(result_json,'$.activation_id')=?",
            (activation_id,),
        )
        connection.commit()
    executable.with_suffix(".reconcile-ok").touch()
    executable.with_suffix(".rotate-on-thread-read").touch()
    try:
        with TestClient(create_app(settings)) as client:
            login = client.post("/login", data={"password": PASSWORD}, follow_redirects=False)
            client.cookies.update(login.cookies)
            account = client.get("/api/internal/v1/dashboard").json()["data"][0]
            assert account["activation_state"] == "UNSCHEDULED"
            assert account["overall_state"] == "HEALTHY"
            assert "Activation Ambiguous" not in client.get("/incidents").text
        with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
            assert (
                connection.execute(
                    "SELECT bundle_id FROM credential_bundles WHERE state='ACTIVE'"
                ).fetchone()[0]
                != active_before
            )
            assert (
                connection.execute(
                    "SELECT bundle_id FROM credential_bundles WHERE state='EXPORT'"
                ).fetchone()[0]
                == export_before
            )
    finally:
        executable.with_suffix(".reconcile-ok").unlink(missing_ok=True)
        executable.with_suffix(".rotate-on-thread-read").unlink(missing_ok=True)

    with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
        connection.execute(
            "UPDATE activation_attempts SET state='SAFETY_BLOCKED' WHERE activation_id=?",
            (activation_id,),
        )
        connection.execute(
            "UPDATE account_state SET activation_state='SAFETY_BLOCKED',overall_state='WARNING'"
        )
        connection.execute(
            """INSERT INTO activation_attempts
            SELECT 'stale-plan',account_id,'stale-window','SCHEDULED',prompt_version,prompt_sha256,
            'REPORTED_RESET','CONFIRMED',basis_reset_at_s,basis_duration_minutes,0,'PLANNED',NULL,NULL,
            'stale-plan',NULL,NULL,NULL,created_at_ms,updated_at_ms,NULL,1
            FROM activation_attempts WHERE activation_id=?""",
            (activation_id,),
        )
        connection.commit()
    executable.with_suffix(".weekly-exhausted").touch()
    with TestClient(create_app(settings)) as client:
        login = client.post("/login", data={"password": PASSWORD}, follow_redirects=False)
        client.cookies.update(login.cookies)
        refresh = client.post(
            f"/accounts/{public}/refresh",
            data={"csrf_token": client.cookies["wk_csrf"]},
            follow_redirects=False,
        )
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if "Succeeded" in client.get(refresh.headers["location"]).text:
                break
            time.sleep(0.05)
        else:
            raise AssertionError("exhausted refresh did not complete")
        account = client.get("/api/internal/v1/dashboard").json()["data"][0]
        assert account["activation_state"] == "SAFETY_BLOCKED"
        executable.with_suffix(".weekly-exhausted").unlink()
        acknowledged = client.post(
            f"/accounts/{public}/ambiguity/acknowledge",
            data={"csrf_token": client.cookies["wk_csrf"], "admin_password": PASSWORD},
            follow_redirects=False,
        )
        assert acknowledged.status_code == 303
        account = client.get("/api/internal/v1/dashboard").json()["data"][0]
        assert account["activation_state"] != "SAFETY_BLOCKED"
        with closing(sqlite3.connect(settings.data_dir / "windowkeeper.db")) as connection:
            assert (
                connection.execute(
                    "SELECT state FROM activation_attempts WHERE activation_id='stale-plan'"
                ).fetchone()[0]
                == "CANCELLED"
            )
