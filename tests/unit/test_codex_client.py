import asyncio
from types import SimpleNamespace
from typing import Any, cast

import pytest

from codex_broker.codex.client import AppServerClient


@pytest.mark.asyncio
async def test_stderr_eof_is_harmless() -> None:
    stderr = asyncio.StreamReader()
    stderr.feed_eof()
    client = object.__new__(AppServerClient)
    client.process = cast(Any, SimpleNamespace(stderr=stderr))
    await client._drain_stderr()


@pytest.mark.asyncio
async def test_transport_close_wakes_notification_waiter() -> None:
    client = object.__new__(AppServerClient)
    client._notifications = asyncio.Queue(maxsize=1)
    client._terminal_error = None
    notifications = client.notifications()
    waiting: asyncio.Future[dict[str, Any]] = asyncio.ensure_future(anext(notifications))
    await asyncio.sleep(0)
    client._wake_notification_waiters()
    with pytest.raises(StopAsyncIteration):
        await waiting
