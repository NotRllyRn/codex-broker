import asyncio
import json
import socket
from pathlib import Path
from unittest.mock import AsyncMock

import pytest

from codex_broker.database import Database
from codex_broker.vault import Vault, decode_key, generate_key
from codex_broker.webhooks import WebhookDispatcher, _resolve_public_destination


@pytest.mark.asyncio
async def test_webhook_resolution_pins_a_public_address(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    lookup = AsyncMock(
        return_value=[(socket.AF_INET, socket.SOCK_STREAM, 6, "", ("93.184.216.34", 443))]
    )
    monkeypatch.setattr(asyncio.get_running_loop(), "getaddrinfo", lookup)
    connect_url, host, sni = await _resolve_public_destination(
        "https://hooks.example.test/delivery?id=1"
    )
    assert connect_url == "https://93.184.216.34:443/delivery?id=1"
    assert host == "hooks.example.test"
    assert sni == "hooks.example.test"


@pytest.mark.asyncio
async def test_webhook_body_is_immutable_and_destination_is_encrypted(tmp_path: Path) -> None:
    database = Database(tmp_path / "db.sqlite")
    database.start()
    instance = await database.call(
        lambda connection: str(
            connection.execute("SELECT instance_uuid FROM instance_metadata").fetchone()[0]
        )
    )
    dispatcher = WebhookDispatcher(database, Vault(decode_key(generate_key()), instance))
    with pytest.raises(ValueError):
        await dispatcher.create_destination("Private", "https://127.0.0.1/hook")
    destination = await dispatcher.create_destination(
        "Test", "https://example.test/hook", "signing-secret"
    )
    slack = await dispatcher.create_destination(
        "Slack", "https://hooks.slack.test/services/a", kind="slack"
    )
    discord = await dispatcher.create_destination(
        "Discord", "https://discord.test/api/webhooks/a", kind="discord"
    )
    event = await dispatcher.emit(
        "incident.opened",
        "account:a",
        {
            "account_name": "Arina",
            "account_email": "arina@example.test",
            "incident_status": "OPEN",
            "severity": "ERROR",
            "summary": "Activation outcome could not be proven",
            "cause_code": "ACTIVATION_AMBIGUOUS",
            "cause_summary": "Codex completion was not observed",
            "reason": "Replay is blocked to prevent duplicate usage.",
            "recommended_action": "Review the latest activation and acknowledge the ambiguity.",
            "occurrence_count": 2,
            "incident_id": "incident-1",
        },
    )
    row = await database.call(
        lambda connection: connection.execute(
            "SELECT immutable_body FROM webhook_deliveries WHERE destination_id=?", (destination,)
        ).fetchone()
    )
    generic_body = json.loads(bytes(row[0]))
    assert generic_body["notification"] == {
        "source": "WINDOWKEEPER",
        "code": "WK-101",
        "title": "INCIDENT OPENED",
    }
    assert generic_body["data"]["account_name"] == "Arina"
    assert event.encode() in bytes(row[0])
    provider_rows = await database.call(
        lambda connection: connection.execute(
            "SELECT destination_id,immutable_body FROM webhook_deliveries WHERE destination_id IN (?,?)",
            (slack, discord),
        ).fetchall()
    )
    provider_bodies = {item[0]: json.loads(bytes(item[1])) for item in provider_rows}
    assert provider_bodies[slack]["blocks"][0]["text"]["text"] == "WINDOWKEEPER · WK-101"
    assert "Arina &lt;arina@example.test&gt;" in provider_bodies[slack]["blocks"][1]["text"]["text"]
    assert "How to fix:" in provider_bodies[slack]["text"]
    assert provider_bodies[discord]["content"] == "WINDOWKEEPER · WK-101"
    assert provider_bodies[discord]["allowed_mentions"] == {"parse": []}
    assert "ACTIVATION_AMBIGUOUS" in provider_bodies[discord]["embeds"][0]["description"]
    stored = await database.call(
        lambda connection: connection.execute(
            "SELECT encrypted_url FROM webhook_destinations WHERE destination_id=?", (destination,)
        ).fetchone()[0]
    )
    assert b"https://example.test" not in bytes(stored)
    test_event = await dispatcher.test(destination)
    tested_destinations = await database.call(
        lambda connection: connection.execute(
            "SELECT destination_id FROM webhook_deliveries WHERE event_id=?",
            (test_event,),
        ).fetchall()
    )
    assert [row[0] for row in tested_destinations] == [destination]
    test_body = await database.call(
        lambda connection: json.loads(
            bytes(
                connection.execute(
                    "SELECT canonical_body FROM webhook_events WHERE event_id=?", (test_event,)
                ).fetchone()[0]
            )
        )
    )
    assert test_body["notification"]["code"] == "WK-900"
    assert "No action required" in test_body["data"]["recommended_action"]
    for event_type in ("incident.updated", "incident.resolved"):
        await dispatcher.emit(
            event_type,
            "account:a",
            {"account_name": "Arina", "summary": event_type, "incident_status": "OPEN"},
            destination_id=destination,
        )
    event_rows = await database.call(
        lambda connection: connection.execute(
            "SELECT event_type,canonical_body FROM webhook_events WHERE event_type IN ('incident.updated','incident.resolved')"
        ).fetchall()
    )
    event_codes = {row[0]: json.loads(bytes(row[1]))["notification"]["code"] for row in event_rows}
    assert event_codes == {"incident.updated": "WK-102", "incident.resolved": "WK-103"}
    await database.close()
