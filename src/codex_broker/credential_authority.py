# pyright: reportMissingImports=false

import base64
import json
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any, Protocol

from codex_broker.errors import WindowkeeperError
from codex_broker.vault import Vault

ACCOUNT_CLAIM = "https://api.openai.com/auth"
REFRESH_SKEW_MS = 5 * 60 * 1000


class CredentialServices(Protocol):
    async def credential_payload_for_lease(
        self, account: dict[str, Any], needs_refresh: Callable[[dict[str, Any]], bool]
    ) -> dict[str, Any]: ...


@dataclass(frozen=True, slots=True)
class Lease:
    account_id: str
    access_token: str
    expires_at_ms: int


def _jwt_payload(token: str) -> dict[str, Any]:
    try:
        part = token.split(".")[1]
        value = json.loads(base64.urlsafe_b64decode(part + "=" * (-len(part) % 4)))
    except (IndexError, ValueError, TypeError, json.JSONDecodeError) as error:
        raise ValueError("token is not a valid JWT") from error
    if not isinstance(value, dict):
        raise ValueError("token JWT payload is not an object")
    return value


def _lease_from_payload(vault: Vault, payload: dict[str, Any]) -> Lease:
    try:
        auth = json.loads(vault.auth_json(payload))
        tokens = auth["tokens"]
        access_token = tokens["access_token"]
        claims = _jwt_payload(access_token)
        account_claim = claims[ACCOUNT_CLAIM]
        account_id = account_claim["chatgpt_account_id"]
        expires_at_ms = claims["exp"] * 1000
    except (KeyError, TypeError, ValueError, json.JSONDecodeError) as error:
        raise WindowkeeperError(
            "CREDENTIAL_FORMAT_INVALID", "The active credential cannot be leased", 503
        ) from error
    if (
        not isinstance(access_token, str)
        or not isinstance(account_id, str)
        or not account_id
        or not isinstance(expires_at_ms, int)
        or expires_at_ms <= 0
    ):
        raise WindowkeeperError(
            "CREDENTIAL_FORMAT_INVALID", "The active credential cannot be leased", 503
        )
    return Lease(account_id, access_token, expires_at_ms)


class CredentialAuthority:
    def __init__(self, services: CredentialServices, vault: Vault) -> None:
        self.services = services
        self.vault = vault

    async def lease(
        self, account: dict[str, Any], now_ms: int, *, force_refresh: bool = False
    ) -> Lease:
        def stale(payload: dict[str, Any]) -> bool:
            if force_refresh:
                return True
            try:
                return _lease_from_payload(self.vault, payload).expires_at_ms <= now_ms + REFRESH_SKEW_MS
            except WindowkeeperError:
                return True

        payload = await self.services.credential_payload_for_lease(account, stale)
        lease = _lease_from_payload(self.vault, payload)
        if lease.expires_at_ms <= now_ms:
            raise WindowkeeperError("CREDENTIAL_EXPIRED", "The active credential is expired", 503)
        return lease
