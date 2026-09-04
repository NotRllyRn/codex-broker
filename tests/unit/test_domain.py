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


def test_freshness_and_status_are_conservative() -> None:
    assert freshness(None, 1_000) == "UNKNOWN"
    assert freshness(1_000, 1_000 + 31 * 60_000) == "STALE"
    assert (
        overall_state(
            enabled=True,
            auth_state="AUTH_REQUIRED",
            worker_state="STOPPED",
            usage_state="FRESH",
            has_open_error=False,
        )
        == "ACTION_REQUIRED"
    )
