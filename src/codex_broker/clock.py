import asyncio
import time
from datetime import UTC, datetime
from typing import Protocol


class Clock(Protocol):
    def now_ms(self) -> int: ...
    def monotonic(self) -> float: ...
    async def sleep(self, seconds: float) -> None: ...


class SystemClock:
    def now_ms(self) -> int:
        return time.time_ns() // 1_000_000

    def monotonic(self) -> float:
        return time.monotonic()

    async def sleep(self, seconds: float) -> None:
        await asyncio.sleep(seconds)


def iso_time(value_ms: int | None) -> str | None:
    return (
        datetime.fromtimestamp(value_ms / 1000, UTC).isoformat().replace("+00:00", "Z")
        if value_ms
        else None
    )
