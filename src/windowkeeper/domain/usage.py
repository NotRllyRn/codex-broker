from collections.abc import Mapping, Sequence
from typing import Any

from windowkeeper.domain.models import NormalizedUsage, RawWindow

WEEK_MINUTES = 10_080
WEEK_TOLERANCE = 0.05
DAY_MINUTES = 1_440


def _window(value: Mapping[str, Any], slot: str) -> RawWindow:
    return RawWindow(
        slot=slot,
        used_percent=value.get("usedPercent"),
        duration_minutes=value.get("windowDurationMins"),
        resets_at_s=value.get("resetsAt"),
    )


def normalize_usage(payload: Mapping[str, Any]) -> NormalizedUsage:
    limits = payload.get("rateLimitsByLimitId") or {}
    selected_id = "codex" if "codex" in limits else None
    source = limits.get(selected_id, {}) if selected_id else payload
    raw: Sequence[Mapping[str, Any]] = source.get("windows") or [
        value for key in ("primary", "secondary") if (value := source.get(key))
    ]
    windows = [_window(value, str(value.get("name", index))) for index, value in enumerate(raw)]
    weekly = [
        item
        for item in windows
        if item.duration_minutes
        and abs(item.duration_minutes - WEEK_MINUTES) <= WEEK_MINUTES * WEEK_TOLERANCE
    ]
    short = sorted(
        (
            item
            for item in windows
            if item not in weekly
            and item.duration_minutes
            and 0 < item.duration_minutes < DAY_MINUTES
        ),
        key=lambda item: item.duration_minutes or DAY_MINUTES,
    )
    chosen_short = (
        short[0]
        if len(short) == 1 or (short and short[0].duration_minutes != short[1].duration_minutes)
        else None
    )
    chosen_weekly = weekly[0] if len(weekly) == 1 else None
    used = {id(item) for item in (chosen_short, chosen_weekly) if item}
    return NormalizedUsage(
        selected_id,
        chosen_short,
        chosen_weekly,
        tuple(item for item in windows if id(item) not in used),
    )


def clamped_percent(value: int | None) -> int | None:
    return max(0, min(100, value)) if value is not None else None


def freshness(last_success_ms: int | None, now_ms: int, poll_seconds: int = 300) -> str:
    if not last_success_ms:
        return "UNKNOWN"
    age = now_ms - last_success_ms
    if age <= poll_seconds * 2 * 1000:
        return "FRESH"
    if age <= 30 * 60 * 1000:
        return "AGING"
    return "STALE"
