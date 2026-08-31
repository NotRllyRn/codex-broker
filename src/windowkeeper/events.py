import asyncio
import json
from collections import deque
from collections.abc import AsyncGenerator
from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True, slots=True)
class Event:
    id: int
    name: str
    data: dict[str, Any]

    def encode(self) -> bytes:
        return f"id: {self.id}\nevent: {self.name}\ndata: {json.dumps(self.data, separators=(',', ':'))}\n\n".encode()


class Broadcaster:
    def __init__(self, replay_size: int = 2_000, client_size: int = 256) -> None:
        self._sequence = 0
        self._ring: deque[Event] = deque(maxlen=replay_size)
        self._clients: set[asyncio.Queue[Event]] = set()
        self._client_size = client_size

    def publish(self, name: str, data: dict[str, Any]) -> Event:
        self._sequence += 1
        event = Event(self._sequence, name, data)
        self._ring.append(event)
        for client in tuple(self._clients):
            try:
                client.put_nowait(event)
            except asyncio.QueueFull as overflow:
                del overflow
                try:
                    client.get_nowait()
                    client.put_nowait(Event(self._sequence, "gap", {"reason": "slow_client"}))
                except asyncio.QueueFull as repeated_overflow:
                    del repeated_overflow
                    self._clients.discard(client)
        return event

    async def subscribe(self, last_event_id: int | None = None) -> AsyncGenerator[bytes, None]:
        queue: asyncio.Queue[Event] = asyncio.Queue(self._client_size)
        if last_event_id is not None:
            replay = [event for event in self._ring if event.id > last_event_id]
            replay_gap = bool(self._ring and last_event_id < self._ring[0].id - 1)
            if replay_gap or len(replay) > self._client_size:
                queue.put_nowait(Event(self._sequence, "gap", {"reason": "replay_unavailable"}))
                available = max(0, self._client_size - 1)
                replay = replay[-available:] if available else []
            for event in replay:
                queue.put_nowait(event)
        self._clients.add(queue)
        try:
            while True:
                try:
                    event = await asyncio.wait_for(queue.get(), 15)
                    yield event.encode()
                except TimeoutError:
                    yield b": heartbeat\n\n"
        finally:
            self._clients.discard(queue)
