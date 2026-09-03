import pytest

from codex_broker.events import Broadcaster


@pytest.mark.asyncio
async def test_replay_overflow_emits_gap_without_blocking() -> None:
    events = Broadcaster(replay_size=10, client_size=2)
    for index in range(5):
        events.publish("account.updated", {"index": index})
    stream = events.subscribe(0)
    try:
        first = await anext(stream)
        second = await anext(stream)
    finally:
        await stream.aclose()
    assert b"event: gap" in first
    assert b'"index":4' in second
