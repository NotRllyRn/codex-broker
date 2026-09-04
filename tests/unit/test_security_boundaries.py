import hashlib
from typing import Any
from urllib.parse import quote

import pytest

from codex_broker.errors import WindowkeeperError
from codex_broker.redaction import redact, sanitize_url
from codex_broker.services import (
    browser_contract,
    validate_callback,
    verify_identity,
)


def test_browser_callback_contract_and_state() -> None:
    redirect = "http://localhost:1455/auth/callback"
    auth = f"https://auth.openai.test/start?redirect_uri={quote(redirect, safe='')}&state=expected"
    contract = browser_contract(auth, (1455, 1457))
    callback = validate_callback(f"{redirect}?code=accepted&state=expected", contract)
    assert "code=accepted" in callback
    with pytest.raises(WindowkeeperError) as caught:
        validate_callback(f"{redirect}?code=accepted&state=wrong", contract)
    assert caught.value.code == "BROWSER_CALLBACK_STATE_MISMATCH"
    assert contract.state_hash == hashlib.sha256(b"expected").digest()
    with pytest.raises(WindowkeeperError) as oversized:
        browser_contract(auth + "&padding=" + "x" * 17_000, (1455, 1457))
    assert oversized.value.code == "CODEX_BROWSER_AUTH_CONTRACT_CHANGED"


def test_chatgpt_identity_accepts_nullable_email_but_rejects_mismatch() -> None:
    assert (
        verify_identity(
            {"upstream_email": "owner@example.test"},
            {"account": {"type": "chatgpt", "email": None, "planType": "pro"}},
        )["planType"]
        == "pro"
    )
    assert (
        verify_identity(
            {"upstream_email": "owner@example.test"},
            {"account": {"type": "chatgpt", "email": ""}},
        )["email"]
        == ""
    )
    with pytest.raises(WindowkeeperError) as reauthentication:
        verify_identity(
            {"upstream_email": "owner@example.test"},
            {"account": {"type": "chatgpt", "email": "other@example.test"}},
        )
    assert reauthentication.value.code == "AUTH_IDENTITY_MISMATCH"


@pytest.mark.parametrize("identity", ({}, {"account": None}))
def test_missing_codex_account_requires_authentication(identity: dict[str, Any]) -> None:
    with pytest.raises(WindowkeeperError) as missing:
        verify_identity({}, identity)
    assert missing.value.code == "CODEX_AUTH_REQUIRED"


@pytest.mark.parametrize(
    "identity",
    (
        {"account": {}},
        {"account": {"planType": "pro"}},
        {"account": {"type": "apiKey"}},
    ),
)
def test_identity_still_rejects_empty_or_non_chatgpt_account(identity: dict[str, Any]) -> None:
    with pytest.raises(WindowkeeperError) as unverifiable:
        verify_identity({}, identity)
    assert unverifiable.value.code == "AUTH_IDENTITY_UNVERIFIED"


def test_redaction_is_recursive_and_sanitizes_urls() -> None:
    value = redact(
        {
            "authorization": "Bearer secret",
            "nested": {
                "message": "failure at https://localhost:1455/auth/callback?code=secret&state=private",
                "token_shape": "sk-abcdefghijklmnopqrst",
                "url": "https://example.test/x?code=secret#fragment",
            },
        }
    )
    assert value["authorization"] == "[REDACTED]"
    assert "secret" not in str(value)
    assert sanitize_url("https://example.test/x?a=b") == "https://example.test/x?a=%5BREDACTED%5D"
