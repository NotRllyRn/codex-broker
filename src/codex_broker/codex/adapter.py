from dataclasses import dataclass
from typing import Any

from ..domain.models import LoginMethod
from .client import AppServerClient, WriteEvidence

# Official Codex standard-tier milli-credits per 1M input, cached-input, and output tokens.
# Source: https://developers.openai.com/codex/pricing (verified 2026-07-26).
CREDIT_RATES: dict[str, tuple[int, int, int]] = {
    "gpt-5.6-sol": (125_000, 12_500, 750_000),
    "gpt-5.6-terra": (62_500, 6_250, 375_000),
    "gpt-5.6-luna": (25_000, 2_500, 150_000),
    "gpt-5.5": (125_000, 12_500, 750_000),
    "gpt-5.4": (62_500, 6_250, 375_000),
    "gpt-5.4-mini": (18_750, 1_875, 113_000),
}
PRICING_VERIFIED_AT = "2026-07-26"


@dataclass(frozen=True, slots=True)
class ActivationModel:
    model: str
    effort: str


def select_activation_model(models: list[dict[str, Any]]) -> ActivationModel:
    priced = [
        (model, CREDIT_RATES[str(model.get("model"))])
        for model in models
        if not model.get("hidden")
        and "text" in model.get("inputModalities", ["text", "image"])
        and str(model.get("model")) in CREDIT_RATES
    ]
    cheapest = [
        model
        for model, rates in priced
        if all(
            all(value <= other for value, other in zip(rates, other_rates, strict=True))
            for _, other_rates in priced
        )
    ]
    if not cheapest:
        raise RuntimeError("No available Codex model has unambiguously cheapest verified pricing")
    selected = min(cheapest, key=lambda model: str(model["model"]))
    efforts = selected.get("supportedReasoningEfforts") or []
    if not efforts:
        raise RuntimeError("The cheapest available Codex model advertises no reasoning effort")
    return ActivationModel(str(selected["model"]), str(efforts[0]["reasoningEffort"]))


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

    async def activation_model(self) -> ActivationModel:
        models: list[dict[str, Any]] = []
        cursor: str | None = None
        while True:
            params: dict[str, Any] = {"includeHidden": False, "limit": 100}
            if cursor:
                params["cursor"] = cursor
            result, _ = await self.client.request("model/list", params)
            models.extend(result.get("data") or [])
            next_cursor = result.get("nextCursor")
            if not next_cursor:
                return select_activation_model(models)
            if next_cursor == cursor:
                raise RuntimeError("Codex model pagination did not advance")
            cursor = str(next_cursor)

    async def create_thread(self, cwd: str, choice: ActivationModel) -> str:
        result, _ = await self.client.request(
            "thread/start",
            {
                "cwd": cwd,
                "model": choice.model,
                "serviceTier": "default",
                "approvalPolicy": "never",
                "sandbox": "read-only",
                "ephemeral": False,
                "experimentalRawEvents": False,
            },
        )
        if result.get("model") != choice.model or result.get("serviceTier") not in {
            None,
            "default",
        }:
            raise RuntimeError("Codex did not honor the selected low-cost model")
        thread = result.get("thread") or result
        return str(thread.get("id"))

    async def start_turn(
        self,
        thread_id: str,
        activation_id: str,
        prompt: str,
        choice: ActivationModel,
    ) -> tuple[str, WriteEvidence]:
        result, evidence = await self.client.request(
            "turn/start",
            {
                "threadId": thread_id,
                "input": [{"type": "text", "text": prompt, "text_elements": []}],
                "clientUserMessageId": activation_id,
                "model": choice.model,
                "effort": choice.effort,
                "serviceTier": "default",
            },
            timeout=60,
        )
        turn = result.get("turn") or result
        return str(turn.get("id")), evidence

    async def read_thread(self, thread_id: str) -> dict[str, Any]:
        result, _ = await self.client.request(
            "thread/read", {"threadId": thread_id, "includeTurns": True}
        )
        return result
