import asyncio
import hashlib
import json
import logging
import secrets
import sqlite3
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any, Protocol, TypeVar, cast
from urllib.parse import parse_qs, urlencode, urlsplit

import httpx

from .clock import SystemClock
from .codex.adapter import PRICING_VERIFIED_AT, LoginInteraction
from .database import Database
from .domain.models import AccountSummary, LoginMethod, RawWindow
from .domain.scheduling import decide_schedule
from .domain.usage import normalize_usage
from .errors import Conflict, WindowkeeperError
from .ids import new_id, public_token
from .redaction import redact
from .security import digest
from .vault import Envelope, Vault

T = TypeVar("T")
PROMPT = 'Respond with exactly "OK" and perform no other actions.'
PROMPT_DIGEST = hashlib.sha256(PROMPT.encode()).digest()
INCIDENT_GUIDANCE = {
    "activation_ambiguous": (
        "Windowkeeper dispatched an activation but could not prove whether Codex completed it, so replay is blocked to prevent duplicate usage.",
        "Open the account, review the latest activation operation, then acknowledge the ambiguity to resume scheduling.",
    ),
    "activation_safety": (
        "Codex requested an action outside Windowkeeper's read-only, no-tool activation contract.",
        "Review the activation evidence, verify the pinned Codex release, then acknowledge the safety block only when it is understood.",
    ),
    "authentication_failed": (
        "Codex rejected or could not refresh the account credential, so usage refresh and activation cannot continue.",
        "Open the account and use Replace or repair credentials with device code or browser sign-in.",
    ),
    "credential_checkpoint": (
        "Codex may have changed its credential, but Windowkeeper could not safely persist the result.",
        "Stop account activity and preserve the quarantined runtime before restarting or reauthenticating.",
    ),
}


def _incident_webhook_data(
    details: dict[str, Any], status: str, summary: str, reason: str, action: str
) -> dict[str, Any]:
    return {
        "incident_id": details["incident_id"],
        "problem_type": details["problem_type"],
        "incident_status": status,
        "severity": details["severity"],
        "summary": summary,
        "cause_code": details["cause_code"],
        "cause_summary": details["cause_summary"],
        "reason": reason,
        "recommended_action": action,
        "occurrence_count": details["occurrence_count"],
        "first_seen_at_ms": details["opened_at_ms"],
        "last_seen_at_ms": details["last_seen_at_ms"],
        "account_name": details["display_name"],
        "account_email": details["upstream_email"],
        "account_id": details["public_token"],
    }


class ServiceSettings(Protocol):
    @property
    def browser_oauth_mode(self) -> str: ...
    @property
    def callback_ports(self) -> tuple[int, ...]: ...
    @property
    def login_timeout_seconds(self) -> int: ...
    @property
    def codex_version(self) -> str: ...
    @property
    def browser_callback_max_bytes(self) -> int: ...
    @property
    def usage_refresh_concurrency(self) -> int: ...
    @property
    def auth_concurrency(self) -> int: ...
    @property
    def activation_concurrency(self) -> int: ...
    @property
    def usage_poll_seconds(self) -> int: ...
    @property
    def activation_safety_delay_seconds(self) -> int: ...
    @property
    def activation_jitter_max_seconds(self) -> int: ...
    @property
    def estimated_schedule_enabled(self) -> bool: ...


class EventPort(Protocol):
    def publish(self, name: str, data: dict[str, Any]) -> Any: ...


class RuntimePort(Protocol):
    async def start_fresh(
        self,
        account_id: str,
        payload: dict[str, Any] | None = None,
        workspace_constraint: str | None = None,
    ) -> Any: ...
    async def get_existing(self, account_id: str) -> Any | None: ...
    async def discard(self, runtime: Any) -> None: ...
    async def archive(self, runtime: Any) -> None: ...
    async def preserve(self, runtime: Any) -> None: ...
    async def stop(self, account_id: str) -> None: ...


class WebhookPort(Protocol):
    async def emit(
        self, event_type: str, subject: str, data: dict[str, Any], incident_id: str | None = None
    ) -> str: ...


@dataclass(frozen=True, slots=True)
class BrowserContract:
    scheme: str
    host: str
    port: int
    path: str
    state_hash: bytes


def browser_contract(
    authorization_url: str,
    allowed_ports: tuple[int, ...],
    maximum_bytes: int = 16_384,
) -> BrowserContract:
    if len(authorization_url.encode()) > maximum_bytes:
        raise WindowkeeperError(
            "CODEX_BROWSER_AUTH_CONTRACT_CHANGED",
            "Codex returned an oversized browser sign-in contract",
            409,
        )
    try:
        auth = urlsplit(authorization_url)
        query = parse_qs(auth.query, strict_parsing=True)
        redirects, states = query.get("redirect_uri", []), query.get("state", [])
        if (
            auth.scheme != "https"
            or auth.username
            or auth.password
            or auth.fragment
            or len(redirects) != 1
            or len(states) != 1
        ):
            raise ValueError("unsafe authorization contract")
        redirect = urlsplit(redirects[0])
        if (
            redirect.scheme != "http"
            or redirect.hostname not in {"localhost", "127.0.0.1"}
            or redirect.port not in allowed_ports
            or redirect.path != "/auth/callback"
            or redirect.username
            or redirect.password
            or redirect.fragment
        ):
            raise ValueError("unexpected callback contract")
    except ValueError as error:
        raise WindowkeeperError(
            "CODEX_BROWSER_AUTH_CONTRACT_CHANGED",
            "Codex returned an unsupported browser sign-in contract",
            409,
        ) from error
    if not redirect.hostname or not redirect.port:
        raise WindowkeeperError(
            "CODEX_BROWSER_AUTH_CONTRACT_CHANGED",
            "Codex returned an incomplete callback contract",
            409,
        )
    return BrowserContract(
        "http",
        redirect.hostname,
        redirect.port,
        redirect.path,
        hashlib.sha256(states[0].encode()).digest(),
    )


def validate_callback(value: str, contract: BrowserContract, maximum_bytes: int = 16_384) -> str:
    if len(value.encode()) > maximum_bytes:
        raise WindowkeeperError("BROWSER_CALLBACK_INVALID", "The callback URL is too large")
    try:
        callback = urlsplit(value)
        query = parse_qs(callback.query, strict_parsing=True)
        code, state = query.get("code", []), query.get("state", [])
        if (
            callback.scheme != contract.scheme
            or callback.hostname != contract.host
            or callback.port != contract.port
            or callback.path != contract.path
            or callback.username
            or callback.password
            or callback.fragment
            or len(code) != 1
            or len(state) != 1
        ):
            raise ValueError("callback does not match")
    except ValueError as error:
        raise WindowkeeperError(
            "BROWSER_CALLBACK_INVALID", "The callback URL is not valid", 400
        ) from error
    if not secrets.compare_digest(hashlib.sha256(state[0].encode()).digest(), contract.state_hash):
        raise WindowkeeperError(
            "BROWSER_CALLBACK_STATE_MISMATCH", "The callback belongs to another sign-in", 409
        )
    encoded_query = urlencode({"code": code[0], "state": state[0]})
    return f"{contract.scheme}://{contract.host}:{contract.port}{contract.path}?{encoded_query}"


def verify_identity(account: dict[str, Any], identity: dict[str, Any]) -> dict[str, Any]:
    if "account" not in identity or identity["account"] is None:
        raise WindowkeeperError(
            "CODEX_AUTH_REQUIRED",
            "Codex authentication must be renewed",
            409,
        )
    observed = identity["account"]
    if not isinstance(observed, dict) or observed.get("type") != "chatgpt":
        raise WindowkeeperError(
            "AUTH_IDENTITY_UNVERIFIED",
            "Codex did not return a ChatGPT identity",
            409,
        )
    expected_email = account.get("upstream_email")
    observed_email = observed.get("email")
    if (
        expected_email
        and observed_email
        and str(expected_email).casefold() != str(observed_email).casefold()
    ):
        raise WindowkeeperError(
            "AUTH_IDENTITY_MISMATCH",
            "The authenticated ChatGPT identity does not match this account",
            409,
        )
    return observed


def _integer(value: Any) -> int:
    try:
        return int(value)
    except (TypeError, ValueError, OverflowError) as error:
        raise RuntimeError("stored numeric value is invalid") from error


def _find_mapping(value: Any, key: str, expected: str) -> dict[str, Any] | None:
    if isinstance(value, dict):
        if str(value.get(key, "")) == expected:
            return value
        for nested in value.values():
            if found := _find_mapping(nested, key, expected):
                return found
    elif isinstance(value, list):
        for nested in value:
            if found := _find_mapping(nested, key, expected):
                return found
    return None


def _reconciled_result(
    response: dict[str, Any], turn_id: str | None, activation_id: str
) -> tuple[str, bool] | None:
    turn = _find_mapping(response, "id", turn_id) if turn_id else None
    turn = turn or _find_mapping(response, "clientUserMessageId", activation_id)
    if not turn:
        return None
    status = str(turn.get("status") or turn.get("state") or "").lower()
    if status not in {"completed", "failed", "cancelled"}:
        return None
    encoded = json.dumps(turn, separators=(",", ":")).lower()
    unsafe = any(marker in encoded for marker in ('"type":"tool', '"type":"approval'))
    if status != "completed":
        return status.upper(), unsafe

    text: list[str] = []

    def collect(value: Any) -> None:
        if isinstance(value, dict):
            for key, nested in value.items():
                if key in {"text", "delta", "outputText"} and isinstance(nested, str):
                    text.append(nested)
                else:
                    collect(nested)
        elif isinstance(value, list):
            for nested in value:
                collect(nested)

    collect(turn.get("items", turn))
    return "".join(text).strip(), unsafe


@dataclass(slots=True)
class StoredInteraction:
    attempt_id: str
    session_hash: bytes
    nonce_hash: bytes
    interaction: LoginInteraction
    contract: BrowserContract | None = None
    consumed: bool = False


class ApplicationServices:
    """The application seam used by HTTP, CLI, scheduler, and tests."""

    def __init__(
        self,
        database: Database,
        settings: ServiceSettings,
        vault: Vault,
        runtime: RuntimePort,
        events: EventPort,
        webhooks: WebhookPort | None = None,
    ) -> None:
        self.database = database
        self.settings = settings
        self.vault = vault
        self.runtime = runtime
        self.events = events
        self.webhooks = webhooks
        self.clock = SystemClock()
        self.interactions: dict[str, StoredInteraction] = {}
        self._tasks: set[asyncio.Task[Any]] = set()
        self._login_tasks: dict[str, asyncio.Task[Any]] = {}
        self._usage_semaphore = asyncio.Semaphore(settings.usage_refresh_concurrency)
        self._auth_semaphore = asyncio.Semaphore(settings.auth_concurrency)
        self._activation_semaphore = asyncio.Semaphore(settings.activation_concurrency)
        self._browser_login_lock = asyncio.Lock()
        self._credential_locks: dict[str, asyncio.Lock] = {}
        self.log = logging.getLogger("windowkeeper.services")

    def _background(self, coroutine: Any) -> asyncio.Task[Any]:
        task = asyncio.create_task(coroutine)
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)
        return task

    async def reconcile_startup(self) -> None:
        now = self.clock.now_ms()

        def work(
            connection: sqlite3.Connection,
        ) -> tuple[list[dict[str, Any]], list[str]]:
            checkpoint_accounts = [
                str(row[0])
                for row in connection.execute(
                    "SELECT account_id FROM account_state WHERE worker_state IN('CREDENTIAL_IN_USE','CREDENTIAL_QUARANTINED')"
                )
            ]
            connection.execute(
                "UPDATE account_state SET worker_state='CREDENTIAL_QUARANTINED',auth_state='AUTH_REQUIRED',activation_state='UNSCHEDULED',overall_state='ERROR',last_error_code='CREDENTIAL_CHECKPOINT_UNCERTAIN',last_error_summary='Service restarted before credential checkpoint completion',updated_at_ms=?,state_version=state_version+1 WHERE worker_state IN('CREDENTIAL_IN_USE','CREDENTIAL_QUARANTINED')",
                (now,),
            )
            for account_id in checkpoint_accounts:
                connection.execute(
                    "UPDATE activation_attempts SET state='CANCELLED',completed_at_ms=?,updated_at_ms=?,state_version=state_version+1 WHERE account_id=? AND state='PLANNED'",
                    (now, now, account_id),
                )
            uncertain = [
                dict(row)
                for row in connection.execute(
                    "SELECT aa.activation_id,aa.account_id,aa.upstream_thread_id,aa.upstream_turn_id,aa.client_user_message_id,a.public_token,a.workspace_constraint,(SELECT operation_id FROM operations o WHERE o.account_id=aa.account_id AND o.kind='activation.run' ORDER BY o.created_at_ms DESC LIMIT 1) AS operation_id FROM activation_attempts aa JOIN accounts a USING(account_id) WHERE aa.state IN('TURN_DISPATCHING','TURN_ACCEPTED','RUNNING')"
                )
            ]
            connection.execute(
                "UPDATE login_attempts SET state='RESTART_REQUIRED',error_code='LOGIN_RESTART_REQUIRED',updated_at_ms=? WHERE state NOT IN ('COMPLETED','CANCELLED','EXPIRED','FAILED_RETRYABLE','FAILED_ACTION_REQUIRED','RESTART_REQUIRED','SUPERSEDED')",
                (now,),
            )
            connection.execute(
                "UPDATE operations SET state='FAILED',error_code='LOGIN_RESTART_REQUIRED',error_summary='Sign-in must be restarted',completed_at_ms=?,state_version=state_version+1 WHERE kind LIKE 'login.%' AND state NOT IN ('SUCCEEDED','FAILED','CANCELLED')",
                (now,),
            )
            ambiguous_accounts = connection.execute(
                "SELECT DISTINCT account_id FROM activation_attempts WHERE state IN('TURN_DISPATCHING','TURN_ACCEPTED','RUNNING')"
            ).fetchall()
            definite_accounts = connection.execute(
                "SELECT DISTINCT account_id FROM activation_attempts WHERE state IN('QUEUED','THREAD_CREATED')"
            ).fetchall()
            connection.execute(
                "UPDATE activation_attempts SET state='AMBIGUOUS',ambiguity_reason='Service restarted after dispatch began',updated_at_ms=?,completed_at_ms=?,state_version=state_version+1 WHERE state IN('TURN_DISPATCHING','TURN_ACCEPTED','RUNNING')",
                (now, now),
            )
            connection.execute(
                "UPDATE activation_attempts SET state='FAILED_DEFINITE',ambiguity_reason='Service restarted before dispatch began',updated_at_ms=?,completed_at_ms=?,state_version=state_version+1 WHERE state IN('QUEUED','THREAD_CREATED')",
                (now, now),
            )
            connection.execute(
                "UPDATE activation_operations SET state='AMBIGUOUS',error_code='ACTIVATION_AMBIGUOUS',error_summary='Service restarted after dispatch began',completed_at_ms=?,updated_at_ms=? WHERE activation_id IN (SELECT activation_id FROM activation_attempts WHERE state='AMBIGUOUS') AND state IN('STARTED','REQUEST_WRITING','AWAITING_RESPONSE','RECONCILING')",
                (now, now),
            )
            connection.execute(
                "UPDATE activation_operations SET state='FAILED',error_code='SERVICE_RESTARTED',error_summary='Service restarted before dispatch began',completed_at_ms=?,updated_at_ms=? WHERE state IN('STARTED','REQUEST_WRITING','AWAITING_RESPONSE','RECONCILING')",
                (now, now),
            )
            for row in ambiguous_accounts:
                connection.execute(
                    "UPDATE account_state SET activation_state='AMBIGUOUS',overall_state='WARNING',last_error_code='ACTIVATION_AMBIGUOUS',last_error_summary='Activation dispatch was interrupted by restart',updated_at_ms=?,state_version=state_version+1 WHERE account_id=?",
                    (now, row[0]),
                )
            for row in definite_accounts:
                connection.execute(
                    "UPDATE account_state SET activation_state='UNSCHEDULED',overall_state='WARNING',last_error_code='SERVICE_RESTARTED',last_error_summary='Activation ended before dispatch',updated_at_ms=?,state_version=state_version+1 WHERE account_id=?",
                    (now, row[0]),
                )
            connection.execute(
                "UPDATE operations SET state='AMBIGUOUS',error_code='ACTIVATION_AMBIGUOUS',error_summary='Activation dispatch was interrupted by restart',completed_at_ms=?,state_version=state_version+1 WHERE kind='activation.run' AND state='RUNNING'",
                (now,),
            )
            connection.execute(
                "UPDATE operations SET state='FAILED',error_code='SERVICE_RESTARTED',error_summary='Operation was interrupted by service restart',completed_at_ms=?,state_version=state_version+1 WHERE state IN('QUEUED','RUNNING','WAITING_FOR_USER')",
                (now,),
            )
            connection.execute(
                "UPDATE webhook_deliveries SET state='RETRY_SCHEDULED',lease_token=NULL,lease_expires_at_ms=NULL,next_attempt_at_ms=? WHERE state='LEASED'",
                (now,),
            )
            return uncertain, checkpoint_accounts

        attempts, checkpoint_accounts = await self.database.transaction(work)
        reconciled = await asyncio.gather(
            *(self._reconcile_activation(attempt) for attempt in attempts)
        )
        for account_id in checkpoint_accounts:
            await self.open_incident(
                account_id,
                "credential_checkpoint",
                "ERROR",
                "Service restarted before credential checkpoint completion",
            )
        for attempt, succeeded in zip(attempts, reconciled, strict=True):
            if not succeeded:
                await self.open_incident(
                    str(attempt["account_id"]),
                    "activation_ambiguous",
                    "ERROR",
                    "Activation dispatch was interrupted by service restart",
                )

    async def _reconcile_activation(self, attempt: dict[str, Any]) -> bool:
        thread_id = attempt.get("upstream_thread_id")
        operation_id = attempt.get("operation_id")
        if not thread_id or not operation_id:
            return False
        account = {
            "account_id": str(attempt["account_id"]),
            "public_token": str(attempt["public_token"]),
            "workspace_constraint": attempt.get("workspace_constraint"),
        }
        try:
            evidence = await self._run_managed(
                account, lambda runtime: runtime.adapter.read_thread(str(thread_id))
            )
            reconciled = _reconciled_result(
                evidence,
                str(attempt["upstream_turn_id"]) if attempt.get("upstream_turn_id") else None,
                str(attempt["client_user_message_id"]),
            )
        except Exception as error:
            self.log.warning(
                "activation reconciliation failed",
                extra={
                    "event": "activation.reconciliation_failed",
                    "account_id": account["account_id"],
                    "error_code": type(error).__name__,
                },
            )
            return False
        if not reconciled:
            return False
        result, unsafe = reconciled
        if unsafe:
            await self._ambiguous_activation(
                account,
                str(attempt["activation_id"]),
                str(operation_id),
                "Reconciliation found a tool or approval item",
                safety_blocked=True,
            )
            return True
        if result in {"FAILED", "CANCELLED"}:
            now = self.clock.now_ms()

            def failed(connection: sqlite3.Connection) -> None:
                connection.execute(
                    "UPDATE activation_attempts SET state='FAILED_DEFINITE',terminal_status=?,ambiguity_reason=NULL,completed_at_ms=?,updated_at_ms=?,state_version=state_version+1 WHERE activation_id=?",
                    (result, now, now, attempt["activation_id"]),
                )
                connection.execute(
                    "UPDATE activation_operations SET state='FAILED',error_code='UPSTREAM_TURN_TERMINAL',error_summary='Reconciliation found a definite terminal turn',completed_at_ms=?,updated_at_ms=? WHERE activation_id=?",
                    (now, now, attempt["activation_id"]),
                )
                connection.execute(
                    "UPDATE operations SET state='FAILED',error_code='UPSTREAM_TURN_TERMINAL',error_summary='Activation ended without completing',completed_at_ms=?,state_version=state_version+1 WHERE operation_id=?",
                    (now, operation_id),
                )
                connection.execute(
                    "UPDATE account_state SET activation_state='UNSCHEDULED',overall_state='WARNING',last_error_code='UPSTREAM_TURN_TERMINAL',last_error_summary='Activation ended without completing',updated_at_ms=?,state_version=state_version+1 WHERE account_id=?",
                    (now, account["account_id"]),
                )

            await self.database.transaction(failed)
            return True
        await self._complete_activation(
            account,
            str(attempt["activation_id"]),
            str(operation_id),
            result,
        )
        return True

    def start_background(self) -> None:
        self._background(self._scheduler_loop())
        self._background(self._usage_loop())
        self._background(self._maintenance_loop())

    async def _maintenance_loop(self) -> None:
        while True:
            try:
                now = self.clock.now_ms()
                cutoff = now - 30 * 24 * 60 * 60 * 1000
                incident_cutoff = now - 90 * 24 * 60 * 60 * 1000

                def prune(
                    connection: sqlite3.Connection,
                    now_ms: int = now,
                    retention_cutoff: int = cutoff,
                    resolved_incident_cutoff: int = incident_cutoff,
                ) -> None:
                    connection.execute(
                        "DELETE FROM admin_sessions WHERE rowid IN (SELECT rowid FROM admin_sessions WHERE absolute_expires_at_ms<? ORDER BY absolute_expires_at_ms LIMIT 250)",
                        (now_ms,),
                    )
                    connection.execute(
                        "DELETE FROM usage_snapshots WHERE snapshot_id IN (SELECT snapshot_id FROM usage_snapshots WHERE attempted_at_ms<? ORDER BY attempted_at_ms LIMIT 250)",
                        (retention_cutoff,),
                    )
                    connection.execute(
                        "DELETE FROM webhook_events WHERE event_id IN (SELECT event_id FROM webhook_events WHERE created_at_ms<? ORDER BY created_at_ms LIMIT 250)",
                        (retention_cutoff,),
                    )
                    connection.execute(
                        "DELETE FROM operations WHERE operation_id IN (SELECT operation_id FROM operations WHERE completed_at_ms<? ORDER BY completed_at_ms LIMIT 250)",
                        (retention_cutoff,),
                    )
                    connection.execute(
                        "DELETE FROM credential_bundles WHERE bundle_id IN (SELECT bundle_id FROM credential_bundles WHERE state='RETIRED' AND COALESCE(retired_at_ms,created_at_ms)<? ORDER BY COALESCE(retired_at_ms,created_at_ms) LIMIT 250)",
                        (retention_cutoff,),
                    )
                    connection.execute(
                        "DELETE FROM activation_attempts WHERE activation_id IN (SELECT activation_id FROM activation_attempts WHERE state IN('COMPLETED_OK','COMPLETED_WARNING','FAILED_DEFINITE','CANCELLED','AMBIGUOUS_CLOSED') AND updated_at_ms<? ORDER BY updated_at_ms LIMIT 250)",
                        (resolved_incident_cutoff,),
                    )
                    connection.execute(
                        "DELETE FROM incidents WHERE incident_id IN (SELECT incident_id FROM incidents WHERE state='RESOLVED' AND resolved_at_ms<? ORDER BY resolved_at_ms LIMIT 250)",
                        (resolved_incident_cutoff,),
                    )
                    connection.execute("PRAGMA incremental_vacuum(64)")

                await self.database.transaction(prune)
                await self.clock.sleep(3600)
            except asyncio.CancelledError as cancellation:
                del cancellation
                return
            except Exception as error:
                self.log.warning(
                    "maintenance pass failed",
                    extra={
                        "event": "maintenance.failed",
                        "error_code": type(error).__name__,
                    },
                )
                await self.clock.sleep(300)

    async def _scheduler_loop(self) -> None:
        while True:
            try:
                await self._run_due_activations()
                await asyncio.sleep(5)
            except asyncio.CancelledError as cancellation:
                del cancellation
                return
            except Exception as error:
                self.log.error(
                    "scheduler tick failed: %s",
                    type(error).__name__,
                    extra={"event": "scheduler.tick_failed"},
                )
                await asyncio.sleep(10)

    async def _run_due_activations(self) -> None:
        now = self.clock.now_ms()

        def claim(connection: sqlite3.Connection) -> list[tuple[dict[str, Any], str, str]]:
            rows = connection.execute(
                """SELECT aa.activation_id,a.account_id,a.public_token,a.display_name,a.enabled,s.auth_state
                FROM activation_attempts aa JOIN accounts a USING(account_id) JOIN account_state s USING(account_id)
                JOIN usage_current u USING(account_id)
                WHERE aa.state='PLANNED' AND aa.scheduled_for_ms<=? AND a.enabled=1
                AND a.deleted_at_ms IS NULL AND s.auth_state='VERIFIED'
                AND COALESCE(u.short_used_percent_raw,0)<100
                AND COALESCE(u.weekly_used_percent_raw,0)<100
                AND s.activation_state NOT IN('AMBIGUOUS','SAFETY_BLOCKED')
                AND aa.activation_id=(SELECT candidate.activation_id FROM activation_attempts candidate
                    WHERE candidate.account_id=aa.account_id AND candidate.state='PLANNED'
                    AND candidate.scheduled_for_ms<=? ORDER BY candidate.scheduled_for_ms,candidate.created_at_ms LIMIT 1)
                AND NOT EXISTS(SELECT 1 FROM activation_attempts active WHERE active.account_id=aa.account_id
                    AND active.state IN('QUEUED','THREAD_CREATED','TURN_DISPATCHING','TURN_ACCEPTED','RUNNING','AMBIGUOUS','SAFETY_BLOCKED'))
                ORDER BY aa.scheduled_for_ms LIMIT ?""",
                (now, now, self.settings.activation_concurrency),
            ).fetchall()
            claimed: list[tuple[dict[str, Any], str, str]] = []
            for row in rows:
                changed = connection.execute(
                    "UPDATE activation_attempts SET state='QUEUED',updated_at_ms=?,state_version=state_version+1 WHERE activation_id=? AND state='PLANNED'",
                    (now, row["activation_id"]),
                ).rowcount
                if not changed:
                    continue
                connection.execute(
                    "UPDATE account_state SET activation_state='ACTIVATING',updated_at_ms=?,state_version=state_version+1 WHERE account_id=?",
                    (now, row["account_id"]),
                )
                operation_id = new_id()
                connection.execute(
                    "INSERT INTO operations VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                    (
                        operation_id,
                        row["account_id"],
                        "activation.run",
                        "SCHEDULED",
                        "QUEUED",
                        None,
                        None,
                        None,
                        None,
                        None,
                        now,
                        None,
                        None,
                        None,
                        None,
                        1,
                    ),
                )
                claimed.append((dict(row), str(row["activation_id"]), operation_id))
            return claimed

        for account, activation_id, operation_id in await self.database.transaction(claim):
            self._background(self._run_activation(account, activation_id, operation_id))

    async def _usage_loop(self) -> None:
        while True:
            try:
                await asyncio.sleep(self.settings.usage_poll_seconds + secrets.randbelow(31))

                def read(connection: sqlite3.Connection) -> list[str]:
                    return [
                        str(row[0])
                        for row in connection.execute(
                            "SELECT public_token FROM accounts a JOIN account_state s USING(account_id) WHERE a.enabled=1 AND a.deleted_at_ms IS NULL AND s.auth_state='VERIFIED'"
                        )
                    ]

                for account_token in await self.database.call(read):
                    await self.refresh(account_token, "SCHEDULED")
            except asyncio.CancelledError as cancellation:
                del cancellation
                return
            except Exception as error:
                self.log.error(
                    "usage polling failed: %s",
                    type(error).__name__,
                    extra={"event": "usage.poll_failed"},
                )

    async def close(self) -> None:
        for task in self._tasks:
            task.cancel()
        await asyncio.gather(*self._tasks, return_exceptions=True)
        self._tasks.clear()

    async def create_account(
        self,
        display_name: str,
        *,
        labels: list[str] | None = None,
        workspace: str | None = None,
        method: LoginMethod = LoginMethod.CHATGPT_DEVICE_CODE,
    ) -> dict[str, Any]:
        name = " ".join(display_name.split())
        if not name or len(name) > 80:
            raise WindowkeeperError(
                "ACCOUNT_NAME_INVALID", "Enter an account name of 1-80 characters"
            )
        account_id = new_id()
        token = public_token()
        now = self.clock.now_ms()
        clean_labels = sorted({" ".join(value.split()) for value in labels or [] if value.strip()})
        if len(clean_labels) > 20 or any(len(label) > 40 for label in clean_labels):
            raise WindowkeeperError(
                "ACCOUNT_LABELS_INVALID", "Use at most 20 labels of 1-40 characters"
            )

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "INSERT INTO accounts VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    account_id,
                    token,
                    name,
                    "chatgpt",
                    method.value,
                    None,
                    workspace,
                    0,
                    "ENROLLING",
                    now,
                    now,
                    None,
                ),
            )
            connection.execute(
                "INSERT INTO account_state VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    account_id,
                    "ENROLLING",
                    "STOPPED",
                    "STARTING",
                    "UNKNOWN",
                    "UNSCHEDULED",
                    None,
                    None,
                    None,
                    None,
                    None,
                    None,
                    None,
                    1,
                    now,
                ),
            )
            connection.execute(
                "INSERT INTO usage_current(account_id,last_attempt_at_ms) VALUES(?,?)",
                (account_id, now),
            )
            for label in clean_labels:
                row = connection.execute(
                    "SELECT label_id FROM labels WHERE lower(name)=lower(?)", (label,)
                ).fetchone()
                label_id = str(row[0]) if row else new_id()
                if not row:
                    connection.execute("INSERT INTO labels VALUES(?,?,?)", (label_id, label, now))
                connection.execute("INSERT INTO account_labels VALUES(?,?)", (account_id, label_id))

        try:
            await self.database.transaction(work)
        except sqlite3.IntegrityError as error:
            raise Conflict(
                "ACCOUNT_NAME_EXISTS", "An active account already uses that name"
            ) from error
        self.events.publish("account.updated", {"resource_id": token, "state": "STARTING"})
        return {"account_id": account_id, "public_token": token, "display_name": name}

    async def _account_row(self, public: str) -> dict[str, Any]:
        def work(connection: sqlite3.Connection) -> dict[str, Any] | None:
            row = connection.execute(
                "SELECT a.*,s.* FROM accounts a JOIN account_state s USING(account_id) WHERE a.public_token=? AND a.deleted_at_ms IS NULL",
                (public,),
            ).fetchone()
            return dict(row) if row else None

        row = await self.database.call(work)
        if not row:
            raise WindowkeeperError("ACCOUNT_NOT_FOUND", "Account not found", 404)
        return row

    async def accounts(self) -> list[AccountSummary]:
        def work(connection: sqlite3.Connection) -> list[dict[str, Any]]:
            return [
                dict(row)
                for row in connection.execute(
                    """SELECT a.account_id,a.public_token,a.display_name,a.enabled,s.overall_state,s.auth_state,
                    s.usage_state,s.activation_state,u.short_used_percent_raw,u.short_resets_at_s,
                    u.weekly_used_percent_raw,u.weekly_resets_at_s,u.complete_read_at_ms,u.last_error_summary,
                    (SELECT group_concat(l.name, ', ') FROM account_labels al JOIN labels l USING(label_id) WHERE al.account_id=a.account_id) labels,
                    (SELECT schedule_confidence FROM activation_attempts aa WHERE aa.account_id=a.account_id ORDER BY created_at_ms DESC LIMIT 1) confidence,
                    (SELECT scheduled_for_ms FROM activation_attempts aa WHERE aa.account_id=a.account_id AND state='PLANNED' ORDER BY created_at_ms DESC LIMIT 1) next_activation,
                    (SELECT kind FROM operations o WHERE o.account_id=a.account_id AND o.state NOT IN('SUCCEEDED','FAILED','CANCELLED') ORDER BY created_at_ms DESC LIMIT 1) active_operation
                    FROM accounts a JOIN account_state s USING(account_id) LEFT JOIN usage_current u USING(account_id)
                    WHERE a.deleted_at_ms IS NULL ORDER BY lower(a.display_name)"""
                ).fetchall()
            ]

        rows = await self.database.call(work)
        result: list[AccountSummary] = []
        for row in rows:
            labels = str(row.get("labels") or "")
            result.append(
                AccountSummary(
                    account_id=row["account_id"],
                    public_token=row["public_token"],
                    display_name=row["display_name"],
                    labels=[item.strip() for item in labels.split(",") if item.strip()],
                    enabled=bool(row["enabled"]),
                    overall_state=row["overall_state"],
                    auth_state=row["auth_state"],
                    usage_state=row["usage_state"],
                    activation_state=row["activation_state"],
                    short_percent=row.get("short_used_percent_raw"),
                    short_reset_ms=(
                        row["short_resets_at_s"] * 1000 if row.get("short_resets_at_s") else None
                    ),
                    weekly_percent=row.get("weekly_used_percent_raw"),
                    weekly_reset_ms=(
                        row["weekly_resets_at_s"] * 1000 if row.get("weekly_resets_at_s") else None
                    ),
                    schedule_confidence=row.get("confidence") or "UNKNOWN",
                    next_activation_ms=row.get("next_activation"),
                    last_refresh_ms=row.get("complete_read_at_ms"),
                    active_operation=row.get("active_operation"),
                    evidence=row.get("last_error_summary")
                    or (
                        "Complete rate-limit evidence available"
                        if row.get("complete_read_at_ms")
                        else "No complete usage read yet"
                    ),
                )
            )
        return result

    async def account_detail(self, public: str) -> dict[str, Any]:
        account = await self._account_row(public)

        def work(connection: sqlite3.Connection) -> dict[str, Any]:
            usage = connection.execute(
                "SELECT * FROM usage_current WHERE account_id=?", (account["account_id"],)
            ).fetchone()
            operations = connection.execute(
                "SELECT * FROM operations WHERE account_id=? ORDER BY created_at_ms DESC LIMIT 20",
                (account["account_id"],),
            ).fetchall()
            activations = connection.execute(
                "SELECT * FROM activation_attempts WHERE account_id=? ORDER BY created_at_ms DESC LIMIT 20",
                (account["account_id"],),
            ).fetchall()
            incidents = connection.execute(
                "SELECT * FROM incidents WHERE scope_key=? ORDER BY opened_at_ms DESC LIMIT 20",
                (account["account_id"],),
            ).fetchall()
            auth_export = connection.execute(
                "SELECT created_at_ms FROM credential_bundles WHERE account_id=? AND state='EXPORT'",
                (account["account_id"],),
            ).fetchone()
            labels = [
                str(row[0])
                for row in connection.execute(
                    "SELECT l.name FROM labels l JOIN account_labels al USING(label_id) WHERE al.account_id=? ORDER BY lower(l.name)",
                    (account["account_id"],),
                )
            ]
            return {
                "labels": labels,
                "usage": dict(usage) if usage else {},
                "operations": [dict(row) for row in operations],
                "activations": [dict(row) for row in activations],
                "incidents": [dict(row) for row in incidents],
                "auth_export": {
                    "available": auth_export is not None,
                    "created_at_ms": auth_export["created_at_ms"] if auth_export else None,
                },
            }

        detail = await self.database.call(work)
        account["labels"] = detail.pop("labels")
        return {"account": account, **detail}

    async def _create_operation(
        self, account_id: str | None, kind: str, trigger: str = "USER"
    ) -> str:
        operation_id = new_id()
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "INSERT INTO operations VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    operation_id,
                    account_id,
                    kind,
                    trigger,
                    "QUEUED",
                    None,
                    None,
                    None,
                    None,
                    None,
                    now,
                    None,
                    None,
                    None,
                    None,
                    1,
                ),
            )

        await self.database.transaction(work)
        return operation_id

    async def start_login(
        self,
        public: str,
        method: LoginMethod,
        session_token: str,
        *,
        recover_checkpoint: bool = False,
    ) -> dict[str, str]:
        account = await self._account_row(public)
        if account["worker_state"] == "CREDENTIAL_QUARANTINED" and not recover_checkpoint:
            raise Conflict(
                "CREDENTIAL_RUNTIME_BLOCKED",
                "Explicit reauthentication is required to recover this credential",
            )
        if method == LoginMethod.MANUAL_TOKENS:
            raise Conflict(
                "LOGIN_METHOD_UNAVAILABLE",
                "Manual token import is retired; use device code or browser sign-in",
            )
        if method == LoginMethod.CHATGPT_BROWSER and self.settings.browser_oauth_mode == "disabled":
            raise Conflict(
                "LOGIN_METHOD_UNAVAILABLE", "Browser sign-in is disabled for this deployment"
            )
        operation_id = await self._create_operation(
            account["account_id"], f"login.{method.value.lower()}"
        )
        attempt_id = new_id()
        nonce = secrets.token_urlsafe(32)
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> None:
            if recover_checkpoint:
                connection.execute(
                    "UPDATE account_state SET worker_state='STOPPED',updated_at_ms=?,state_version=state_version+1 WHERE account_id=? AND auth_state='AUTH_REQUIRED' AND worker_state='CREDENTIAL_QUARANTINED'",
                    (now, account["account_id"]),
                )
            connection.execute(
                "INSERT INTO login_attempts VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    attempt_id,
                    account["account_id"],
                    operation_id,
                    method.value,
                    "CREATED",
                    None,
                    digest(session_token),
                    digest(nonce),
                    None,
                    None,
                    now,
                    None,
                    now + self.settings.login_timeout_seconds * 1000,
                    None,
                    None,
                    account.get("workspace_constraint"),
                    None,
                    None,
                    None,
                    None,
                    now,
                    now,
                ),
            )

        try:
            await self.database.transaction(work)
        except sqlite3.IntegrityError as error:
            active = await self.database.call(
                lambda connection: connection.execute(
                    "SELECT 1 FROM login_attempts WHERE account_id=? AND state IN('CREATED','STARTING_RUNTIME','STARTING_LOGIN','WAITING_FOR_USER','OAUTH_COMPLETED','VERIFYING_ACCOUNT','STARTING_EXPORT_LOGIN','WAITING_FOR_EXPORT_USER','VERIFYING_EXPORT','FORKING_CREDENTIALS','QUIESCING_RUNTIME','CHECKPOINTING_CREDENTIAL','CANCEL_REQUESTED')",
                    (account["account_id"],),
                ).fetchone()
            )
            code = "LOGIN_ALREADY_ACTIVE" if active else "LOGIN_STORAGE_CONSTRAINT"
            summary = (
                "Another sign-in is active" if active else "Sign-in storage rejected the request"
            )
            await self._fail_operation(operation_id, code, summary)
            if active:
                raise Conflict(
                    code, "Another sign-in is already active for this account"
                ) from error
            raise WindowkeeperError(
                code, "Sign-in could not be recorded; apply database migrations and retry", 500
            ) from error
        task = self._background(
            self._run_login(
                account,
                operation_id,
                attempt_id,
                method,
                session_token,
                nonce,
            )
        )
        self._login_tasks[attempt_id] = task
        task.add_done_callback(lambda _task: self._login_tasks.pop(attempt_id, None))
        return {
            "operation_id": operation_id,
            "login_attempt_id": attempt_id,
            "interaction_nonce": nonce,
        }

    async def _capture_login(
        self,
        account: dict[str, Any],
        operation_id: str,
        attempt_id: str,
        method: LoginMethod,
        session_token: str,
        nonce: str,
    ) -> tuple[Any, dict[str, Any], dict[str, Any]]:
        runtime = await self.runtime.start_fresh(
            account["account_id"], workspace_constraint=account.get("workspace_constraint")
        )
        authenticated = False
        try:
            async with runtime.lock:
                await self._login_state(attempt_id, "STARTING_LOGIN")
                interaction = await runtime.adapter.start_login(method)
            contract = (
                browser_contract(
                    interaction.auth_url.reveal(),
                    self.settings.callback_ports,
                    self.settings.browser_callback_max_bytes,
                )
                if interaction.auth_url
                else None
            )
            callback_mode = (
                "AUTOMATIC_LOOPBACK"
                if contract and self.settings.browser_oauth_mode == "host-loopback"
                else ("MANUAL_FORWARD" if contract else None)
            )
            self.interactions[attempt_id] = StoredInteraction(
                attempt_id, digest(session_token), digest(nonce), interaction, contract
            )
            now = self.clock.now_ms()

            def ready(connection: sqlite3.Connection) -> None:
                changed = connection.execute(
                    "UPDATE login_attempts SET state='WAITING_FOR_USER',upstream_login_id=?,callback_port=?,callback_mode=?,started_at_ms=?,updated_at_ms=? WHERE login_attempt_id=? AND state='STARTING_LOGIN'",
                    (
                        interaction.login_id,
                        contract.port if contract else None,
                        callback_mode,
                        now,
                        now,
                        attempt_id,
                    ),
                ).rowcount
                if not changed:
                    raise Conflict("LOGIN_CANCELLED", "Sign-in was cancelled before interaction")
                connection.execute(
                    "UPDATE operations SET state='WAITING_FOR_USER',progress_code='WAITING_FOR_USER',progress_summary='Complete ChatGPT sign-in',state_version=state_version+1 WHERE operation_id=?",
                    (operation_id,),
                )

            await self.database.transaction(ready)
            self.events.publish(
                "login.updated",
                {
                    "attempt_id": attempt_id,
                    "account_id": account["public_token"],
                    "state": "WAITING_FOR_USER",
                    "interaction_ready": True,
                },
            )
            await self._await_login_completion(
                runtime.adapter.client.notifications(), interaction.login_id
            )
            authenticated = True
            self.interactions.pop(attempt_id, None)
            if not await self._login_state(
                attempt_id, "VERIFYING_ACCOUNT", expected="WAITING_FOR_USER"
            ):
                raise Conflict("LOGIN_CANCELLED", "Sign-in was cancelled before verification")
            async with runtime.lock:
                identity = await runtime.adapter.account()
                verify_identity(account, identity)
                await runtime.client.close()
                payload = self.vault.capture(
                    runtime.codex_home,
                    self.settings.codex_version,
                    account.get("workspace_constraint"),
                )
            return runtime, identity, payload
        except BaseException:
            self.interactions.pop(attempt_id, None)
            await runtime.client.close()
            if authenticated:
                await self.runtime.preserve(runtime)
            else:
                await self.runtime.discard(runtime)
            raise

    async def _run_login(
        self,
        account: dict[str, Any],
        operation_id: str,
        attempt_id: str,
        method: LoginMethod,
        session_token: str,
        nonce: str,
    ) -> None:
        lock = self._browser_login_lock if method == LoginMethod.CHATGPT_BROWSER else asyncio.Lock()
        try:
            async with self._auth_semaphore, lock, self._credential_lock(account["account_id"]):
                await self._operation_state(
                    operation_id, "RUNNING", "STARTING_RUNTIME", "Starting isolated Codex runtime"
                )
                await self._login_state(attempt_id, "STARTING_RUNTIME")
                source_runtime, source_identity, source = await self._capture_login(
                    account, operation_id, attempt_id, method, session_token, nonce
                )
                promotion_task = asyncio.create_task(
                    self._promote_login_source(account["account_id"], attempt_id, source)
                )
                try:
                    cancellation = await self._await_critical(promotion_task)
                    promoted = promotion_task.result()
                except BaseException:
                    preserve_task = asyncio.create_task(self.runtime.preserve(source_runtime))
                    await self._await_critical(preserve_task)
                    raise
                if not promoted:
                    preserve_task = asyncio.create_task(self.runtime.preserve(source_runtime))
                    await self._await_critical(preserve_task)
                    raise Conflict("LOGIN_CANCELLED", "Sign-in was cancelled before checkpointing")
                discard_task = asyncio.create_task(self.runtime.discard(source_runtime))
                discard_cancellation = await self._await_critical(discard_task)
                cancellation = cancellation or discard_cancellation
                if cancellation:
                    raise cancellation
                identity, _, export_error = await self._fork_credentials(
                    account, source, source_identity
                )
                export_available = bool(await self._bundle_payload(account["account_id"], "EXPORT"))
                await self._commit_login(
                    account,
                    operation_id,
                    attempt_id,
                    method,
                    identity,
                    export_available,
                    export_error,
                )
            await self.refresh(account["public_token"], "LOGIN")
        except asyncio.CancelledError:
            state = await self.database.call(
                lambda connection: connection.execute(
                    "SELECT state FROM login_attempts WHERE login_attempt_id=?", (attempt_id,)
                ).fetchone()
            )
            if not state or state[0] != "CANCELLED":
                await self._fail_login(
                    attempt_id,
                    operation_id,
                    "RESTART_REQUIRED",
                    "LOGIN_RESTART_REQUIRED",
                    "Sign-in was interrupted",
                )
            raise
        except WindowkeeperError as error:
            self.log.warning("login rejected", extra={"event": "login.rejected"})
            action_required = error.code in {
                "AUTH_IDENTITY_UNVERIFIED",
                "AUTH_IDENTITY_MISMATCH",
                "CODEX_BROWSER_AUTH_CONTRACT_CHANGED",
            }
            await self._fail_login(
                attempt_id,
                operation_id,
                "FAILED_ACTION_REQUIRED" if action_required else "FAILED_RETRYABLE",
                error.code,
                error.detail,
            )
        except Exception as error:
            self.log.warning("login failed", extra={"event": "login.failed"})
            await self._fail_login(
                attempt_id,
                operation_id,
                "FAILED_RETRYABLE",
                "LOGIN_FAILED",
                str(error)[:200],
            )

    async def _await_login_completion(self, notifications: Any, login_id: str) -> None:
        async with asyncio.timeout(self.settings.login_timeout_seconds):
            async for event in notifications:
                if event.get("method") != "account/login/completed":
                    continue
                params = event.get("params") or {}
                if params.get("loginId") != login_id:
                    raise WindowkeeperError(
                        "ACCOUNT_ISOLATION_VIOLATION",
                        "Codex routed a sign-in event to the wrong account",
                    )
                if not params.get("success"):
                    raise WindowkeeperError("LOGIN_DENIED", "ChatGPT sign-in was not approved")
                return
        raise TimeoutError("sign-in expired")

    async def interaction(self, attempt_id: str, session_token: str, nonce: str) -> dict[str, Any]:
        stored = self.interactions.get(attempt_id)
        if (
            not stored
            or stored.consumed
            or not secrets.compare_digest(stored.session_hash, digest(session_token))
            or not secrets.compare_digest(stored.nonce_hash, digest(nonce))
        ):
            raise WindowkeeperError(
                "LOGIN_INTERACTION_NOT_READY", "The sign-in interaction is unavailable", 404
            )
        interaction = stored.interaction
        return {
            "attempt_id": attempt_id,
            "method": interaction.method.value,
            "authorization_url": interaction.auth_url.reveal() if interaction.auth_url else None,
            "verification_url": interaction.verification_url.reveal()
            if interaction.verification_url
            else None,
            "user_code": interaction.user_code.reveal() if interaction.user_code else None,
            "callback_mode": (
                "AUTOMATIC_LOOPBACK"
                if stored.contract and self.settings.browser_oauth_mode == "host-loopback"
                else ("MANUAL_FORWARD" if stored.contract else None)
            ),
            "expires_at_ms": interaction.expires_at_ms,
        }

    async def forward_callback(
        self, attempt_id: str, session_token: str, nonce: str, callback_url: str
    ) -> None:
        stored = self.interactions.get(attempt_id)
        if (
            not stored
            or stored.consumed
            or not stored.contract
            or not secrets.compare_digest(stored.session_hash, digest(session_token))
            or not secrets.compare_digest(stored.nonce_hash, digest(nonce))
        ):
            raise Conflict("LOGIN_INTERACTION_ALREADY_CONSUMED", "This callback cannot be used")
        destination = validate_callback(
            callback_url, stored.contract, self.settings.browser_callback_max_bytes
        )
        stored.consumed = True
        try:
            async with httpx.AsyncClient(
                follow_redirects=False, trust_env=False, timeout=httpx.Timeout(5, connect=2)
            ) as client:
                response = await client.get(destination, headers={"User-Agent": "windowkeeper/0.1"})
                if response.status_code >= 400:
                    raise WindowkeeperError(
                        "BROWSER_CALLBACK_FORWARD_FAILED", "Codex did not accept the callback", 502
                    )
        finally:
            destination = "[REDACTED]"
            callback_url = "[REDACTED]"

    async def cancel_login(self, attempt_id: str, session_token: str) -> str:
        stored = self.interactions.get(attempt_id)
        if not stored or not secrets.compare_digest(stored.session_hash, digest(session_token)):
            raise WindowkeeperError(
                "LOGIN_INTERACTION_SESSION_MISMATCH", "This sign-in belongs to another session", 403
            )

        def request_cancel(connection: sqlite3.Connection) -> dict[str, Any] | None:
            row = connection.execute(
                "SELECT * FROM login_attempts WHERE login_attempt_id=?", (attempt_id,)
            ).fetchone()
            if not row:
                return None
            changed = connection.execute(
                "UPDATE login_attempts SET state='CANCEL_REQUESTED',updated_at_ms=? WHERE login_attempt_id=? AND state IN('CREATED','STARTING_RUNTIME','STARTING_LOGIN','WAITING_FOR_USER','OAUTH_COMPLETED','VERIFYING_ACCOUNT')",
                (self.clock.now_ms(), attempt_id),
            ).rowcount
            return dict(row) if changed else None

        attempt = await self.database.transaction(request_cancel)
        if not attempt:
            raise Conflict("LOGIN_NOT_CANCELLABLE", "Sign-in is no longer cancellable")
        operation_id = await self._create_operation(attempt["account_id"], "login.cancel")
        self.interactions.pop(attempt_id, None)
        self._background(self._cancel_login_runtime(attempt, operation_id))
        return operation_id

    async def _cancel_login_runtime(self, attempt: dict[str, Any], operation_id: str) -> None:
        try:
            runtime = await self.runtime.get_existing(attempt["account_id"])
            if runtime is None:
                raise Conflict("LOGIN_RUNTIME_GONE", "The sign-in runtime is no longer available")
            async with runtime.lock:
                await runtime.adapter.cancel_login(attempt["upstream_login_id"])
            await self._login_state(attempt["login_attempt_id"], "CANCELLED")
            await self._operation_state(
                attempt["operation_id"], "CANCELLED", "CANCELLED", "Sign-in cancelled"
            )
            await self._operation_state(operation_id, "SUCCEEDED", "CANCELLED", "Sign-in cancelled")
            if task := self._login_tasks.get(str(attempt["login_attempt_id"])):
                task.cancel()
        except Exception as error:
            await self._login_state(attempt["login_attempt_id"], "FAILED_RETRYABLE")
            await self._fail_operation(operation_id, "LOGIN_CANCEL_FAILED", str(error)[:200])

    async def _bundle_payload(self, account_id: str, state: str) -> dict[str, Any] | None:
        def work(connection: sqlite3.Connection) -> dict[str, Any] | None:
            row = connection.execute(
                "SELECT * FROM credential_bundles WHERE account_id=? AND state=?",
                (account_id, state),
            ).fetchone()
            return dict(row) if row else None

        row = await self.database.call(work)
        if not row:
            return None
        return self.vault.decrypt(
            Envelope(
                row["bundle_id"],
                row["account_id"],
                row["key_id"],
                row["nonce"],
                row["ciphertext"],
                row["aad"],
                row["payload_schema_version"],
                row["envelope_version"],
            )
        )

    async def _credential_payload(self, account_id: str) -> dict[str, Any]:
        payload = await self._bundle_payload(account_id, "ACTIVE")
        if not payload:
            raise Conflict("AUTH_REQUIRED", "The account must be authenticated first")
        return payload

    def _credential_lock(self, account_id: str) -> asyncio.Lock:
        return self._credential_locks.setdefault(account_id, asyncio.Lock())

    async def _credential_use_started(self, account_id: str) -> None:
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> None:
            changed = connection.execute(
                "UPDATE account_state SET worker_state='CREDENTIAL_IN_USE',updated_at_ms=?,state_version=state_version+1 WHERE account_id=? AND worker_state='STOPPED'",
                (now, account_id),
            ).rowcount
            if not changed:
                raise Conflict(
                    "CREDENTIAL_RUNTIME_BLOCKED",
                    "Credential recovery is required before another runtime can start",
                )

        await self.database.transaction(work)

    async def _credential_use_finished(self, account_id: str, *, safe: bool) -> None:
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> None:
            if safe:
                connection.execute(
                    "UPDATE account_state SET worker_state='STOPPED',updated_at_ms=?,state_version=state_version+1 WHERE account_id=?",
                    (now, account_id),
                )
                return
            connection.execute(
                "UPDATE account_state SET worker_state='CREDENTIAL_QUARANTINED',auth_state='AUTH_REQUIRED',activation_state='UNSCHEDULED',overall_state='ERROR',last_error_code='CREDENTIAL_CHECKPOINT_FAILED',last_error_summary='Credential recovery is required',updated_at_ms=?,state_version=state_version+1 WHERE account_id=?",
                (now, account_id),
            )
            connection.execute(
                "UPDATE activation_attempts SET state='CANCELLED',completed_at_ms=?,updated_at_ms=?,state_version=state_version+1 WHERE account_id=? AND state='PLANNED'",
                (now, now, account_id),
            )

        await self.database.transaction(work)

    def _replace_active_row(
        self, connection: sqlite3.Connection, envelope: Envelope, now: int
    ) -> None:
        connection.execute(
            "UPDATE credential_bundles SET state='RETIRED',retired_at_ms=? WHERE account_id=? AND state='ACTIVE'",
            (now, envelope.account_id),
        )
        connection.execute(
            "INSERT INTO credential_bundles VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (
                envelope.bundle_id,
                envelope.account_id,
                "ACTIVE",
                envelope.envelope_version,
                envelope.payload_schema_version,
                envelope.key_id,
                envelope.nonce,
                envelope.ciphertext,
                envelope.aad,
                self.settings.codex_version,
                now,
                now,
                None,
            ),
        )

    async def _promote_active_payload(self, account_id: str, payload: dict[str, Any]) -> None:
        envelope = self.vault.encrypt(account_id, payload)
        now = self.clock.now_ms()
        await self.database.transaction(
            lambda connection: self._replace_active_row(connection, envelope, now)
        )

    async def _promote_login_source(
        self, account_id: str, attempt_id: str, payload: dict[str, Any]
    ) -> bool:
        envelope = self.vault.encrypt(account_id, payload)
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> bool:
            changed = connection.execute(
                "UPDATE login_attempts SET state='CHECKPOINTING_CREDENTIAL',updated_at_ms=? WHERE login_attempt_id=? AND state='VERIFYING_ACCOUNT'",
                (now, attempt_id),
            ).rowcount
            if not changed:
                return False
            self._replace_active_row(connection, envelope, now)
            return True

        return await self.database.transaction(work)

    async def _install_export_payload(self, account_id: str, payload: dict[str, Any]) -> bool:
        envelope = self.vault.encrypt(account_id, payload)
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> bool:
            if connection.execute(
                "SELECT 1 FROM credential_bundles WHERE account_id=? AND state='EXPORT'",
                (account_id,),
            ).fetchone():
                return False
            connection.execute(
                "INSERT INTO credential_bundles VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    envelope.bundle_id,
                    envelope.account_id,
                    "EXPORT",
                    envelope.envelope_version,
                    envelope.payload_schema_version,
                    envelope.key_id,
                    envelope.nonce,
                    envelope.ciphertext,
                    envelope.aad,
                    self.settings.codex_version,
                    now,
                    None,
                    None,
                ),
            )
            return True

        return await self.database.transaction(work)

    async def _await_critical(self, task: asyncio.Task[Any]) -> asyncio.CancelledError | None:
        cancellation: asyncio.CancelledError | None = None
        while not task.done():
            try:
                await asyncio.shield(task)
            except asyncio.CancelledError as error:
                cancellation = cancellation or error
        if task.cancelled():
            raise asyncio.CancelledError
        failure = task.exception()
        if failure:
            raise failure
        return cancellation

    async def _run_managed_locked(
        self,
        account: dict[str, Any],
        call: Callable[[Any], Awaitable[T]],
    ) -> T:
        account_id = str(account["account_id"])
        source = await self._credential_payload(account_id)
        source_fingerprint = self.vault.auth_fingerprint(source)
        await self._credential_use_started(account_id)
        try:
            runtime = await self.runtime.start_fresh(
                account_id, source, account.get("workspace_constraint")
            )
        except BaseException:
            await self._credential_use_finished(account_id, safe=True)
            raise
        result: T | None = None
        primary_error: BaseException | None = None
        checkpoint_error: BaseException | None = None
        async with runtime.lock:
            try:
                result = await call(runtime)
            except BaseException as error:
                primary_error = error

            async def checkpoint() -> None:
                await runtime.client.close()
                captured = self.vault.capture(
                    runtime.codex_home,
                    self.settings.codex_version,
                    account.get("workspace_constraint"),
                )
                if self.vault.auth_fingerprint(captured) != source_fingerprint:
                    await self._promote_active_payload(account_id, captured)

            checkpoint_task = asyncio.create_task(checkpoint())
            try:
                cancellation = await self._await_critical(checkpoint_task)
                primary_error = primary_error or cancellation
            except BaseException as error:
                checkpoint_error = error

        cleanup_task = asyncio.create_task(
            self.runtime.preserve(runtime)
            if checkpoint_error is not None
            else self.runtime.discard(runtime)
        )
        try:
            cancellation = await self._await_critical(cleanup_task)
            primary_error = primary_error or cancellation
        except BaseException as error:
            checkpoint_error = checkpoint_error or error
            preserve_task = asyncio.create_task(self.runtime.preserve(runtime))
            try:
                cancellation = await self._await_critical(preserve_task)
                primary_error = primary_error or cancellation
            except BaseException as ignored:
                del ignored

        state_task = asyncio.create_task(
            self._credential_use_finished(account_id, safe=checkpoint_error is None)
        )
        try:
            cancellation = await self._await_critical(state_task)
            primary_error = primary_error or cancellation
        except BaseException as error:
            checkpoint_error = checkpoint_error or error

        if checkpoint_error is not None:
            incident_task = asyncio.create_task(
                self.open_incident(
                    account_id,
                    "credential_checkpoint",
                    "ERROR",
                    "Codex credential state could not be safely checkpointed",
                )
            )
            try:
                cancellation = await self._await_critical(incident_task)
                primary_error = primary_error or cancellation
            except BaseException as ignored:
                del ignored
            raise WindowkeeperError(
                "CREDENTIAL_CHECKPOINT_FAILED",
                "Codex credential state could not be safely checkpointed",
                503,
            ) from checkpoint_error
        if primary_error is not None:
            raise primary_error
        return cast(T, result)

    async def _run_managed(
        self,
        account: dict[str, Any],
        call: Callable[[Any], Awaitable[T]],
    ) -> T:
        async with self._credential_lock(str(account["account_id"])):
            return await self._run_managed_locked(account, call)

    async def _issue_fork_candidate(
        self,
        account: dict[str, Any],
        source: dict[str, Any],
        source_identity: dict[str, Any],
        state: str,
        forbidden_fingerprints: set[str],
    ) -> tuple[dict[str, Any], bool]:
        account_id = str(account["account_id"])
        await self._credential_use_started(account_id)
        try:
            runtime = await self.runtime.start_fresh(
                account_id, source, account.get("workspace_constraint")
            )
        except BaseException:
            await self._credential_use_finished(account_id, safe=True)
            raise
        try:
            expected = dict(account)
            expected["upstream_email"] = verify_identity(account, source_identity).get("email")
            async with runtime.lock:
                identity = await runtime.adapter.account(refresh_token=True)
                verify_identity(expected, identity)
                await runtime.client.close()
                payload = self.vault.capture(
                    runtime.codex_home,
                    self.settings.codex_version,
                    account.get("workspace_constraint"),
                )
            if self.vault.auth_fingerprint(payload) in forbidden_fingerprints:
                raise WindowkeeperError(
                    "CODEX_TOKEN_NOT_ROTATED",
                    f"Codex did not create a separate {state.lower()} credential",
                )
            persist_task: asyncio.Task[Any]
            if state == "ACTIVE":
                persist_task = asyncio.create_task(
                    self._promote_active_payload(account["account_id"], payload)
                )
            else:
                persist_task = asyncio.create_task(
                    self._install_export_payload(account["account_id"], payload)
                )
            cancellation = await self._await_critical(persist_task)
            installed = True if state == "ACTIVE" else bool(persist_task.result())
            discard_task = asyncio.create_task(self.runtime.discard(runtime))
            discard_cancellation = await self._await_critical(discard_task)
            cancellation = cancellation or discard_cancellation
            state_task = asyncio.create_task(self._credential_use_finished(account_id, safe=True))
            state_cancellation = await self._await_critical(state_task)
            cancellation = cancellation or state_cancellation
            if cancellation:
                raise cancellation
            return identity, installed
        except BaseException as primary_error:
            close_failed = False
            close_task = asyncio.create_task(runtime.client.close())
            try:
                await self._await_critical(close_task)
            except BaseException as ignored:
                del ignored
                close_failed = True
            safe = state == "EXPORT" and not close_failed
            cleanup_task = asyncio.create_task(
                self.runtime.archive(runtime) if safe else self.runtime.preserve(runtime)
            )
            try:
                await self._await_critical(cleanup_task)
            except BaseException as ignored:
                del ignored
            state_task = asyncio.create_task(self._credential_use_finished(account_id, safe=safe))
            try:
                await self._await_critical(state_task)
            except BaseException as ignored:
                del ignored
            raise primary_error

    async def _fork_credentials(
        self,
        account: dict[str, Any],
        source: dict[str, Any],
        source_identity: dict[str, Any],
    ) -> tuple[dict[str, Any], bool, str | None]:
        source_fingerprint = self.vault.auth_fingerprint(source)
        managed_identity, _ = await self._issue_fork_candidate(
            account,
            source,
            source_identity,
            "ACTIVE",
            {source_fingerprint},
        )
        active = await self._credential_payload(account["account_id"])
        if await self._bundle_payload(account["account_id"], "EXPORT"):
            return managed_identity, True, None
        try:
            _, installed = await self._issue_fork_candidate(
                account,
                source,
                source_identity,
                "EXPORT",
                {source_fingerprint, self.vault.auth_fingerprint(active)},
            )
            return managed_identity, installed, None
        except Exception:
            return managed_identity, False, "EXPORT_FORK_FAILED"

    async def export_auth_json(self, public: str) -> bytes:
        account = await self._account_row(public)
        payload = await self._bundle_payload(account["account_id"], "EXPORT")
        if not payload:
            raise Conflict("AUTH_EXPORT_UNAVAILABLE", "No downloadable auth.json is available")
        return self.vault.auth_json(payload)

    async def refresh(self, public: str, trigger: str = "USER") -> str:
        account = await self._account_row(public)
        if account["auth_state"] != "VERIFIED" or account["worker_state"] != "STOPPED":
            raise Conflict(
                "USAGE_REFRESH_NOT_ELIGIBLE",
                "Credential recovery or reauthentication is required before usage refresh",
            )
        now = self.clock.now_ms()

        def coalesce(connection: sqlite3.Connection) -> tuple[str, bool]:
            active = connection.execute(
                "SELECT operation_id FROM operations WHERE account_id=? AND kind='usage.refresh' AND state IN('QUEUED','RUNNING','WAITING_FOR_USER','RETRY_SCHEDULED') ORDER BY created_at_ms LIMIT 1",
                (account["account_id"],),
            ).fetchone()
            if active:
                return str(active[0]), False
            operation_id = new_id()
            connection.execute(
                "INSERT INTO operations VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    operation_id,
                    account["account_id"],
                    "usage.refresh",
                    trigger,
                    "QUEUED",
                    None,
                    None,
                    None,
                    None,
                    None,
                    now,
                    None,
                    None,
                    None,
                    None,
                    1,
                ),
            )
            return operation_id, True

        operation_id, created = await self.database.transaction(coalesce)
        if created:
            self._background(self._run_refresh(account, operation_id))
        return operation_id

    async def _run_refresh(self, account: dict[str, Any], operation_id: str) -> None:
        started = self.clock.monotonic()
        await self._operation_state(
            operation_id, "RUNNING", "READING_USAGE", "Reading complete rate limits"
        )
        try:
            async with self._usage_semaphore:
                raw = await self._run_managed(
                    account, lambda runtime: runtime.adapter.rate_limits()
                )
            await self._commit_usage(account, dict(raw), operation_id, started)
        except Exception as error:
            await self._record_usage_failure(account, operation_id, started, error)

    async def _record_usage_failure(
        self,
        account: dict[str, Any],
        operation_id: str,
        started: float,
        error: Exception,
    ) -> None:
        now = self.clock.now_ms()
        duration = _integer((self.clock.monotonic() - started) * 1000)
        snapshot_id = new_id()
        error_code = error.code if isinstance(error, WindowkeeperError) else "USAGE_REFRESH_FAILED"
        summary = (
            str(redact(error.detail))[:200]
            if isinstance(error, WindowkeeperError)
            else "Usage could not be refreshed"
        )
        auth_failure = error_code == "CODEX_AUTH_REQUIRED"
        action_required = error_code in {"AUTH_IDENTITY_MISMATCH", "WORKSPACE_MISMATCH"}
        checkpoint_failure = error_code == "CREDENTIAL_CHECKPOINT_FAILED"

        def failed(connection: sqlite3.Connection) -> None:
            connection.execute(
                "INSERT INTO usage_snapshots VALUES(?,?,?,?,?,?,?,?,?,?,?)",
                (
                    snapshot_id,
                    account["account_id"],
                    now,
                    None,
                    0,
                    None,
                    None,
                    None,
                    error_code,
                    summary,
                    duration,
                ),
            )
            connection.execute(
                "UPDATE usage_current SET last_attempt_at_ms=?,stale=1,last_error_code=?,last_error_summary=?,state_version=state_version+1 WHERE account_id=?",
                (now, error_code, summary, account["account_id"]),
            )
            if auth_failure or checkpoint_failure:
                connection.execute(
                    "UPDATE account_state SET auth_state='AUTH_REQUIRED',usage_state='STALE',activation_state='UNSCHEDULED',overall_state=?,last_error_code=?,last_error_summary=?,state_version=state_version+1,updated_at_ms=? WHERE account_id=?",
                    (
                        "ERROR" if checkpoint_failure else "ACTION_REQUIRED",
                        error_code,
                        summary,
                        now,
                        account["account_id"],
                    ),
                )
            else:
                overall = "ACTION_REQUIRED" if action_required else "WARNING"
                connection.execute(
                    "UPDATE account_state SET usage_state='STALE',overall_state=?,last_error_code=?,last_error_summary=?,state_version=state_version+1,updated_at_ms=? WHERE account_id=?",
                    (overall, error_code, summary, now, account["account_id"]),
                )
            if auth_failure or action_required or checkpoint_failure:
                connection.execute(
                    "UPDATE activation_attempts SET state='CANCELLED',completed_at_ms=?,updated_at_ms=?,state_version=state_version+1 WHERE account_id=? AND state='PLANNED'",
                    (now, now, account["account_id"]),
                )

        await self.database.transaction(failed)
        if auth_failure:
            await self.open_incident(
                account["account_id"],
                "authentication_failed",
                "ERROR",
                "Codex authentication must be renewed",
            )
        await self._fail_operation(operation_id, error_code, summary)
        self.events.publish(
            "account.updated", {"resource_id": account["public_token"], "state": "WARNING"}
        )

    async def _commit_usage(
        self,
        account: dict[str, Any],
        raw: dict[str, Any],
        operation_id: str,
        started: float,
    ) -> None:
        normalized = normalize_usage(raw)
        now = self.clock.now_ms()
        snapshot_id = new_id()
        duration = _integer((self.clock.monotonic() - started) * 1000)
        short = normalized.short
        weekly = normalized.weekly

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "INSERT INTO usage_snapshots VALUES(?,?,?,?,?,?,?,?,?,?,?)",
                (
                    snapshot_id,
                    account["account_id"],
                    now - duration,
                    now,
                    1,
                    normalized.selected_limit_id,
                    json.dumps(redact(raw), separators=(",", ":")),
                    json.dumps(
                        {
                            "window_count": 2 + len(normalized.others),
                            "short_duration_minutes": short.duration_minutes if short else None,
                            "short_resets_at_s": short.resets_at_s if short else None,
                            "weekly_duration_minutes": weekly.duration_minutes if weekly else None,
                            "weekly_resets_at_s": weekly.resets_at_s if weekly else None,
                        },
                        separators=(",", ":"),
                    ),
                    None,
                    None,
                    duration,
                ),
            )
            connection.execute(
                """UPDATE usage_current SET snapshot_id=?,selected_limit_id=?,short_raw_slot=?,short_used_percent_raw=?,
                short_duration_minutes=?,short_resets_at_s=?,short_anomaly=?,weekly_raw_slot=?,weekly_used_percent_raw=?,
                weekly_duration_minutes=?,weekly_resets_at_s=?,weekly_anomaly=?,complete_read_at_ms=?,last_attempt_at_ms=?,
                stale=0,last_error_code=NULL,last_error_summary=NULL,source='APP_SERVER',state_version=state_version+1 WHERE account_id=?""",
                (
                    snapshot_id,
                    normalized.selected_limit_id,
                    short.slot if short else None,
                    short.used_percent if short else None,
                    short.duration_minutes if short else None,
                    short.resets_at_s if short else None,
                    1
                    if short
                    and short.used_percent is not None
                    and not 0 <= short.used_percent <= 100
                    else 0,
                    weekly.slot if weekly else None,
                    weekly.used_percent if weekly else None,
                    weekly.duration_minutes if weekly else None,
                    weekly.resets_at_s if weekly else None,
                    1
                    if weekly
                    and weekly.used_percent is not None
                    and not 0 <= weekly.used_percent <= 100
                    else 0,
                    now,
                    now,
                    account["account_id"],
                ),
            )
            connection.execute(
                "UPDATE account_state SET usage_state='FRESH',overall_state=CASE WHEN activation_state IN('AMBIGUOUS','SAFETY_BLOCKED') THEN 'WARNING' WHEN auth_state='VERIFIED' THEN 'HEALTHY' ELSE overall_state END,last_error_code=CASE WHEN activation_state IN('AMBIGUOUS','SAFETY_BLOCKED') THEN last_error_code END,last_error_summary=CASE WHEN activation_state IN('AMBIGUOUS','SAFETY_BLOCKED') THEN last_error_summary END,state_version=state_version+1,updated_at_ms=? WHERE account_id=?",
                (now, account["account_id"]),
            )
            connection.execute(
                "UPDATE operations SET state='SUCCEEDED',progress_code='COMPLETE',progress_summary='Usage refreshed',completed_at_ms=?,state_version=state_version+1 WHERE operation_id=?",
                (now, operation_id),
            )

        await self.database.transaction(work)
        await self.plan(account["public_token"], normalized.short)
        self.events.publish(
            "account.updated", {"resource_id": account["public_token"], "state": "FRESH"}
        )

    async def plan(self, public: str, short_override: RawWindow | None = None) -> None:
        account = await self._account_row(public)

        def read(
            connection: sqlite3.Connection,
        ) -> tuple[dict[str, Any] | None, set[str], tuple[int, ...], bool, int | None, int]:
            usage = connection.execute(
                "SELECT * FROM usage_current WHERE account_id=?", (account["account_id"],)
            ).fetchone()
            keys = {
                str(row[0])
                for row in connection.execute(
                    "SELECT window_key FROM activation_attempts WHERE account_id=?",
                    (account["account_id"],),
                )
            }
            reset_evidence = tuple(
                _integer(row[0])
                for row in connection.execute(
                    "SELECT basis_reset_at_s FROM activation_attempts WHERE account_id=? AND basis_reset_at_s IS NOT NULL",
                    (account["account_id"],),
                )
            )
            ambiguous = bool(
                connection.execute(
                    "SELECT 1 FROM activation_attempts WHERE account_id=? AND state IN('AMBIGUOUS','SAFETY_BLOCKED','TURN_DISPATCHING','TURN_ACCEPTED','RUNNING')",
                    (account["account_id"],),
                ).fetchone()
            )
            last = connection.execute(
                "SELECT completed_at_ms FROM activation_attempts WHERE account_id=? AND state='COMPLETED_OK' ORDER BY completed_at_ms DESC LIMIT 1",
                (account["account_id"],),
            ).fetchone()
            observations = 0
            if usage and usage["short_duration_minutes"]:
                for row in connection.execute(
                    "SELECT raw_shape_summary_json FROM usage_snapshots WHERE account_id=? AND success=1 ORDER BY completed_at_ms DESC LIMIT 2",
                    (account["account_id"],),
                ):
                    try:
                        normalized = json.loads(row[0] or "{}")
                    except (json.JSONDecodeError, TypeError):
                        continue
                    if normalized.get("short_duration_minutes") == usage["short_duration_minutes"]:
                        observations += 1
            return (
                dict(usage) if usage else None,
                keys,
                reset_evidence,
                ambiguous,
                _integer(last[0]) if last else None,
                observations,
            )

        usage, keys, reset_evidence, ambiguous, last, observations = await self.database.call(read)
        short = short_override
        if not short and usage and usage.get("short_duration_minutes"):
            short = RawWindow(
                str(usage.get("short_raw_slot") or "short"),
                usage.get("short_used_percent_raw"),
                usage.get("short_duration_minutes"),
                usage.get("short_resets_at_s"),
            )
        weekly = None
        if usage and usage.get("weekly_duration_minutes"):
            weekly = RawWindow(
                str(usage.get("weekly_raw_slot") or "weekly"),
                usage.get("weekly_used_percent_raw"),
                usage.get("weekly_duration_minutes"),
                usage.get("weekly_resets_at_s"),
            )
        decision = decide_schedule(
            account_id=account["account_id"],
            enabled=bool(account["enabled"]),
            auth_verified=account["auth_state"] == "VERIFIED",
            short=short,
            weekly=weekly,
            now_ms=self.clock.now_ms(),
            safety_delay_seconds=self.settings.activation_safety_delay_seconds,
            jitter_max_seconds=self.settings.activation_jitter_max_seconds,
            existing_window_keys=keys,
            ambiguous_predecessor=ambiguous,
            last_successful_activation_ms=last,
            consistent_observations=observations,
            estimated_enabled=self.settings.estimated_schedule_enabled,
        )
        if not decision.window_key or decision.window_key in keys or not decision.run_at_ms:
            return
        if decision.basis_reset_at_s and any(
            abs(decision.basis_reset_at_s - reset_at_s) <= 60 for reset_at_s in reset_evidence
        ):
            return
        activation_id = new_id()
        now = self.clock.now_ms()

        def create(connection: sqlite3.Connection) -> bool:
            if connection.execute(
                "SELECT 1 FROM activation_attempts WHERE account_id=? AND state IN('AMBIGUOUS','SAFETY_BLOCKED','TURN_DISPATCHING','TURN_ACCEPTED','RUNNING')",
                (account["account_id"],),
            ).fetchone():
                return False
            connection.execute(
                "INSERT INTO activation_attempts VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    activation_id,
                    account["account_id"],
                    decision.window_key,
                    "SCHEDULED",
                    1,
                    PROMPT_DIGEST,
                    decision.source,
                    decision.confidence,
                    decision.basis_reset_at_s,
                    decision.basis_duration_minutes,
                    decision.run_at_ms,
                    "PLANNED",
                    None,
                    None,
                    activation_id,
                    None,
                    None,
                    None,
                    now,
                    now,
                    None,
                    1,
                ),
            )
            connection.execute(
                "UPDATE account_state SET activation_state=?,state_version=state_version+1,updated_at_ms=? WHERE account_id=?",
                (f"{decision.confidence}_SCHEDULE", now, account["account_id"]),
            )
            return True

        if not await self.database.transaction(create):
            return
        self.events.publish(
            "account.updated", {"resource_id": public, "next_activation_ms": decision.run_at_ms}
        )

    async def activate(self, public: str, trigger: str = "MANUAL") -> str:
        account = await self._account_row(public)
        if not account["enabled"] or account["auth_state"] != "VERIFIED":
            raise Conflict(
                "ACTIVATION_NOT_ELIGIBLE", "Enable and authenticate the account before activation"
            )
        if account["activation_state"] in {"AMBIGUOUS", "SAFETY_BLOCKED"}:
            raise Conflict(
                "ACTIVATION_SAFETY_BLOCKED", "Resolve the ambiguous predecessor before activation"
            )
        operation_id = await self._create_operation(
            account["account_id"], "activation.run", trigger
        )
        now = self.clock.now_ms()

        activation_id = new_id()

        def create(connection: sqlite3.Connection) -> tuple[str, bool]:
            active = connection.execute(
                "SELECT activation_id FROM activation_attempts WHERE account_id=? AND state IN('QUEUED','THREAD_CREATED','TURN_DISPATCHING','TURN_ACCEPTED','RUNNING','AMBIGUOUS','SAFETY_BLOCKED') ORDER BY created_at_ms LIMIT 1",
                (account["account_id"],),
            ).fetchone()
            if active:
                return str(active[0]), False
            planned = connection.execute(
                "SELECT activation_id FROM activation_attempts WHERE account_id=? AND state='PLANNED' ORDER BY scheduled_for_ms,created_at_ms LIMIT 1",
                (account["account_id"],),
            ).fetchone()
            if planned:
                changed = connection.execute(
                    "UPDATE activation_attempts SET state='QUEUED',trigger=?,scheduled_for_ms=?,updated_at_ms=?,state_version=state_version+1 WHERE activation_id=? AND state='PLANNED'",
                    (trigger, now, now, planned[0]),
                ).rowcount
                return str(planned[0]), bool(changed)
            usage = connection.execute(
                "SELECT short_resets_at_s,short_duration_minutes FROM usage_current WHERE account_id=?",
                (account["account_id"],),
            ).fetchone()
            reset_at_s = _integer(usage[0]) if usage and usage[0] else None
            duration_minutes = _integer(usage[1]) if usage and usage[1] else None
            key = (
                f"reported:{reset_at_s}"
                if reset_at_s and reset_at_s * 1000 > now
                else f"manual:unknown:{now // 86_400_000}"
            )
            existing = connection.execute(
                "SELECT activation_id,state FROM activation_attempts WHERE account_id=? AND (window_key=? OR (? IS NOT NULL AND basis_reset_at_s BETWEEN ? AND ?)) ORDER BY created_at_ms LIMIT 1",
                (
                    account["account_id"],
                    key,
                    reset_at_s,
                    (reset_at_s - 60) if reset_at_s else 0,
                    (reset_at_s + 60) if reset_at_s else 0,
                ),
            ).fetchone()
            if existing:
                selected = str(existing[0])
                if existing[1] != "PLANNED":
                    return selected, False
                changed = connection.execute(
                    "UPDATE activation_attempts SET state='QUEUED',scheduled_for_ms=?,updated_at_ms=?,state_version=state_version+1 WHERE activation_id=? AND state='PLANNED'",
                    (now, now, selected),
                ).rowcount
                return selected, bool(changed)
            connection.execute(
                "INSERT INTO activation_attempts VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    activation_id,
                    account["account_id"],
                    key,
                    trigger,
                    1,
                    PROMPT_DIGEST,
                    "MANUAL" if trigger == "MANUAL" else "REPORTED_RESET",
                    "CONFIRMED" if trigger != "MANUAL" else "OPERATOR",
                    reset_at_s,
                    duration_minutes,
                    now,
                    "QUEUED",
                    None,
                    None,
                    activation_id,
                    None,
                    None,
                    None,
                    now,
                    now,
                    None,
                    1,
                ),
            )
            return activation_id, True

        try:
            activation_id, admitted = await self.database.transaction(create)
        except sqlite3.IntegrityError as error:
            await self._fail_operation(
                operation_id, "ACTIVATION_DUPLICATE", "This window already has an activation"
            )
            raise Conflict(
                "ACTIVATION_DUPLICATE", "This window already has an activation"
            ) from error
        if not admitted:
            await self._fail_operation(
                operation_id, "ACTIVATION_DUPLICATE", "This window already has an activation"
            )
            raise Conflict("ACTIVATION_DUPLICATE", "This window already has an activation")
        await self.database.call(
            lambda connection: connection.execute(
                "UPDATE operations SET result_json=? WHERE operation_id=?",
                (json.dumps({"activation_id": activation_id}), operation_id),
            )
        )
        self._background(self._run_activation(account, activation_id, operation_id))
        return operation_id

    async def _run_activation(
        self, account: dict[str, Any], activation_id: str, operation_id: str
    ) -> None:
        try:
            await self._operation_state(
                operation_id, "RUNNING", "STARTING_RUNTIME", "Starting activation"
            )
            if not await self.database.call(
                lambda connection: self._activation_eligible(
                    connection, account["account_id"], activation_id
                )
            ):
                raise Conflict(
                    "ACTIVATION_NOT_ELIGIBLE", "Account became ineligible before activation"
                )
            async with self._activation_semaphore:
                result = await self._run_managed(
                    account,
                    lambda runtime: self._perform_activation(
                        runtime, account, activation_id, operation_id
                    ),
                )
            await self._complete_activation(account, activation_id, operation_id, result)
        except BaseException as error:
            await self._handle_activation_error(account, activation_id, operation_id, error)

    def _activation_eligible(
        self, connection: sqlite3.Connection, account_id: str, activation_id: str
    ) -> bool:
        row = connection.execute(
            "SELECT a.enabled,a.deleted_at_ms,s.auth_state,s.activation_state,aa.state,"
            "u.short_used_percent_raw,u.weekly_used_percent_raw "
            "FROM accounts a JOIN account_state s USING(account_id) "
            "JOIN activation_attempts aa USING(account_id) "
            "JOIN usage_current u USING(account_id) "
            "WHERE a.account_id=? AND aa.activation_id=?",
            (account_id, activation_id),
        ).fetchone()
        return bool(
            row
            and row[0]
            and row[1] is None
            and row[2] == "VERIFIED"
            and row[3] not in {"AMBIGUOUS", "SAFETY_BLOCKED"}
            and row[4] in {"QUEUED", "THREAD_CREATED"}
            and (row[5] is None or _integer(row[5]) < 100)
            and (row[6] is None or _integer(row[6]) < 100)
        )

    async def _perform_activation(
        self,
        runtime: Any,
        account: dict[str, Any],
        activation_id: str,
        operation_id: str,
    ) -> str:
        model = await runtime.adapter.activation_model()
        thread_id = await runtime.adapter.create_thread(str(runtime.workspace), model)
        now = self.clock.now_ms()

        def thread_created(connection: sqlite3.Connection) -> None:
            connection.execute(
                "UPDATE activation_attempts SET state='THREAD_CREATED',upstream_thread_id=?,updated_at_ms=?,state_version=state_version+1 WHERE activation_id=?",
                (thread_id, now, activation_id),
            )
            connection.execute(
                "UPDATE operations SET result_json=? WHERE operation_id=?",
                (
                    json.dumps(
                        {
                            "activation_id": activation_id,
                            "model": model.model,
                            "reasoning_effort": model.effort,
                            "service_tier": "default",
                            "pricing_verified_at": PRICING_VERIFIED_AT,
                        }
                    ),
                    operation_id,
                ),
            )
            connection.execute(
                "INSERT INTO activation_operations VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    new_id(),
                    activation_id,
                    "SUBMIT",
                    1,
                    "STARTED",
                    None,
                    None,
                    None,
                    None,
                    None,
                    thread_id,
                    None,
                    None,
                    None,
                    None,
                    now,
                    now,
                ),
            )

        await self.database.transaction(thread_created)
        if not await self.database.call(
            lambda connection: self._activation_eligible(
                connection, account["account_id"], activation_id
            )
        ):
            raise Conflict(
                "ACTIVATION_NOT_ELIGIBLE",
                "Account became ineligible before activation submission",
            )
        dispatch_started = self.clock.now_ms()

        def mark_dispatching(connection: sqlite3.Connection) -> None:
            connection.execute(
                "UPDATE activation_attempts SET state='TURN_DISPATCHING',updated_at_ms=?,state_version=state_version+1 WHERE activation_id=? AND state='THREAD_CREATED'",
                (dispatch_started, activation_id),
            )
            connection.execute(
                "UPDATE activation_operations SET state='REQUEST_WRITING',write_started_at_ms=?,updated_at_ms=? WHERE activation_id=? AND state='STARTED'",
                (dispatch_started, dispatch_started, activation_id),
            )

        await self.database.transaction(mark_dispatching)
        turn_id, _ = await runtime.adapter.start_turn(thread_id, activation_id, PROMPT, model)
        await self._accept_turn(activation_id, turn_id)
        return await self._await_turn(runtime.adapter.client.notifications(), turn_id)

    async def _handle_activation_error(
        self,
        account: dict[str, Any],
        activation_id: str,
        operation_id: str,
        error: BaseException,
    ) -> None:
        not_eligible = (
            isinstance(error, WindowkeeperError) and error.code == "ACTIVATION_NOT_ELIGIBLE"
        )
        if not_eligible:
            now = self.clock.now_ms()

            def pause_if_exhausted(connection: sqlite3.Connection) -> bool:
                row = connection.execute(
                    "SELECT aa.state,aa.schedule_confidence,u.short_used_percent_raw,u.weekly_used_percent_raw FROM activation_attempts aa JOIN usage_current u USING(account_id) WHERE aa.activation_id=?",
                    (activation_id,),
                ).fetchone()
                if not row:
                    return False
                exhausted = any(value is not None and _integer(value) >= 100 for value in row[2:])
                if row[0] not in {"QUEUED", "THREAD_CREATED"} or not exhausted:
                    return str(row[0]) == "CANCELLED"
                connection.execute(
                    "UPDATE activation_attempts SET state='PLANNED',upstream_thread_id=NULL,updated_at_ms=?,state_version=state_version+1 WHERE activation_id=?",
                    (now, activation_id),
                )
                connection.execute(
                    "DELETE FROM activation_operations WHERE activation_id=? AND state='STARTED'",
                    (activation_id,),
                )
                connection.execute(
                    "UPDATE operations SET state='CANCELLED',progress_code='USAGE_EXHAUSTED',progress_summary='Waiting for usage to reset',completed_at_ms=?,state_version=state_version+1 WHERE operation_id=?",
                    (now, operation_id),
                )
                schedule_state = (
                    "ESTIMATED_SCHEDULE" if str(row[1]) == "ESTIMATED" else "CONFIRMED_SCHEDULE"
                )
                connection.execute(
                    "UPDATE account_state SET activation_state=?,state_version=state_version+1,updated_at_ms=? WHERE account_id=?",
                    (schedule_state, now, account["account_id"]),
                )
                return True

            if await self.database.transaction(pause_if_exhausted):
                return
        safety_blocked = False
        definitely_failed = False
        if isinstance(error, WindowkeeperError):
            safety_blocked = error.code == "ACTIVATION_SAFETY_VIOLATION"
            definitely_failed = error.code in {
                "ACTIVATION_UPSTREAM_FAILED",
                "CODEX_RPC_REJECTED",
                "CODEX_AUTH_REQUIRED",
            }
        await self._ambiguous_activation(
            account,
            activation_id,
            operation_id,
            str(error)[:200],
            safety_blocked=safety_blocked,
            definitely_failed=definitely_failed,
        )
        if isinstance(error, asyncio.CancelledError):
            raise error

    async def _await_turn(self, notifications: Any, turn_id: str) -> str:
        text = ""
        try:
            async with asyncio.timeout(300):
                async for event in notifications:
                    method = str(event.get("method", ""))
                    raw_params = event.get("params")
                    params = raw_params if isinstance(raw_params, dict) else {}
                    item = params.get("item")
                    item_type = str(item.get("type", "")) if isinstance(item, dict) else ""
                    safety_evidence = f"{method}/{item_type}".lower()
                    if any(
                        marker in safety_evidence
                        for marker in (
                            "commandexecution",
                            "filechange",
                            "toolcall",
                            "requestapproval",
                            "requestuserinput",
                        )
                    ):
                        raise WindowkeeperError(
                            "ACTIVATION_SAFETY_VIOLATION",
                            "Activation requested a forbidden action",
                        )
                    turn = params.get("turn")
                    event_turn_id = (
                        turn.get("id") if isinstance(turn, dict) else params.get("turnId")
                    )
                    if event_turn_id != turn_id:
                        continue
                    if method in {"item/agentMessage/delta", "item/agentMessageDelta"}:
                        text += str(params.get("delta", ""))
                    if method == "turn/completed":
                        status = str(turn.get("status")) if isinstance(turn, dict) else "completed"
                        if status == "completed":
                            return text.strip()
                        raise WindowkeeperError(
                            "ACTIVATION_UPSTREAM_FAILED",
                            f"Codex turn ended with status {status}",
                        )
        except TimeoutError as error:
            raise TimeoutError("activation did not reach a terminal state") from error
        raise TimeoutError("Codex notification stream closed before activation completed")

    async def _accept_turn(self, activation_id: str, turn_id: str) -> None:
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "UPDATE activation_attempts SET state='TURN_ACCEPTED',upstream_turn_id=?,updated_at_ms=?,state_version=state_version+1 WHERE activation_id=?",
                (turn_id, now, activation_id),
            )
            connection.execute(
                "UPDATE activation_operations SET state='AWAITING_RESPONSE',upstream_turn_id=?,write_completed_at_ms=?,accepted_at_ms=?,updated_at_ms=? WHERE activation_id=? AND state IN('STARTED','REQUEST_WRITING')",
                (turn_id, now, now, now, activation_id),
            )

        await self.database.transaction(work)

    async def _complete_activation(
        self, account: dict[str, Any], activation_id: str, operation_id: str, result: str
    ) -> None:
        now = self.clock.now_ms()
        normalized = "COMPLETED_OK" if result == "OK" else "COMPLETED_WARNING"

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "UPDATE activation_attempts SET state=?,normalized_result=?,terminal_status='COMPLETED',ambiguity_reason=NULL,completed_at_ms=?,updated_at_ms=?,state_version=state_version+1 WHERE activation_id=?",
                (normalized, normalized, now, now, activation_id),
            )
            connection.execute(
                "UPDATE activation_operations SET state='COMPLETED',error_code=NULL,error_summary=NULL,completed_at_ms=?,updated_at_ms=? WHERE activation_id=?",
                (now, now, activation_id),
            )
            connection.execute(
                "UPDATE activation_attempts SET state='CANCELLED',ambiguity_reason='Superseded by successful activation',completed_at_ms=?,updated_at_ms=?,state_version=state_version+1 WHERE account_id=? AND activation_id<>? AND state='PLANNED'",
                (now, now, account["account_id"], activation_id),
            )
            connection.execute(
                "UPDATE operations SET state='SUCCEEDED',progress_code=?,progress_summary=?,error_code=NULL,error_summary=NULL,completed_at_ms=?,state_version=state_version+1 WHERE operation_id=?",
                (
                    normalized,
                    "Activation completed"
                    if result == "OK"
                    else "Activation completed with an unexpected response",
                    now,
                    operation_id,
                ),
            )
            connection.execute(
                "UPDATE account_state SET activation_state=?,overall_state=?,last_error_code=NULL,last_error_summary=NULL,state_version=state_version+1,updated_at_ms=? WHERE account_id=?",
                (
                    "UNSCHEDULED" if result == "OK" else "WARNING",
                    "HEALTHY" if result == "OK" else "WARNING",
                    now,
                    account["account_id"],
                ),
            )

        await self.database.transaction(work)
        self.events.publish(
            "account.updated", {"resource_id": account["public_token"], "activation": normalized}
        )

    async def _ambiguous_activation(
        self,
        account: dict[str, Any],
        activation_id: str,
        operation_id: str,
        reason: str,
        *,
        safety_blocked: bool = False,
        definitely_failed: bool = False,
    ) -> None:
        now = self.clock.now_ms()
        safe_reason = str(redact(reason))[:200]

        def work(connection: sqlite3.Connection) -> str:
            row = connection.execute(
                "SELECT state FROM activation_attempts WHERE activation_id=?", (activation_id,)
            ).fetchone()
            state = (
                "AMBIGUOUS"
                if not definitely_failed
                and row
                and row[0]
                in {"TURN_DISPATCHING", "TURN_ACCEPTED", "RUNNING", "AMBIGUOUS", "SAFETY_BLOCKED"}
                else "FAILED_DEFINITE"
            )
            connection.execute(
                "UPDATE activation_attempts SET state=?,ambiguity_reason=?,updated_at_ms=?,completed_at_ms=?,state_version=state_version+1 WHERE activation_id=?",
                (state, safe_reason, now, now, activation_id),
            )
            connection.execute(
                "UPDATE activation_operations SET state=?,error_code=?,error_summary=?,completed_at_ms=?,updated_at_ms=? WHERE activation_id=? AND state IN('STARTED','REQUEST_WRITING','AWAITING_RESPONSE','RECONCILING')",
                (
                    "AMBIGUOUS" if state == "AMBIGUOUS" else "FAILED",
                    "ACTIVATION_SAFETY_VIOLATION"
                    if safety_blocked
                    else ("ACTIVATION_AMBIGUOUS" if state == "AMBIGUOUS" else "ACTIVATION_FAILED"),
                    safe_reason,
                    now,
                    now,
                    activation_id,
                ),
            )
            connection.execute(
                "UPDATE operations SET state=?,error_code=?,error_summary=?,completed_at_ms=?,state_version=state_version+1 WHERE operation_id=?",
                (
                    "AMBIGUOUS" if state == "AMBIGUOUS" else "FAILED",
                    "ACTIVATION_SAFETY_VIOLATION"
                    if safety_blocked
                    else ("ACTIVATION_AMBIGUOUS" if state == "AMBIGUOUS" else "ACTIVATION_FAILED"),
                    safe_reason,
                    now,
                    operation_id,
                ),
            )
            connection.execute(
                "UPDATE account_state SET activation_state=?,overall_state=?,last_error_code=?,last_error_summary=?,state_version=state_version+1,updated_at_ms=? WHERE account_id=?",
                (
                    "SAFETY_BLOCKED"
                    if safety_blocked
                    else ("AMBIGUOUS" if state == "AMBIGUOUS" else "UNSCHEDULED"),
                    "WARNING" if state == "AMBIGUOUS" else "ERROR",
                    "ACTIVATION_SAFETY_VIOLATION"
                    if safety_blocked
                    else ("ACTIVATION_AMBIGUOUS" if state == "AMBIGUOUS" else "ACTIVATION_FAILED"),
                    safe_reason,
                    now,
                    account["account_id"],
                ),
            )
            return state

        state = await self.database.transaction(work)
        if state == "AMBIGUOUS":
            await self.open_incident(
                account["account_id"],
                "activation_safety" if safety_blocked else "activation_ambiguous",
                "ERROR",
                "Activation requested a forbidden action"
                if safety_blocked
                else "Activation outcome could not be proven",
            )
        self.events.publish(
            "account.updated",
            {
                "resource_id": account["public_token"],
                "activation": "SAFETY_BLOCKED"
                if safety_blocked
                else ("AMBIGUOUS" if state == "AMBIGUOUS" else "FAILED_DEFINITE"),
            },
        )

    async def open_incident(self, account_id: str, kind: str, severity: str, summary: str) -> str:
        incident_id = new_id()
        now = self.clock.now_ms()
        summary = str(redact(summary))[:200]

        def work(connection: sqlite3.Connection) -> tuple[dict[str, Any], bool]:
            row = connection.execute(
                "SELECT incident_id FROM incidents WHERE scope_kind='account' AND scope_key=? AND problem_type=? AND state='OPEN'",
                (account_id, kind),
            ).fetchone()
            opened = not row
            if row:
                incident = str(row[0])
                connection.execute(
                    "UPDATE incidents SET occurrence_count=occurrence_count+1,last_seen_at_ms=?,summary=?,state_version=state_version+1 WHERE incident_id=?",
                    (now, summary, incident),
                )
            else:
                incident = incident_id
                connection.execute(
                    "INSERT INTO incidents VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                    (
                        incident,
                        "account",
                        account_id,
                        kind,
                        "OPEN",
                        severity,
                        summary,
                        kind.upper(),
                        1,
                        now,
                        now,
                        None,
                        None,
                        1,
                    ),
                )
            details = connection.execute(
                """SELECT i.incident_id,i.problem_type,i.severity,i.summary,
                i.occurrence_count,i.opened_at_ms,i.last_seen_at_ms,a.display_name,s.upstream_email,
                a.public_token,COALESCE(s.last_error_code,i.current_error_code) AS cause_code,
                COALESCE(s.last_error_summary,i.summary) AS cause_summary
                FROM incidents i JOIN accounts a ON a.account_id=i.scope_key
                JOIN account_state s ON s.account_id=a.account_id WHERE i.incident_id=?""",
                (incident,),
            ).fetchone()
            if not details:
                raise RuntimeError("incident context is unavailable")
            return dict(details), opened

        details, opened = await self.database.transaction(work)
        incident_id = str(details["incident_id"])
        self.events.publish("incident.updated", {"incident_id": incident_id, "state": "OPEN"})
        if self.webhooks:
            reason, action = INCIDENT_GUIDANCE.get(
                kind,
                (
                    "Windowkeeper detected an account condition that requires operator attention.",
                    "Open the account and Incidents pages, review the latest operation, and correct the reported condition.",
                ),
            )
            await self.webhooks.emit(
                "incident.opened" if opened else "incident.updated",
                f"account:{details['public_token']}",
                _incident_webhook_data(details, "OPEN", str(details["summary"]), reason, action),
                incident_id,
            )
        return incident_id

    async def resolve_incident(self, account_id: str, kind: str) -> None:
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> dict[str, Any] | None:
            row = connection.execute(
                """SELECT i.incident_id,i.problem_type,i.severity,i.summary,
                i.current_error_code AS cause_code,i.summary AS cause_summary,
                i.occurrence_count,i.opened_at_ms,i.last_seen_at_ms,a.display_name,s.upstream_email,
                a.public_token FROM incidents i JOIN accounts a ON a.account_id=i.scope_key
                JOIN account_state s ON s.account_id=a.account_id
                WHERE i.scope_kind='account' AND i.scope_key=? AND i.problem_type=? AND i.state='OPEN'""",
                (account_id, kind),
            ).fetchone()
            if not row:
                return None
            connection.execute(
                "UPDATE incidents SET state='RESOLVED',resolved_at_ms=?,resolution_reason='RECOVERED',state_version=state_version+1 WHERE incident_id=?",
                (now, row["incident_id"]),
            )
            return dict(row)

        details = await self.database.transaction(work)
        if not details:
            return
        incident_id = str(details["incident_id"])
        self.events.publish("incident.updated", {"incident_id": incident_id, "state": "RESOLVED"})
        if self.webhooks:
            reason, _ = INCIDENT_GUIDANCE.get(
                kind,
                (
                    "Windowkeeper previously detected an account condition requiring attention.",
                    "",
                ),
            )
            await self.webhooks.emit(
                "incident.resolved",
                f"account:{details['public_token']}",
                _incident_webhook_data(
                    details,
                    "RESOLVED",
                    f"Recovered from: {details['summary']}",
                    reason,
                    "No action required. Windowkeeper closed this incident and resumed normal account processing.",
                )
                | {"resolved_at_ms": now},
                incident_id,
            )

    async def operations(self, limit: int = 100) -> list[dict[str, Any]]:
        def work(connection: sqlite3.Connection) -> list[dict[str, Any]]:
            return [
                dict(row)
                for row in connection.execute(
                    "SELECT * FROM operations ORDER BY created_at_ms DESC LIMIT ?",
                    (min(limit, 500),),
                )
            ]

        return await self.database.call(work)

    async def operation(self, operation_id: str) -> dict[str, Any]:
        def work(connection: sqlite3.Connection) -> dict[str, Any] | None:
            row = connection.execute(
                "SELECT * FROM operations WHERE operation_id=?", (operation_id,)
            ).fetchone()
            return dict(row) if row else None

        value = await self.database.call(work)
        if not value:
            raise WindowkeeperError("OPERATION_NOT_FOUND", "Operation not found", 404)
        return value

    async def incidents(self) -> list[dict[str, Any]]:
        def work(connection: sqlite3.Connection) -> list[dict[str, Any]]:
            return [
                dict(row)
                for row in connection.execute(
                    "SELECT * FROM incidents ORDER BY opened_at_ms DESC LIMIT 200"
                )
            ]

        return await self.database.call(work)

    async def acknowledge_ambiguity(self, public: str) -> None:
        account = await self._account_row(public)
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> bool:
            changed = connection.execute(
                "UPDATE activation_attempts SET state='AMBIGUOUS_CLOSED',updated_at_ms=?,state_version=state_version+1 WHERE account_id=? AND state IN('AMBIGUOUS','SAFETY_BLOCKED')",
                (now, account["account_id"]),
            ).rowcount
            if changed:
                connection.execute(
                    "UPDATE activation_attempts SET state='CANCELLED',ambiguity_reason='Discarded after ambiguity review',completed_at_ms=?,updated_at_ms=?,state_version=state_version+1 WHERE account_id=? AND state='PLANNED'",
                    (now, now, account["account_id"]),
                )
                connection.execute(
                    "UPDATE account_state SET activation_state='UNSCHEDULED',overall_state='WARNING',last_error_code=NULL,last_error_summary=NULL,state_version=state_version+1,updated_at_ms=? WHERE account_id=?",
                    (now, account["account_id"]),
                )
            return bool(changed)

        if not await self.database.transaction(work):
            raise Conflict("ACTIVATION_NOT_AMBIGUOUS", "No ambiguous activation is open")
        await self.resolve_incident(account["account_id"], "activation_ambiguous")
        await self.resolve_incident(account["account_id"], "activation_safety")
        await self.plan(public)
        self.events.publish(
            "account.updated", {"resource_id": public, "activation": "AMBIGUOUS_CLOSED"}
        )

    async def set_labels(self, public: str, labels: list[str]) -> None:
        account = await self._account_row(public)
        clean = sorted({" ".join(value.split()) for value in labels if value.strip()})
        if len(clean) > 20 or any(len(label) > 40 for label in clean):
            raise WindowkeeperError(
                "ACCOUNT_LABELS_INVALID", "Use at most 20 labels of 1-40 characters"
            )
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "DELETE FROM account_labels WHERE account_id=?", (account["account_id"],)
            )
            for label in clean:
                row = connection.execute(
                    "SELECT label_id FROM labels WHERE lower(name)=lower(?)", (label,)
                ).fetchone()
                label_id = str(row[0]) if row else new_id()
                if not row:
                    connection.execute("INSERT INTO labels VALUES(?,?,?)", (label_id, label, now))
                connection.execute(
                    "INSERT INTO account_labels VALUES(?,?)", (account["account_id"], label_id)
                )
            connection.execute(
                "DELETE FROM labels WHERE NOT EXISTS (SELECT 1 FROM account_labels WHERE account_labels.label_id=labels.label_id)"
            )

        await self.database.transaction(work)
        self.events.publish("account.updated", {"resource_id": public, "labels": clean})

    async def set_enabled(self, public: str, enabled: bool) -> None:
        account = await self._account_row(public)
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "UPDATE accounts SET enabled=?,updated_at_ms=? WHERE account_id=?",
                (1 if enabled else 0, now, account["account_id"]),
            )
            connection.execute(
                "UPDATE account_state SET overall_state=?,state_version=state_version+1,updated_at_ms=? WHERE account_id=?",
                ("STARTING" if enabled else "DISABLED", now, account["account_id"]),
            )
            if not enabled:
                connection.execute(
                    "UPDATE activation_attempts SET state='CANCELLED',completed_at_ms=?,updated_at_ms=?,state_version=state_version+1 WHERE account_id=? AND state IN('PLANNED','QUEUED','THREAD_CREATED')",
                    (now, now, account["account_id"]),
                )
                connection.execute(
                    "UPDATE operations SET state='CANCELLED',progress_code='ACCOUNT_DISABLED',progress_summary='Account was disabled',completed_at_ms=?,state_version=state_version+1 WHERE account_id=? AND kind='activation.run' AND state='QUEUED'",
                    (now, account["account_id"]),
                )

        await self.database.transaction(work)
        if not enabled:
            await self.runtime.stop(account["account_id"])
        elif account["auth_state"] == "VERIFIED":
            await self.plan(public)
        self.events.publish("account.updated", {"resource_id": public, "enabled": enabled})

    async def delete_account(self, public: str, confirmation: str) -> None:
        account = await self._account_row(public)
        if confirmation != account["display_name"]:
            raise Conflict("DELETE_CONFIRMATION_MISMATCH", "Type the account name exactly")
        await self.runtime.stop(account["account_id"])
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "UPDATE accounts SET enabled=0,lifecycle_state='DELETED',deleted_at_ms=?,updated_at_ms=? WHERE account_id=?",
                (now, now, account["account_id"]),
            )
            connection.execute(
                "DELETE FROM credential_bundles WHERE account_id=?", (account["account_id"],)
            )
            connection.execute(
                "UPDATE incidents SET state='CLOSED',resolved_at_ms=?,resolution_reason='administratively_closed' WHERE scope_key=? AND state='OPEN'",
                (now, account["account_id"]),
            )

        await self.database.transaction(work)
        self.events.publish("account.updated", {"resource_id": public, "deleted": True})

    async def _commit_login(
        self,
        account: dict[str, Any],
        operation_id: str,
        attempt_id: str,
        method: LoginMethod,
        identity: dict[str, Any],
        export_available: bool,
        export_error: str | None,
    ) -> None:
        now = self.clock.now_ms()
        account_info = verify_identity(account, identity)
        email = account_info.get("email")
        plan = account_info.get("planType")

        def work(connection: sqlite3.Connection) -> None:
            changed = connection.execute(
                "UPDATE login_attempts SET state='COMPLETED',observed_email=?,observed_plan_type=?,oauth_completed_at_ms=?,completed_at_ms=?,updated_at_ms=? WHERE login_attempt_id=? AND state='CHECKPOINTING_CREDENTIAL'",
                (email, plan, now, now, now, attempt_id),
            ).rowcount
            if not changed:
                raise Conflict("LOGIN_CANCELLED", "Sign-in did not own the credential checkpoint")
            connection.execute(
                "UPDATE accounts SET enabled=1,lifecycle_state='ACTIVE',last_successful_login_method=?,updated_at_ms=? WHERE account_id=?",
                (method.value, now, account["account_id"]),
            )
            connection.execute(
                "UPDATE account_state SET auth_state='VERIFIED',worker_state='STOPPED',overall_state='WARNING',upstream_email=COALESCE(?,upstream_email),upstream_plan=COALESCE(?,upstream_plan),last_auth_verified_at_ms=?,last_error_code=NULL,last_error_summary=NULL,state_version=state_version+1,updated_at_ms=? WHERE account_id=?",
                (email, plan, now, now, account["account_id"]),
            )
            connection.execute(
                "UPDATE operations SET state='SUCCEEDED',progress_code=?,progress_summary=?,error_code=?,error_summary=?,completed_at_ms=?,state_version=state_version+1 WHERE operation_id=?",
                (
                    export_error or "COMPLETED",
                    "Sign-in completed; export snapshot is unavailable"
                    if export_error
                    else "Sign-in completed and credentials were checkpointed",
                    export_error,
                    "The managed credential is safe; reauthenticate to retry export creation"
                    if export_error
                    else None,
                    now,
                    operation_id,
                ),
            )

        await self.database.transaction(work)
        await self.resolve_incident(account["account_id"], "authentication_failed")
        self.events.publish(
            "login.updated",
            {
                "attempt_id": attempt_id,
                "account_id": account["public_token"],
                "state": "COMPLETED",
                "export_available": export_available,
            },
        )

    async def _login_state(
        self, attempt_id: str, state: str, *, expected: str | None = None
    ) -> bool:
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> bool:
            if expected is None:
                changed = connection.execute(
                    "UPDATE login_attempts SET state=?,updated_at_ms=? WHERE login_attempt_id=?",
                    (state, now, attempt_id),
                ).rowcount
            else:
                changed = connection.execute(
                    "UPDATE login_attempts SET state=?,updated_at_ms=? WHERE login_attempt_id=? AND state=?",
                    (state, now, attempt_id, expected),
                ).rowcount
            return bool(changed)

        return await self.database.transaction(work)

    async def _activation_state(self, activation_id: str, state: str) -> None:
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "UPDATE activation_attempts SET state=?,updated_at_ms=?,state_version=state_version+1 WHERE activation_id=?",
                (state, now, activation_id),
            )

        await self.database.transaction(work)

    async def _operation_state(
        self, operation_id: str, state: str, code: str, summary: str
    ) -> None:
        now = self.clock.now_ms()

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "UPDATE operations SET state=?,progress_code=?,progress_summary=?,started_at_ms=COALESCE(started_at_ms,?),completed_at_ms=CASE WHEN ? IN('SUCCEEDED','FAILED','CANCELLED') THEN ? ELSE completed_at_ms END,state_version=state_version+1 WHERE operation_id=?",
                (state, code, summary, now, state, now, operation_id),
            )

        await self.database.transaction(work)
        self.events.publish("operation.updated", {"operation_id": operation_id, "state": state})

    async def _fail_operation(self, operation_id: str, code: str, summary: str) -> None:
        now = self.clock.now_ms()
        safe_summary = str(redact(summary))[:200]

        def work(connection: sqlite3.Connection) -> None:
            connection.execute(
                "UPDATE operations SET state='FAILED',error_code=?,error_summary=?,completed_at_ms=?,state_version=state_version+1 WHERE operation_id=?",
                (code, safe_summary, now, operation_id),
            )

        await self.database.transaction(work)
        self.events.publish(
            "operation.updated",
            {"operation_id": operation_id, "state": "FAILED", "error_code": code},
        )

    async def _fail_login(
        self, attempt_id: str, operation_id: str, state: str, code: str, summary: str
    ) -> None:
        self.interactions.pop(attempt_id, None)
        now = self.clock.now_ms()
        safe_summary = str(redact(summary))[:200]

        def work(connection: sqlite3.Connection) -> str | None:
            row = connection.execute(
                "SELECT l.account_id,s.auth_state FROM login_attempts l JOIN account_state s USING(account_id) WHERE l.login_attempt_id=?",
                (attempt_id,),
            ).fetchone()
            changed = connection.execute(
                "UPDATE login_attempts SET state=?,error_code=?,error_summary=?,completed_at_ms=?,updated_at_ms=? WHERE login_attempt_id=? AND state NOT IN('COMPLETED','CANCELLED','EXPIRED','FAILED_RETRYABLE','FAILED_ACTION_REQUIRED','RESTART_REQUIRED','SUPERSEDED')",
                (state, code, safe_summary, now, now, attempt_id),
            ).rowcount
            connection.execute(
                "UPDATE operations SET state='FAILED',error_code=?,error_summary=?,completed_at_ms=?,state_version=state_version+1 WHERE operation_id=? AND state NOT IN('SUCCEEDED','FAILED','CANCELLED')",
                (code, safe_summary, now, operation_id),
            )
            if not changed or not row:
                return None
            account_id = str(row[0])
            has_active_credential = connection.execute(
                "SELECT 1 FROM credential_bundles WHERE account_id=? AND state='ACTIVE'",
                (account_id,),
            ).fetchone()
            remains_verified = bool(has_active_credential and row[1] == "VERIFIED")
            connection.execute(
                "UPDATE account_state SET auth_state=?,overall_state=?,last_error_code=?,last_error_summary=?,state_version=state_version+1,updated_at_ms=? WHERE account_id=?",
                (
                    "VERIFIED" if remains_verified else "AUTH_REQUIRED",
                    "WARNING" if remains_verified else "ACTION_REQUIRED",
                    code,
                    safe_summary,
                    now,
                    account_id,
                ),
            )
            return account_id

        if account_id := await self.database.transaction(work):
            self.events.publish(
                "login.updated", {"attempt_id": attempt_id, "state": state, "error_code": code}
            )
            await self.open_incident(account_id, "authentication_failed", "ERROR", safe_summary)
