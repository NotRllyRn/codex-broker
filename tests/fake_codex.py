#!/usr/bin/env python3
import json
import os
import sys
import time
import tomllib
from pathlib import Path
from typing import Any

if "--version" in sys.argv:
    print("codex-cli 0.145.0")
    raise SystemExit(0)

home = Path(os.environ["CODEX_HOME"])
pending_login_id: str | None = None


def send(value: dict[str, Any]) -> None:
    print(json.dumps(value, separators=(",", ":")), flush=True)


def marker(suffix: str) -> bool:
    try:
        return Path(__file__).with_suffix(suffix).exists()
    except OSError:
        return False


def mutate_auth(label: str) -> None:
    auth = json.loads((home / "auth.json").read_text(encoding="utf-8"))
    auth["checkpoint"] = label
    auth["tokens"]["access_token"] = f"{label}-access"
    auth["tokens"]["refresh_token"] = f"{label}-refresh"
    (home / "auth.json").write_text(json.dumps(auth, separators=(",", ":")), encoding="utf-8")


def workspace_matches() -> bool:
    config = tomllib.loads((home / "config.toml").read_text(encoding="utf-8"))
    forced = config.get("forced_chatgpt_workspace_id")
    observed = os.environ.get("FAKE_CODEX_WORKSPACE", "workspace-1")
    return not forced or forced == observed


for line in sys.stdin:
    try:
        message = json.loads(line)
    except json.JSONDecodeError:
        continue
    method = message.get("method")
    params = message.get("params") or {}
    request_id = message.get("id")
    if request_id is None:
        continue
    if method == "initialize":
        send({"id": request_id, "result": {"serverInfo": {"name": "fake-codex"}}})
    elif method == "account/login/start":
        try:
            home.mkdir(parents=True, exist_ok=True)
            trace = home.parents[1] / ".fake-logins"
            login_number = (
                len(trace.read_text(encoding="utf-8").splitlines()) + 1 if trace.exists() else 1
            )
            with trace.open("a", encoding="utf-8") as stream:
                stream.write(f"login-{login_number}\n")
            (home / "auth.json").write_text(
                json.dumps(
                    {
                        "auth_mode": "chatgpt",
                        "tokens": {
                            "id_token": f"id-{login_number}",
                            "access_token": f"access-{login_number}",
                            "refresh_token": f"refresh-{login_number}",
                        },
                    },
                    separators=(",", ":"),
                ),
                encoding="utf-8",
            )
        except OSError:
            send({"id": request_id, "error": {"code": "credential_write_failed"}})
            continue
        login_id = f"login-{login_number}"
        send(
            {
                "id": request_id,
                "result": {
                    "loginId": login_id,
                    "verificationUrl": "https://auth.openai.test/device",
                    "userCode": "ABCD-EFGH",
                    "expiresAt": int(time.time() * 1000) + 60000,
                },
            }
        )
        if marker(".hold-login"):
            pending_login_id = login_id
        else:
            send(
                {
                    "method": "account/login/completed",
                    "params": {"loginId": login_id, "success": workspace_matches()},
                }
            )
    elif method == "account/login/cancel":
        send({"id": request_id, "result": {}})
        if pending_login_id:
            send(
                {
                    "method": "account/login/completed",
                    "params": {"loginId": pending_login_id, "success": False},
                }
            )
            pending_login_id = None
    elif method == "account/read":
        if params.get("refreshToken"):
            trace = home.parents[1] / ".fake-refreshes"
            refresh_number = (
                len(trace.read_text(encoding="utf-8").splitlines()) + 1 if trace.exists() else 1
            )
            with trace.open("a", encoding="utf-8") as stream:
                stream.write(f"refresh-{refresh_number}\n")
            auth = json.loads((home / "auth.json").read_text(encoding="utf-8"))
            auth["tokens"] |= {
                "id_token": f"fork-id-{refresh_number}",
                "access_token": f"fork-access-{refresh_number}",
                "refresh_token": f"fork-refresh-{refresh_number}",
            }
            (home / "auth.json").write_text(
                json.dumps(auth, separators=(",", ":")), encoding="utf-8"
            )
            if marker(".export-fail") and refresh_number == 2:
                send({"id": request_id, "error": {"code": -32000, "message": "export failed"}})
                continue
        account = None
        if not marker(".account-none"):
            account = {
                "type": "chatgpt",
                "email": (
                    "other@example.test"
                    if params.get("refreshToken") and marker(".managed-email-mismatch")
                    else (None if marker(".account-email-null") else "owner@example.test")
                ),
                "planType": "pro",
            }
        send(
            {
                "id": request_id,
                "result": {"account": account, "requiresOpenaiAuth": account is None},
            }
        )
    elif method == "account/rateLimits/read":
        if marker(".transport-exit-on-rate-limits"):
            raise SystemExit(0)
        if marker(".rotate-on-rate-limits") or marker(".rotate-then-rate-error"):
            mutate_auth("rate-limits")
        if marker(".corrupt-on-rate-limits"):
            (home / "auth.json").write_text("not-json", encoding="utf-8")
        if marker(".rotate-then-rate-error"):
            send({"id": request_id, "error": {"code": -32000, "message": "temporary failure"}})
            continue
        if marker(".auth-error"):
            send({"id": request_id, "error": {"code": -32001, "message": "sign in again"}})
            continue
        send(
            {
                "id": request_id,
                "result": {
                    "rateLimitsByLimitId": {
                        "codex": {
                            "windows": [
                                {
                                    "name": "short",
                                    "usedPercent": 22,
                                    "windowDurationMins": 300,
                                    "resetsAt": int(time.time()) + 3600,
                                },
                                {
                                    "name": "weekly",
                                    "usedPercent": 100 if marker(".weekly-exhausted") else 41,
                                    "windowDurationMins": 10080,
                                    "resetsAt": int(time.time()) + 86400,
                                },
                            ]
                        }
                    }
                },
            }
        )
    elif method == "model/list":
        if marker(".rotate-on-model-list"):
            mutate_auth("model-list")
        send(
            {
                "id": request_id,
                "result": {
                    "data": [
                        {
                            "model": "gpt-5.6-sol",
                            "hidden": False,
                            "inputModalities": ["text", "image"],
                            "supportedReasoningEfforts": [{"reasoningEffort": "low"}],
                        },
                        {
                            "model": "gpt-5.4-mini",
                            "hidden": False,
                            "inputModalities": ["text"],
                            "supportedReasoningEfforts": [
                                {"reasoningEffort": "minimal"},
                                {"reasoningEffort": "low"},
                            ],
                        },
                    ],
                    "nextCursor": None,
                },
            }
        )
    elif method == "thread/start":
        if params.get("model") != "gpt-5.4-mini" or params.get("serviceTier") != "default":
            send({"id": request_id, "error": {"code": "expensive_model"}})
            continue
        send(
            {
                "id": request_id,
                "result": {
                    "thread": {"id": "thread-1"},
                    "model": "gpt-5.4-mini",
                    "serviceTier": "default",
                },
            }
        )
    elif method == "turn/start":
        if (
            params.get("model") != "gpt-5.4-mini"
            or params.get("effort") != "minimal"
            or params.get("serviceTier") != "default"
        ):
            send({"id": request_id, "error": {"code": "expensive_turn"}})
            continue
        if marker(".turn-rejected") or marker(".turn-auth-rejected"):
            code = "unauthorized" if marker(".turn-auth-rejected") else "usage_limit_reached"
            send({"id": request_id, "error": {"code": code}})
            continue
        send({"id": request_id, "result": {"turn": {"id": "turn-1"}}})
        status = "failed" if marker(".turn-failed") else "completed"
        if status == "completed":
            send(
                {
                    "method": "item/agentMessage/delta",
                    "params": {"turnId": "turn-1", "delta": "OK"},
                }
            )
        send(
            {
                "method": "turn/completed",
                "params": {
                    "threadId": "thread-1",
                    "turn": {"id": "turn-1", "items": [], "status": status},
                },
            }
        )
    elif method == "thread/read":
        if marker(".rotate-on-thread-read"):
            mutate_auth("thread-read")
        turns = []
        if marker(".reconcile-ok"):
            turns = [
                {
                    "id": "turn-1",
                    "status": "completed",
                    "items": [{"type": "agentMessage", "text": "OK"}],
                }
            ]
        send(
            {
                "id": request_id,
                "result": {"thread": {"id": "thread-1", "turns": turns}},
            }
        )
    else:
        send({"id": request_id, "error": {"code": -32601, "message": "unknown"}})
