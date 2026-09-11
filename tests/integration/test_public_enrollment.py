import os
import sqlite3
import time
from contextlib import closing
from pathlib import Path
from typing import Any

import httpx
from fastapi.testclient import TestClient
from pydantic import SecretStr

from codex_broker.config import Settings
from codex_broker.vault import generate_key
from codex_broker.web.app import create_app
from codex_broker.web.public import COOKIE, create_public_app

KEY = "public-enrollment-test-key-1234567890"  # noqa: S105
PASSWORD = "correct horse battery staple"  # noqa: S105


def settings(tmp_path: Path) -> Settings:
    executable = Path(__file__).parents[1] / "fake_codex.py"
    os.chmod(executable, 0o700)
    return Settings(
        data_dir=tmp_path / "data",
        runtime_dir=tmp_path / "run",
        vault_key=generate_key(),
        admin_password=PASSWORD,
        codex_executable=str(executable),
        public_enrollment_enabled=True,
        public_enrollment_key=SecretStr(KEY),
        window_pulse_enabled=False,
    )


def test_private_enrollment_creates_email_labeled_account(tmp_path: Path) -> None:
    configured = settings(tmp_path)
    authorization = {"Authorization": f"Bearer {KEY}"}
    with TestClient(create_app(configured)) as client:
        assert (
            client.post(
                "/api/private/v1/public-enrollments",
                json={"session_token": "x" * 43},
            ).status_code
            == 401
        )
        started = client.post(
            "/api/private/v1/public-enrollments",
            headers=authorization,
            json={"session_token": "x" * 43},
        )
        assert started.status_code == 200
        values = started.json()
        rejected = client.post(
            "/api/private/v1/public-enrollments/status",
            headers=authorization,
            json={
                "enrollment_id": values["enrollment_id"],
                "login_attempt_id": values["login_attempt_id"],
                "session_token": "x" * 43,
                "interaction_nonce": "z" * 43,
            },
        )
        assert rejected.status_code == 404
        deadline = time.monotonic() + 8
        while True:
            response = client.post(
                "/api/private/v1/public-enrollments/status",
                headers=authorization,
                json={
                    "enrollment_id": values["enrollment_id"],
                    "login_attempt_id": values["login_attempt_id"],
                    "session_token": "x" * 43,
                    "interaction_nonce": values["interaction_nonce"],
                },
            )
            assert response.status_code == 200
            if response.json()["state"] == "COMPLETED":
                break
            assert time.monotonic() < deadline
            time.sleep(0.05)
        assert response.json()["email"] == "owner@example.test"
        accounts = client.get("/api/internal/v1/dashboard")
        assert accounts.status_code == 401
        duplicate = client.post(
            "/api/private/v1/public-enrollments",
            headers=authorization,
            json={"session_token": "y" * 43},
        ).json()
        while True:
            duplicate_status = client.post(
                "/api/private/v1/public-enrollments/status",
                headers=authorization,
                json={
                    "enrollment_id": duplicate["enrollment_id"],
                    "login_attempt_id": duplicate["login_attempt_id"],
                    "session_token": "y" * 43,
                    "interaction_nonce": duplicate["interaction_nonce"],
                },
            ).json()
            if duplicate_status["state"] == "FAILED":
                break
            assert time.monotonic() < deadline
            time.sleep(0.05)
        with closing(sqlite3.connect(configured.data_dir / "windowkeeper.db")) as connection:
            names = [
                str(row[0])
                for row in connection.execute(
                    "SELECT display_name FROM accounts WHERE deleted_at_ms IS NULL"
                )
            ]
        assert names == ["owner@example.test"]


def test_public_site_exposes_only_guided_enrollment(tmp_path: Path) -> None:
    calls = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        assert request.headers.get("authorization") == f"Bearer {KEY}"
        calls += 1
        if request.url.path.endswith("/public-enrollments"):
            return httpx.Response(
                200,
                json={
                    "enrollment_id": "e" * 32,
                    "login_attempt_id": "a" * 32,
                    "interaction_nonce": "n" * 43,
                },
            )
        body: dict[str, Any] = __import__("json").loads(request.content)
        assert body["session_token"]
        return httpx.Response(
            200,
            json={
                "state": "WAITING_FOR_USER",
                "verification_url": "https://auth.openai.com/codex/device",
                "user_code": "ABCD-EFGH",
                "expires_at_ms": 4_102_444_800_000,
            },
        )

    configured = settings(tmp_path)
    with TestClient(
        create_public_app(configured, httpx.MockTransport(handler)),
        base_url="https://public.example",
    ) as client:
        page = client.get("/")
        assert page.status_code == 200
        assert calls == 0
        assert "Step 1. Copy this code:" in page.text
        assert "Step 2. Open the OpenAI sign-in page" in page.text
        assert "Step 3. Return to this tab" in page.text
        assert "Only continue if you trust its operator" in page.text
        started = client.post("/start")
        assert started.status_code == 200
        assert COOKIE in client.cookies
        assert "HttpOnly" in started.headers.get("set-cookie", "")
        assert "Secure" in started.headers.get("set-cookie", "")
        status = client.get("/status")
        assert status.json()["user_code"] == "ABCD-EFGH"
        assert status.json()["verification_url"] == "https://auth.openai.com/codex/device"
        assert client.get("/login").status_code == 404
        assert client.get("/accounts").status_code == 404
        assert "frame-ancestors 'none'" in page.headers.get("content-security-policy", "")
        assert calls == 2
