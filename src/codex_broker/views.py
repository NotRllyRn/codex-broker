from dataclasses import asdict
from typing import Any

from codex_broker.clock import iso_time
from codex_broker.domain.models import AccountSummary


def account_view(account: AccountSummary) -> dict[str, Any]:
    value = asdict(account)
    for source, target in (
        ("short_reset_ms", "short_reset"),
        ("weekly_reset_ms", "weekly_reset"),
        ("last_refresh_ms", "last_refresh"),
    ):
        value[target] = iso_time(value.pop(source))
    value["short_percent_display"] = min(100, max(0, account.short_percent or 0))
    value["weekly_percent_display"] = min(100, max(0, account.weekly_percent or 0))
    return value
