import json
import os
import sqlite3
import time
from contextlib import closing
from pathlib import Path

from fastapi.testclient import TestClient

from codex_broker.config import Settings
from codex_broker.vault import generate_key
from codex_broker.web.app import create_app

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
