# pyright: reportMissingImports=false

import base64
import hashlib
import json
from typing import Any

import pytest

from codex_broker.credential_authority import ACCOUNT_CLAIM, CredentialAuthority
from codex_broker.errors import WindowkeeperError
from codex_broker.vault import Vault


def token(claims: dict[str, Any]) -> str:
    body = base64.urlsafe_b64encode(json.dumps(claims).encode()).rstrip(b"=").decode()
    return f"header.{body}.signature"


def payload(vault: Vault, access_token: str) -> dict[str, Any]:
    del vault
    content = json.dumps(
        {"tokens": {"access_token": access_token, "refresh_token": "never-returned"}}
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


class Services:
    def __init__(self, value: dict[str, Any], refreshed: dict[str, Any] | None = None) -> None:
        self.value = value
        self.refreshed = refreshed
        self.refreshes = 0

    async def credential_payload_for_lease(
        self, account: dict[str, Any], needs_refresh: Any
    ) -> dict[str, Any]:
        del account
        if needs_refresh(self.value):
            self.refreshes += 1
            if self.refreshed:
                self.value = self.refreshed
        return self.value


@pytest.mark.asyncio
async def test_authority_refreshes_stale_token_and_returns_only_lease_fields() -> None:
    vault = Vault(b"x" * 32, "instance")
    stale = payload(vault, token({"exp": 100, ACCOUNT_CLAIM: {"chatgpt_account_id": "old"}}))
    fresh = payload(
        vault, token({"exp": 10_000, ACCOUNT_CLAIM: {"chatgpt_account_id": "account-1"}})
    )
    services = Services(stale, fresh)
    lease = await CredentialAuthority(services, vault).lease({"account_id": "internal"}, 1_000_000)
    assert services.refreshes == 1
    assert lease.account_id == "account-1"
    assert lease.expires_at_ms == 10_000_000
    assert not hasattr(lease, "refresh_token")


@pytest.mark.asyncio
async def test_authority_rejects_invalid_refreshed_payload() -> None:
    vault = Vault(b"x" * 32, "instance")
    services = Services(payload(vault, "invalid"))
    with pytest.raises(WindowkeeperError, match="cannot be leased"):
        await CredentialAuthority(services, vault).lease({"account_id": "internal"}, 1_000)
