import asyncio
import hashlib
import hmac
import ipaddress
import json
import socket
import sqlite3
from datetime import UTC, datetime
from typing import Any
from urllib.parse import urlsplit

import httpx

from codex_broker.clock import SystemClock
from codex_broker.database import Database
from codex_broker.ids import new_id
from codex_broker.redaction import redact
from codex_broker.vault import Vault

RETRY_SECONDS = (60, 300, 1_800, 7_200, 21_600, 43_200, 86_400, 86_400)
DESTINATION_KINDS = {"generic", "slack", "discord"}
EVENT_CODES = {
    "incident.opened": "CB-101",
    "incident.updated": "CB-102",
    "incident.resolved": "CB-103",
    "codex-broker.test": "CB-900",
}
EVENT_TITLES = {
    "incident.opened": "INCIDENT OPENED",
    "incident.updated": "INCIDENT UPDATED",
    "incident.resolved": "INCIDENT RESOLVED",
    "codex-broker.test": "WEBHOOK TEST",
}


def _text(value: Any, limit: int = 600) -> str:
    return " ".join(str(value or "").split())[:limit]


def _notification_text(event: dict[str, Any]) -> str:
    data = event["data"]
    notification = event["notification"]
    account = _text(data.get("account_name") or event["subject"])
    email = _text(data.get("account_email"))
    lines = [
        f"CODEX BROKER · {notification['code']}",
        str(notification["title"]),
        "",
        f"Account: {account}{f' <{email}>' if email else ''}",
    ]
    status = data.get("incident_status") or data.get("delivery_status")
    if status or data.get("severity"):
        lines.append(
            f"Status: {_text(status or 'UNKNOWN')} · Severity: {_text(data.get('severity', 'INFO'))}"
        )
    lines.append(
        f"What happened: {_text(data.get('summary') or data.get('message') or event['subject'])}"
    )
    cause = _text(data.get("cause_summary"))
    if cause and cause != _text(data.get("summary")):
        lines.append(f"Cause: {_text(data.get('cause_code'))} — {cause}")
    if reason := _text(data.get("reason")):
        lines.append(f"Why it matters: {reason}")
    if action := _text(data.get("recommended_action")):
        lines.append(f"How to fix: {action}")
    if count := data.get("occurrence_count"):
        lines.append(f"Occurrences: {count}")
    if incident_id := _text(data.get("incident_id"), 64):
        lines.append(f"Incident ID: {incident_id}")
    lines.extend(
        (
            f"Event ID: {_text(event['event_id'], 64)}",
            f"Occurred: {event['occurred_at']}",
        )
    )
    return "\n".join(lines)


def _provider_body(kind: str, event: dict[str, Any]) -> bytes:
    message = _notification_text(event)
    if kind == "slack":
        escaped = message.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
        payload: dict[str, Any] = {
            "text": escaped[:3_000],
            "blocks": [
                {
                    "type": "header",
                    "text": {
                        "type": "plain_text",
                        "text": message.splitlines()[0][:150],
                    },
                },
                {
                    "type": "section",
                    "text": {
                        "type": "mrkdwn",
                        "text": escaped.split("\n", 1)[1].strip()[:3_000],
                    },
                },
            ],
        }
    elif kind == "discord":
        payload = {
            "content": message.splitlines()[0],
            "allowed_mentions": {"parse": []},
            "embeds": [
                {
                    "title": str(event["notification"]["title"]),
                    "description": message.split("\n", 1)[1].strip()[:4_000],
                    "footer": {"text": f"Codex Broker event {event['event_id']}"},
                }
            ],
        }
    else:
        payload = event
    return json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()


def _safe_literal_host(hostname: str) -> bool:
    if hostname.lower() == "localhost" or hostname.lower().endswith(".localhost"):
        return False
    try:
        return ipaddress.ip_address(hostname).is_global
    except ValueError:
        return True


async def _resolve_public_destination(url: str) -> tuple[str, str, str]:
    parsed = urlsplit(url)
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username
        or parsed.password
        or not _safe_literal_host(parsed.hostname)
    ):
        raise ValueError("webhook destination is not public")
    addresses = await asyncio.get_running_loop().getaddrinfo(
        parsed.hostname, parsed.port or 443, type=socket.SOCK_STREAM
    )
    resolved = sorted({address[4][0] for address in addresses})
    if not resolved or any(not ipaddress.ip_address(address).is_global for address in resolved):
        raise ValueError("webhook destination resolved to a non-public address")
    selected = resolved[0]
    connect_host = f"[{selected}]" if ipaddress.ip_address(selected).version == 6 else selected
    connect_url = parsed._replace(netloc=f"{connect_host}:{parsed.port or 443}").geturl()
    return connect_url, parsed.netloc, parsed.hostname


def _integer(value: Any) -> int:
    try:
        return int(value)
    except (TypeError, ValueError, OverflowError) as error:
        raise RuntimeError("stored webhook attempt count is invalid") from error


class WebhookDispatcher:
    def __init__(self, database: Database, vault: Vault) -> None:
        self.database = database
        self.vault = vault
        self.clock = SystemClock()
        self._task: asyncio.Task[None] | None = None

    def start(self) -> None:
        self._task = asyncio.create_task(self._loop())

    async def close(self) -> None:
        if self._task:
            self._task.cancel()
            await asyncio.gather(self._task, return_exceptions=True)
            self._task = None

    async def create_destination(
        self,
        display_name: str,
        url: str,
        signing_secret: str | None = None,
        kind: str = "generic",
    ) -> str:
        try:
            parsed = urlsplit(url)
        except ValueError as error:
            raise ValueError("webhook URL is invalid") from error
        if parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.password:
            raise ValueError("webhook URL must be an absolute HTTPS URL without credentials")
        if not _safe_literal_host(parsed.hostname):
            raise ValueError("webhook URL host must be public")
        kind = kind.lower()
        if kind not in DESTINATION_KINDS:
            raise ValueError("webhook destination kind is unsupported")
        destination_id = new_id()
        now = self.clock.now_ms()
        encrypted_url = self.vault.seal_text(f"webhook:{destination_id}:url", url)
        encrypted_secret = (
            self.vault.seal_text(f"webhook:{destination_id}:secret", signing_secret)
            if signing_secret
            else None
        )

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "INSERT INTO webhook_destinations VALUES(?,?,?,?,?,?,?,?,?,?)",
                (
                    destination_id,
                    " ".join(display_name.split())[:80],
                    kind,
                    1,
                    hashlib.sha256(encrypted_url).digest()[:12],
                    encrypted_url,
                    hashlib.sha256(encrypted_secret).digest()[:12] if encrypted_secret else None,
                    encrypted_secret,
                    now,
                    now,
                ),
            )

        await self.database.transaction(work)
        return destination_id

    async def destinations(self) -> list[dict[str, Any]]:
        def work(connection: sqlite3.Connection) -> list[dict[str, Any]]:
            return [
                {
                    "destination_id": row["destination_id"],
                    "display_name": row["display_name"],
                    "kind": row["kind"],
                    "enabled": bool(row["enabled"]),
                    "created_at_ms": row["created_at_ms"],
                }
                for row in connection.execute(
                    "SELECT destination_id,display_name,kind,enabled,created_at_ms FROM webhook_destinations ORDER BY lower(display_name)"
                )
            ]

        return await self.database.call(work)

    async def set_enabled(self, destination_id: str, enabled: bool) -> None:
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> None:
            cursor = connection.execute(
                "UPDATE webhook_destinations SET enabled=?,updated_at_ms=? WHERE destination_id=?",
                (1 if enabled else 0, now, destination_id),
            )
            if cursor.rowcount != 1:
                raise ValueError("webhook destination not found")

        await self.database.transaction(work)

    async def delete_destination(self, destination_id: str) -> None:
        def work(connection: sqlite3.Connection) -> None:
            cursor = connection.execute(
                "DELETE FROM webhook_destinations WHERE destination_id=?", (destination_id,)
            )
            if cursor.rowcount != 1:
                raise ValueError("webhook destination not found")

        await self.database.transaction(work)

    async def emit(
        self,
        event_type: str,
        subject: str,
        data: dict[str, Any],
        incident_id: str | None = None,
        destination_id: str | None = None,
    ) -> str:
        event_id = new_id()
        now = self.clock.now_ms()
        event = {
            "schema": "codex-broker.webhook/v1",
            "event_id": event_id,
            "event_type": event_type,
            "subject": subject,
            "occurred_at_ms": now,
            "occurred_at": datetime.fromtimestamp(now / 1000, UTC).isoformat(),
            "notification": {
                "source": "CODEX_BROKER",
                "code": EVENT_CODES.get(event_type, "CB-999"),
                "title": EVENT_TITLES.get(event_type, event_type.replace(".", " ").upper()),
            },
            "data": redact(data),
        }
        body = _provider_body("generic", event)

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "INSERT INTO webhook_events VALUES(?,?,?,?,?,?,?)",
                (event_id, event_type, subject, now, body, incident_id, now),
            )
            destinations = connection.execute(
                "SELECT destination_id,kind FROM webhook_destinations WHERE destination_id=?"
                if destination_id
                else "SELECT destination_id,kind FROM webhook_destinations WHERE enabled=1",
                (destination_id,) if destination_id else (),
            ).fetchall()
            if destination_id and not destinations:
                raise ValueError("webhook destination not found")
            for destination in destinations:
                connection.execute(
                    "INSERT INTO webhook_deliveries VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                    (
                        new_id(),
                        event_id,
                        destination["destination_id"],
                        "PENDING",
                        0,
                        now,
                        _provider_body(str(destination["kind"]), event),
                        "application/json",
                        None,
                        None,
                        None,
                        None,
                        None,
                        now,
                        None,
                    ),
                )

        await self.database.transaction(work)
        return event_id

    async def test(self, destination_id: str) -> str:
        return await self.emit(
            "codex-broker.test",
            f"destination:{destination_id}",
            {
                "destination_id": destination_id,
                "message": "Codex Broker successfully created a test notification.",
                "delivery_status": "DELIVERED",
                "severity": "INFO",
                "recommended_action": "No action required. This confirms the destination accepts Codex Broker webhooks.",
            },
            destination_id=destination_id,
        )

    async def _loop(self) -> None:
        while True:
            try:
                delivery = await self._claim()
                if delivery:
                    await self._deliver(delivery)
                else:
                    await asyncio.sleep(2)
            except Exception:
                await asyncio.sleep(5)

    async def _claim(self) -> dict[str, Any] | None:
        now = self.clock.now_ms()
        token = new_id()

        def work(connection: sqlite3.Connection) -> dict[str, Any] | None:
            row = connection.execute(
                """SELECT d.*,w.encrypted_url,w.encrypted_signing_secret
                FROM webhook_deliveries d JOIN webhook_destinations w USING(destination_id)
                WHERE d.state IN('PENDING','RETRY_SCHEDULED') AND d.next_attempt_at_ms<=? AND w.enabled=1
                ORDER BY d.next_attempt_at_ms LIMIT 1""",
                (now,),
            ).fetchone()
            if not row:
                return None
            changed = connection.execute(
                "UPDATE webhook_deliveries SET state='LEASED',lease_token=?,lease_expires_at_ms=? WHERE delivery_id=? AND state IN('PENDING','RETRY_SCHEDULED')",
                (token, now + 30_000, row["delivery_id"]),
            ).rowcount
            return dict(row) | {"lease_token": token} if changed else None

        return await self.database.transaction(work)

    async def _deliver(self, delivery: dict[str, Any]) -> None:
        url = self.vault.open_text(delivery["encrypted_url"])
        secret = (
            self.vault.open_text(delivery["encrypted_signing_secret"])
            if delivery.get("encrypted_signing_secret")
            else None
        )
        body = bytes(delivery["immutable_body"])
        headers = {
            "Content-Type": delivery["content_type"],
            "User-Agent": "codex-broker/0.1",
            "X-Codex-Broker-Event-ID": delivery["event_id"],
        }
        if secret:
            headers["X-Codex-Broker-Signature"] = (
                "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
            )
        status: int | None = None
        excerpt = ""
        error_code: str | None = None
        provider_delay_seconds: int | None = None
        try:
            connect_url, host_header, sni_hostname = await _resolve_public_destination(url)
            headers["Host"] = host_header
            async with (
                httpx.AsyncClient(
                    follow_redirects=False, trust_env=False, timeout=httpx.Timeout(10, connect=3)
                ) as client,
                client.stream(
                    "POST",
                    connect_url,
                    content=body,
                    headers=headers,
                    extensions={"sni_hostname": sni_hostname},
                ) as response,
            ):
                status = response.status_code
                captured = bytearray()
                async for chunk in response.aiter_bytes():
                    captured.extend(chunk[: 512 - len(captured)])
                    if len(captured) >= 512:
                        break
                excerpt = str(redact(captured.decode(errors="replace")))
                if status == 429:
                    try:
                        provider_delay_seconds = min(
                            86_400, max(1, int(response.headers.get("Retry-After", "")))
                        )
                    except ValueError:
                        provider_delay_seconds = None
                if not 200 <= status < 300:
                    error_code = "WEBHOOK_HTTP_ERROR"
        except ValueError:
            error_code = "WEBHOOK_DESTINATION_BLOCKED"
        except (OSError, httpx.TimeoutException, httpx.NetworkError) as error:
            error_code = f"WEBHOOK_{type(error).__name__.upper()}"
        finally:
            url = "[REDACTED]"
            secret = None
        await self._finish(delivery, status, error_code, excerpt, provider_delay_seconds)

    async def _finish(
        self,
        delivery: dict[str, Any],
        status: int | None,
        error_code: str | None,
        excerpt: str,
        provider_delay_seconds: int | None = None,
    ) -> None:
        now = self.clock.now_ms()
        attempt = _integer(delivery["attempt_count"]) + 1
        succeeded = error_code is None and status is not None and 200 <= status < 300
        exhausted = attempt >= len(RETRY_SECONDS)
        state = "SUCCEEDED" if succeeded else "FAILED" if exhausted else "RETRY_SCHEDULED"
        retry_seconds = provider_delay_seconds or RETRY_SECONDS[attempt - 1]
        next_at = now if succeeded or exhausted else now + retry_seconds * 1000

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                """UPDATE webhook_deliveries SET state=?,attempt_count=?,next_attempt_at_ms=?,lease_token=NULL,
                lease_expires_at_ms=NULL,last_status_code=?,last_error_code=?,last_response_excerpt=?,completed_at_ms=?
                WHERE delivery_id=? AND lease_token=?""",
                (
                    state,
                    attempt,
                    next_at,
                    status,
                    error_code,
                    excerpt,
                    now if state in {"SUCCEEDED", "FAILED"} else None,
                    delivery["delivery_id"],
                    delivery["lease_token"],
                ),
            )

        await self.database.transaction(work)
