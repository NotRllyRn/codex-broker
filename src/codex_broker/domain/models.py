from dataclasses import dataclass, field
from enum import StrEnum


class LoginMethod(StrEnum):
    CHATGPT_BROWSER = "CHATGPT_BROWSER"
    CHATGPT_DEVICE_CODE = "CHATGPT_DEVICE_CODE"
    MANUAL_TOKENS = "MANUAL_TOKENS"


class OverallState(StrEnum):
    DISABLED = "DISABLED"
    ACTION_REQUIRED = "ACTION_REQUIRED"
    ERROR = "ERROR"
    STARTING = "STARTING"
    WARNING = "WARNING"
    HEALTHY = "HEALTHY"


@dataclass(frozen=True, slots=True)
class RawWindow:
    slot: str
    used_percent: int | None
    duration_minutes: int | None
    resets_at_s: int | None


@dataclass(frozen=True, slots=True)
class NormalizedUsage:
    selected_limit_id: str | None
    short: RawWindow | None
    weekly: RawWindow | None
    others: tuple[RawWindow, ...] = ()


@dataclass(slots=True)
class AccountSummary:
    account_id: str
    public_token: str
    display_name: str
    labels: list[str] = field(default_factory=list)
    enabled: bool = True
    overall_state: str = "STARTING"
    # pi-lens-ignore: python-hardcoded-secrets
    auth_state: str = "UNCONFIGURED"
    usage_state: str = "UNKNOWN"
    short_percent: int | None = None
    short_reset_ms: int | None = None
    weekly_percent: int | None = None
    weekly_reset_ms: int | None = None
    last_refresh_ms: int | None = None
    active_operation: str | None = None
    evidence: str = "No complete usage read yet"
