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

    async def pulse_windows(self) -> dict[str, Any]:
        listed, _ = await self.client.request("model/list", {"limit": 100})
        model, effort = self._pulse_model(listed.get("data"))
        started, _ = await self.client.request(
            "thread/start",
            {"ephemeral": True, "model": model, "serviceTier": "default"},
        )
        thread_id = started.get("thread", {}).get("id")
        if not isinstance(thread_id, str) or not thread_id:
            raise RuntimeError("Codex returned no window-pulse thread ID")
        turn, _ = await self.client.request(
            "turn/start",
            {
                "threadId": thread_id,
                "input": [{"type": "text", "text": "Reply OK."}],
                "model": model,
                "effort": effort,
                "serviceTier": "default",
            },
        )
        await self._wait_for_turn(str(turn.get("turn", {}).get("id", "")))
        return await self.rate_limits()

    @staticmethod
    def _pulse_model(data: Any) -> tuple[str, str]:
        candidates: list[tuple[int, str, str]] = []
        for item in data if isinstance(data, list) else []:
            if not isinstance(item, dict) or item.get("hidden"):
                continue
            model = item.get("model")
            modalities = item.get("inputModalities", [])
            efforts = item.get("supportedReasoningEfforts", [])
            names = [
                value.get("reasoningEffort")
                for value in efforts
                if isinstance(value, dict)
            ]
            if isinstance(model, str) and "text" in modalities and names:
                effort = "minimal" if "minimal" in names else "low" if "low" in names else names[0]
                if isinstance(effort, str):
                    candidates.append((0 if "mini" in model else 1, model, effort))
        if not candidates:
            raise RuntimeError("Codex returned no text model for window pulse")
        _, model, effort = min(candidates)
        return model, effort

    async def _wait_for_turn(self, turn_id: str) -> None:
        if not turn_id:
            raise RuntimeError("Codex returned no window-pulse turn ID")
        async for message in self.client.notifications():
            if message.get("method") != "turn/completed":
                continue
            turn = (message.get("params") or {}).get("turn")
            if not isinstance(turn, dict) or turn.get("id") != turn_id:
                continue
            if turn.get("status") != "completed":
                raise RuntimeError("Codex window pulse failed")
            return
        raise RuntimeError("Codex closed before window pulse completed")
