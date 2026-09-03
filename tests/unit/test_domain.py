from codex_broker.domain.models import RawWindow
from codex_broker.domain.scheduling import decide_schedule, deterministic_jitter
from codex_broker.domain.status import overall_state
from codex_broker.domain.usage import clamped_percent, freshness, normalize_usage


def test_usage_normalization_uses_duration_semantics() -> None:
    usage = normalize_usage(
        {
            "rateLimitsByLimitId": {
                "codex": {
                    "windows": [
                        {
                            "name": "week",
                            "usedPercent": 33,
                            "windowDurationMins": 10_080,
                            "resetsAt": 200,
                        },
                        {
                            "name": "short",
                            "usedPercent": 140,
                            "windowDurationMins": 300,
                            "resetsAt": 100,
                        },
                    ]
                }
            }
        }
    )
    assert usage.short and usage.short.slot == "short"
    assert usage.weekly and usage.weekly.slot == "week"
    assert clamped_percent(140) == 100


def test_schedule_is_deterministic_and_deduplicated() -> None:
    short = RawWindow("short", 10, 300, 2_000)
    first = decide_schedule(
        account_id="a",
        enabled=True,
        auth_verified=True,
        short=short,
        now_ms=1_000_000,
        safety_delay_seconds=60,
        jitter_max_seconds=30,
        last_successful_activation_ms=500_000,
    )
    second = decide_schedule(
        account_id="a",
        enabled=True,
        auth_verified=True,
        short=short,
        now_ms=1_000_000,
        safety_delay_seconds=60,
        jitter_max_seconds=30,
        last_successful_activation_ms=500_000,
    )
    assert first == second
    assert first.window_key == "reported:2000"
    assert deterministic_jitter("a", first.window_key, 30) <= 30
    duplicate = decide_schedule(
        account_id="a",
        enabled=True,
        auth_verified=True,
        short=short,
        now_ms=1_000_000,
        safety_delay_seconds=60,
        jitter_max_seconds=30,
        existing_window_keys={first.window_key},
        last_successful_activation_ms=500_000,
    )
    assert duplicate.run_at_ms is None


def test_schedule_waits_while_either_usage_window_is_exhausted() -> None:
    for short, weekly in (
        (RawWindow("short", 100, 300, 2_000), RawWindow("weekly", 10, 10_080, 3_000)),
        (RawWindow("short", 0, 300, 2_000), RawWindow("weekly", 100, 10_080, 3_000)),
    ):
        decision = decide_schedule(
            account_id="limited",
            enabled=True,
            auth_verified=True,
            short=short,
            weekly=weekly,
            now_ms=1_000_000,
            safety_delay_seconds=60,
            jitter_max_seconds=0,
            last_successful_activation_ms=500_000,
            consistent_observations=2,
        )
        assert not decision.window_key
        assert decision.reason == "usage is exhausted until a reset"


def test_schedule_starts_an_account_without_a_successful_activation() -> None:
    decision = decide_schedule(
        account_id="new",
        enabled=True,
        auth_verified=True,
        short=RawWindow("short", 0, 300, 19_000),
        now_ms=1_000_000,
        safety_delay_seconds=60,
        jitter_max_seconds=30,
    )
    assert decision.window_key == "initial"
    assert decision.run_at_ms == 1_000_000


def test_schedule_uses_last_success_when_an_idle_reset_keeps_moving() -> None:
    decision = decide_schedule(
        account_id="active",
        enabled=True,
        auth_verified=True,
        short=RawWindow("short", 0, 300, 19_600),
        now_ms=1_600_000,
        safety_delay_seconds=60,
        jitter_max_seconds=30,
        last_successful_activation_ms=1_000_000,
        consistent_observations=2,
    )
    assert decision.window_key == "estimated:1000000:300"
    assert decision.source == "OBSERVED_DURATION_FALLBACK"


def test_freshness_and_status_are_conservative() -> None:
    assert freshness(None, 1_000) == "UNKNOWN"
    assert freshness(1_000, 1_000 + 31 * 60_000) == "STALE"
    assert (
        overall_state(
            enabled=True,
            auth_state="AUTH_REQUIRED",
            worker_state="STOPPED",
            usage_state="FRESH",
            activation_state="UNSCHEDULED",
        )
        == "ACTION_REQUIRED"
    )
