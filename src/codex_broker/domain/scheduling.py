import hashlib

from codex_broker.domain.models import RawWindow, ScheduleDecision


def deterministic_jitter(account_id: str, window_key: str, maximum_seconds: int) -> int:
    if maximum_seconds <= 0:
        return 0
    digest = hashlib.sha256(f"{account_id}:{window_key}".encode()).digest()
    return int.from_bytes(digest[:8]) % (maximum_seconds + 1)


def decide_schedule(
    *,
    account_id: str,
    enabled: bool,
    auth_verified: bool,
    short: RawWindow | None,
    now_ms: int,
    weekly: RawWindow | None = None,
    safety_delay_seconds: int,
    jitter_max_seconds: int,
    existing_window_keys: set[str] | None = None,
    ambiguous_predecessor: bool = False,
    last_successful_activation_ms: int | None = None,
    consistent_observations: int = 0,
    estimated_enabled: bool = True,
) -> ScheduleDecision:
    existing = existing_window_keys or set()
    if not enabled or not auth_verified:
        return ScheduleDecision(None, None, "NONE", "UNKNOWN", reason="account is not eligible")
    if ambiguous_predecessor:
        return ScheduleDecision(
            None, None, "NONE", "UNKNOWN", reason="an ambiguous activation blocks scheduling"
        )
    if any(window and (window.used_percent or 0) >= 100 for window in (short, weekly)):
        return ScheduleDecision(
            None, None, "NONE", "CONFIRMED", reason="usage is exhausted until a reset"
        )
    if short and short.used_percent == 0 and not last_successful_activation_ms:
        return ScheduleDecision(
            "initial",
            None if "initial" in existing else now_ms,
            "INITIAL_WINDOW",
            "CONFIRMED",
            basis_duration_minutes=short.duration_minutes,
            reason="start the first managed window",
        )
    expected_reset_ms = (
        last_successful_activation_ms + short.duration_minutes * 60_000
        if last_successful_activation_ms and short and short.duration_minutes
        else None
    )
    if (
        estimated_enabled
        and short
        and short.used_percent == 0
        and short.resets_at_s
        and expected_reset_ms
        and short.resets_at_s * 1000 > expected_reset_ms + 60_000
        and consistent_observations >= 2
    ):
        key = f"estimated:{last_successful_activation_ms}:{short.duration_minutes}"
        jitter = deterministic_jitter(account_id, key, jitter_max_seconds)
        return ScheduleDecision(
            key,
            None if key in existing else expected_reset_ms + (safety_delay_seconds + jitter) * 1000,
            "OBSERVED_DURATION_FALLBACK",
            "ESTIMATED",
            basis_duration_minutes=short.duration_minutes,
            reason="reported idle reset moves with each observation",
        )
    if short and short.resets_at_s and short.resets_at_s * 1000 > now_ms:
        key = f"reported:{short.resets_at_s}"
        if key in existing:
            return ScheduleDecision(
                key, None, "REPORTED_RESET", "CONFIRMED", reason="window already has an attempt"
            )
        jitter = deterministic_jitter(account_id, key, jitter_max_seconds)
        return ScheduleDecision(
            key,
            short.resets_at_s * 1000 + (safety_delay_seconds + jitter) * 1000,
            "REPORTED_RESET",
            "CONFIRMED",
            short.resets_at_s,
            short.duration_minutes,
        )
    if (
        estimated_enabled
        and short
        and short.duration_minutes
        and 0 < short.duration_minutes < 1_440
        and last_successful_activation_ms
        and consistent_observations >= 2
    ):
        key = f"estimated:{last_successful_activation_ms}:{short.duration_minutes}"
        if key in existing:
            return ScheduleDecision(
                key,
                None,
                "OBSERVED_DURATION_FALLBACK",
                "ESTIMATED",
                reason="window already has an attempt",
            )
        jitter = deterministic_jitter(account_id, key, jitter_max_seconds)
        run_at = (
            last_successful_activation_ms
            + (short.duration_minutes * 60 + safety_delay_seconds + jitter) * 1000
        )
        return ScheduleDecision(
            key,
            run_at,
            "OBSERVED_DURATION_FALLBACK",
            "ESTIMATED",
            basis_duration_minutes=short.duration_minutes,
        )
    return ScheduleDecision(
        None, None, "NONE", "UNKNOWN", reason="insufficient fresh reset evidence"
    )
