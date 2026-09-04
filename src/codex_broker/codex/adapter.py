from dataclasses import dataclass
from typing import Any

from ..domain.models import LoginMethod
from .client import AppServerClient


@dataclass(frozen=True, slots=True)
class Secret:
    _value: str

    def reveal(self) -> str:
        return self._value

    def __repr__(self) -> str:
        return "Secret('[REDACTED]')"

    def __str__(self) -> str:
        return "[REDACTED]"


@dataclass(frozen=True, slots=True)
class LoginInteraction:
    login_id: str
    method: LoginMethod
    auth_url: Secret | None = None
    verification_url: Secret | None = None
    user_code: Secret | None = None
    expires_at_ms: int | None = None


class CodexAdapter:
    def __init__(self, client: AppServerClient) -> None:
        self.client = client

    async def start_login(self, method: LoginMethod) -> LoginInteraction:
        if method == LoginMethod.MANUAL_TOKENS:
            raise ValueError("manual token import does not start OAuth")
        kind = "chatgpt" if method == LoginMethod.CHATGPT_BROWSER else "chatgptDeviceCode"
        params: dict[str, Any] = {"type": kind}
        if method == LoginMethod.CHATGPT_BROWSER:
            params |= {"useHostedLoginSuccessPage": True, "appBrand": "codex"}
        result, _ = await self.client.request("account/login/start", params)
        return LoginInteraction(
            login_id=str(result.get("loginId", "")),
            method=method,
            auth_url=Secret(result["authUrl"]) if result.get("authUrl") else None,
            verification_url=Secret(result["verificationUrl"])
            if result.get("verificationUrl")
            else None,
            user_code=Secret(result["userCode"]) if result.get("userCode") else None,
            expires_at_ms=result.get("expiresAt"),
        )

    async def cancel_login(self, login_id: str) -> None:
        await self.client.request("account/login/cancel", {"loginId": login_id})

    async def account(self, refresh_token: bool = False) -> dict[str, Any]:
        result, _ = await self.client.request("account/read", {"refreshToken": refresh_token})
        return result

    async def rate_limits(self) -> dict[str, Any]:
        result, _ = await self.client.request("account/rateLimits/read", {})
        return result
