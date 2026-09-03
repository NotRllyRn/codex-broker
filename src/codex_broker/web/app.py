# pyright: reportMissingImports=false

import json
import os
import stat
import time
from collections import defaultdict, deque
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Any, cast

from fastapi import FastAPI, Form, Header, Request
from fastapi.exceptions import HTTPException
from fastapi.responses import (
    HTMLResponse,
    JSONResponse,
    RedirectResponse,
    Response,
    StreamingResponse,
)
from fastapi.staticfiles import StaticFiles
from jinja2 import Environment, FileSystemLoader, select_autoescape

from codex_broker.compatibility import Compatibility, inspect_codex
from codex_broker.config import Settings, get_settings
from codex_broker.database import Database
from codex_broker.domain.models import LoginMethod
from codex_broker.errors import WindowkeeperError
from codex_broker.events import Broadcaster
from codex_broker.logbook import LogBook, configure_logging
from codex_broker.runtime import RuntimeManager
from codex_broker.security import AdminSecurity, digest
from codex_broker.services import ApplicationServices
from codex_broker.singleton import SingletonLock
from codex_broker.vault import Vault, decode_key
from codex_broker.views import account_view
from codex_broker.webhooks import WebhookDispatcher

SESSION_COOKIE = "wk_session"
CSRF_COOKIE = "wk_csrf"
VARIANTS = {
    "orbit": "Orbit cockpit",
    "ledger": "Evidence ledger",
    "rail": "Command rail",
    "timeline": "Reset timeline",
    "focus": "Account focus",
}


class LoginThrottle:
    def __init__(self, attempts: int = 5, window_seconds: int = 60) -> None:
        self.attempts = attempts
        self.window_seconds = window_seconds
        self._attempts: dict[str, deque[float]] = defaultdict(deque)

    def allow(self, key: str) -> bool:
        now = time.monotonic()
        bucket = self._attempts[key]
        while bucket and now - bucket[0] > self.window_seconds:
            bucket.popleft()
        if len(bucket) >= self.attempts:
            return False
        bucket.append(now)
        return True

    def clear(self, key: str) -> None:
        self._attempts.pop(key, None)


@dataclass(slots=True)
class AppState:
    settings: Settings
    database: Database
    security: AdminSecurity
    services: ApplicationServices
    events: Broadcaster
    runtime: RuntimeManager
    webhooks: WebhookDispatcher
    lock: SingletonLock
    logbook: LogBook
    vault_configured: bool
    compatibility: Compatibility
    ready: bool = False


def _read_secret(path: Path | None, value: str | None) -> str | None:
    if not path:
        return value
    try:
        metadata = path.lstat()
        if path.is_symlink() or not stat.S_ISREG(metadata.st_mode) or metadata.st_mode & 0o077:
            raise RuntimeError(f"secret file is not a protected regular file: {path.name}")
        return path.read_text(encoding="utf-8").strip()
    except OSError as error:
        raise RuntimeError(f"secret file could not be read: {path.name}") from error


def _templates() -> Environment:
    return Environment(
        loader=FileSystemLoader(Path(__file__).parent / "templates"),
        autoescape=select_autoescape(("html", "xml")),
        enable_async=False,
    )


def problem(error: WindowkeeperError, request: Request) -> JSONResponse:
    return JSONResponse(
        {
            "type": f"urn:windowkeeper:problem:{error.code.lower().replace('_', '-')}",
            "title": error.code.replace("_", " ").title(),
            "status": error.status,
            "detail": error.detail,
            "instance": request.url.path,
            "code": error.code,
        },
        status_code=error.status,
        media_type="application/problem+json",
    )


def _security_headers(response: Response, no_store: bool = False) -> Response:
    response.headers.update(
        {
            "Content-Security-Policy": "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'",
            "X-Content-Type-Options": "nosniff",
            "Referrer-Policy": "no-referrer",
            "Permissions-Policy": "camera=(), microphone=(), geolocation=()",
        }
    )
    if no_store:
        response.headers.update({"Cache-Control": "no-store, max-age=0", "Pragma": "no-cache"})
    return response


@asynccontextmanager
async def _lifespan(app: FastAPI) -> AsyncIterator[None]:
    settings: Settings = app.state.bootstrap_settings
    settings.data_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    settings.runtime_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    lock = SingletonLock(settings.data_dir / "windowkeeper.lock")
    database: Database | None = None
    runtime: RuntimeManager | None = None
    webhooks: WebhookDispatcher | None = None
    services: ApplicationServices | None = None
    logbook: LogBook | None = None
    state: AppState | None = None
    lock.acquire()
    try:
        database = Database(settings.data_dir / "windowkeeper.db")
        database.start()
        instance = await database.call(
            lambda connection: str(
                connection.execute(
                    "SELECT instance_uuid FROM instance_metadata WHERE singleton_id=1"
                ).fetchone()[0]
            )
        )
        encoded_key = _read_secret(settings.vault_key_file, settings.vault_key)
        vault_configured = bool(encoded_key)
        root_key = decode_key(encoded_key) if encoded_key else os.urandom(32)
        vault = Vault(root_key, instance)
        if vault_configured:
            sentinel_value = f"windowkeeper:{instance}"

            def verify_vault(connection: Any) -> None:
                row = connection.execute(
                    "SELECT sentinel_ciphertext FROM vault_state WHERE singleton_id=1"
                ).fetchone()
                if row:
                    try:
                        valid = vault.open_text(bytes(row[0])) == sentinel_value
                    except Exception as error:
                        raise RuntimeError(
                            "configured vault key does not match this instance"
                        ) from error
                    if not valid:
                        raise RuntimeError("configured vault key does not match this instance")
                    return
                sealed = vault.seal_text("vault-sentinel", sentinel_value)
                connection.execute(
                    "INSERT INTO vault_state VALUES(1,?,?,?,?)",
                    (vault.key_id, os.urandom(12), sealed, int(time.time() * 1000)),
                )

            await database.transaction(verify_vault)
        compatibility = inspect_codex(settings)
        if compatibility.observed_version:
            settings.codex_version = compatibility.observed_version
        events = Broadcaster()
        runtime = RuntimeManager(settings, vault)
        webhooks = WebhookDispatcher(database, vault)
        services = ApplicationServices(database, settings, vault, runtime, events, webhooks)
        security = AdminSecurity(database, settings)
        admin_password = _read_secret(settings.admin_password_file, settings.admin_password)
        if admin_password:
            await security.bootstrap(admin_password)
        admin_configured = await security.configured()
        logbook = configure_logging(
            settings.log_dir or settings.data_dir / "logs", settings.log_level
        )
        state = AppState(
            settings,
            database,
            security,
            services,
            events,
            runtime,
            webhooks,
            lock,
            logbook,
            vault_configured,
            compatibility,
        )
        app.state.windowkeeper = state
        await services.reconcile_startup()
        state.ready = vault_configured and admin_configured and compatibility.compatible
        if state.ready:
            services.start_background()
            webhooks.start()
        yield
    finally:
        if state:
            state.ready = False
        if services:
            await services.close()
        if webhooks:
            await webhooks.close()
        if runtime:
            await runtime.close()
        if database:
            await database.close()
        if logbook:
            logbook.close()
        lock.release()


def create_app(settings: Settings | None = None) -> FastAPI:
    resolved = settings or get_settings()
    templates = _templates()
    templates.globals["root_path"] = resolved.root_path

    def render(name: str, **context: Any) -> HTMLResponse:
        return HTMLResponse(templates.get_template(name).render(**context))

    login_throttle = LoginThrottle()
    reauth_throttle = LoginThrottle()
    app = FastAPI(
        title="Windowkeeper",
        docs_url=None,
        redoc_url=None,
        openapi_url=None,
        root_path=resolved.root_path,
        lifespan=_lifespan,
    )
    app.state.bootstrap_settings = resolved
    app.mount("/static", StaticFiles(directory=Path(__file__).parent / "static"), name="static")

    @app.middleware("http")
    async def headers(request: Request, call_next: Any) -> Response:
        response = await call_next(request)
        location = response.headers.get("location")
        if (
            resolved.root_path
            and location
            and location.startswith("/")
            and location != resolved.root_path
            and not location.startswith(f"{resolved.root_path}/")
        ):
            response.headers["location"] = f"{resolved.root_path}{location}"
        return _security_headers(response)

    @app.exception_handler(WindowkeeperError)
    async def handle_problem(request: Request, error: WindowkeeperError) -> JSONResponse:
        return problem(error, request)

    def state(request: Request) -> AppState:
        return cast(AppState, request.app.state.windowkeeper)

    async def session_or_none(request: Request) -> dict[str, object] | None:
        return await state(request).security.session(request.cookies.get(SESSION_COOKIE))

    async def require_session(request: Request) -> dict[str, object]:
        current = await session_or_none(request)
        if not current:
            raise HTTPException(401, "administrator login required")
        return current

    async def require_form(request: Request, csrf: str | None) -> str:
        token = request.cookies.get(SESSION_COOKIE)
        if not token:
            raise HTTPException(401, "administrator login required")
        await state(request).security.require_csrf(token, csrf)
        return token

    async def require_reauthentication(request: Request, token: str, password: str) -> None:
        key = digest(token).hex()
        if not reauth_throttle.allow(key):
            raise WindowkeeperError(
                "REAUTH_THROTTLED", "Too many password attempts; retry in one minute", 429
            )
        await state(request).security.reauthenticate(token, password)
        reauth_throttle.clear(key)

    @app.get("/health/live")
    async def live() -> dict[str, str]:
        return {"status": "ok"}

    @app.get("/health/ready")
    async def ready(request: Request) -> JSONResponse:
        current = state(request)
        status = "ok" if current.ready else "unavailable"
        return JSONResponse({"status": status}, status_code=200 if current.ready else 503)

    @app.get("/login", response_class=HTMLResponse)
    async def login_page(request: Request) -> Response:
        if await session_or_none(request):
            return RedirectResponse("/", 303)
        return render("login.html", error=None)

    @app.post("/login")
    async def login(request: Request, password: str = Form()) -> Response:
        current = state(request)
        client_key = request.client.host if request.client else "unknown"
        if not login_throttle.allow(client_key):
            throttled_response = render(
                "login.html", error="Login temporarily unavailable. Wait one minute and retry."
            )
            throttled_response.status_code = 429
            return _security_headers(throttled_response, no_store=True)
        try:
            created = await current.security.login(
                password, request.headers.get("user-agent", "")[:200]
            )
        except WindowkeeperError as error:
            return _security_headers(render("login.html", error=error.detail), no_store=True)
        login_throttle.clear(client_key)
        response = RedirectResponse("/", 303)
        secure = current.settings.cookie_secure == "true" or (
            current.settings.cookie_secure == "auto" and request.url.scheme == "https"
        )
        response.set_cookie(
            SESSION_COOKIE,
            created.token,
            httponly=True,
            secure=secure,
            samesite="lax",
            path=current.settings.root_path or "/",
        )
        response.set_cookie(
            CSRF_COOKIE,
            created.csrf_token,
            httponly=False,
            secure=secure,
            samesite="lax",
            path=current.settings.root_path or "/",
        )
        return _security_headers(response, no_store=True)

    @app.post("/logout")
    async def logout(request: Request, csrf_token: str = Form()) -> Response:
        token = await require_form(request, csrf_token)
        await state(request).security.logout(token)
        response = RedirectResponse("/login", 303)
        response.delete_cookie(SESSION_COOKIE)
        response.delete_cookie(CSRF_COOKIE)
        return response

    @app.get("/", response_class=HTMLResponse)
    async def dashboard(
        request: Request, variant: str = "orbit", state_filter: str = "", q: str = ""
    ) -> Response:
        if not await session_or_none(request):
            return RedirectResponse("/login", 303)
        selected = variant if variant in VARIANTS else "orbit"
        accounts = await state(request).services.accounts()
        if state_filter:
            accounts = [account for account in accounts if account.overall_state == state_filter]
        if q:
            accounts = [
                account for account in accounts if q.lower() in account.display_name.lower()
            ]
        views = [account_view(account) for account in accounts]
        counts = {
            status: sum(account.overall_state == status for account in accounts)
            for status in ("HEALTHY", "WARNING", "ACTION_REQUIRED", "ERROR", "DISABLED")
        }
        return render(
            "dashboard.html",
            request=request,
            accounts=views,
            counts=counts,
            variant=selected,
            variants=VARIANTS,
            csrf=request.cookies.get(CSRF_COOKIE, ""),
            vault_configured=state(request).vault_configured,
            dev=os.environ.get("WINDOWKEEPER_ENV", "development") != "production",
            q=q,
            state_filter=state_filter,
            selected_states={
                status: state_filter == status
                for status in ("HEALTHY", "WARNING", "ACTION_REQUIRED", "ERROR", "DISABLED")
            },
        )

    @app.get("/accounts/new", response_class=HTMLResponse)
    async def account_new(request: Request) -> Response:
        await require_session(request)
        return render(
            "account_new.html",
            csrf=request.cookies.get(CSRF_COOKIE, ""),
            vault_configured=state(request).vault_configured,
        )

    @app.post("/accounts")
    async def account_create(
        request: Request,
        display_name: str = Form(),
        labels: str = Form(""),
        workspace: str = Form(""),
        login_method: str = Form("CHATGPT_DEVICE_CODE"),
        admin_password: str = Form(),
        csrf_token: str = Form(),
    ) -> Response:
        token = await require_form(request, csrf_token)
        current = state(request)
        await require_reauthentication(request, token, admin_password)
        if not current.vault_configured:
            raise WindowkeeperError(
                "VAULT_KEY_REQUIRED", "Configure the vault key before adding accounts", 503
            )
        if not current.compatibility.compatible:
            raise WindowkeeperError(current.compatibility.code, current.compatibility.detail, 503)
        method = LoginMethod(login_method)
        if method == LoginMethod.MANUAL_TOKENS:
            raise WindowkeeperError(
                "LOGIN_METHOD_UNAVAILABLE",
                "Manual token import is retired; use device code or browser sign-in",
                409,
            )
        account = await current.services.create_account(
            display_name,
            labels=[item.strip() for item in labels.split(",")],
            workspace=workspace.strip() or None,
            method=method,
        )
        started = await current.services.start_login(account["public_token"], method, token)
        return _security_headers(
            render(
                "login_progress.html",
                account=account,
                started=started,
                method=method.value,
                csrf=csrf_token,
            ),
            no_store=True,
        )

    @app.get("/accounts/{public}", response_class=HTMLResponse)
    async def account_detail(request: Request, public: str) -> Response:
        await require_session(request)
        detail = await state(request).services.account_detail(public)
        return render(
            "account_detail.html", detail=detail, csrf=request.cookies.get(CSRF_COOKIE, "")
        )

    @app.post("/accounts/{public}/auth-export")
    async def auth_export(
        request: Request,
        public: str,
        admin_password: str = Form(),
        csrf_token: str = Form(),
    ) -> Response:
        token = await require_form(request, csrf_token)
        await require_reauthentication(request, token, admin_password)
        content = await state(request).services.export_auth_json(public)
        return _security_headers(
            Response(
                content=content,
                media_type="application/json",
                headers={"Content-Disposition": 'attachment; filename="auth.json"'},
            ),
            no_store=True,
        )

    @app.post("/accounts/{public}/refresh")
    async def refresh(request: Request, public: str, csrf_token: str = Form()) -> Response:
        await require_form(request, csrf_token)
        operation = await state(request).services.refresh(public)
        return RedirectResponse(f"/operations/{operation}", 303)

    @app.post("/accounts/{public}/activate")
    async def activate(request: Request, public: str, csrf_token: str = Form()) -> Response:
        await require_form(request, csrf_token)
        operation = await state(request).services.activate(public)
        return RedirectResponse(f"/operations/{operation}", 303)

    @app.post("/accounts/{public}/ambiguity/acknowledge")
    async def acknowledge_ambiguity(
        request: Request,
        public: str,
        admin_password: str = Form(),
        csrf_token: str = Form(),
    ) -> Response:
        token = await require_form(request, csrf_token)
        await require_reauthentication(request, token, admin_password)
        await state(request).services.acknowledge_ambiguity(public)
        return RedirectResponse(f"/accounts/{public}", 303)

    @app.post("/accounts/{public}/reauthenticate")
    async def reauthenticate(
        request: Request,
        public: str,
        login_method: str = Form(),
        admin_password: str = Form(),
        csrf_token: str = Form(),
    ) -> Response:
        token = await require_form(request, csrf_token)
        current = state(request)
        await require_reauthentication(request, token, admin_password)
        if not current.vault_configured:
            raise WindowkeeperError(
                "VAULT_KEY_REQUIRED", "Configure the vault key before signing in", 503
            )
        if not current.compatibility.compatible:
            raise WindowkeeperError(current.compatibility.code, current.compatibility.detail, 503)
        account = (await current.services.account_detail(public))["account"]
        method = LoginMethod(login_method)
        if method == LoginMethod.MANUAL_TOKENS:
            raise WindowkeeperError(
                "LOGIN_METHOD_UNAVAILABLE",
                "Manual token import is retired; use device code or browser sign-in",
                409,
            )
        started = await current.services.start_login(public, method, token, recover_checkpoint=True)
        return _security_headers(
            render(
                "login_progress.html",
                account=account,
                started=started,
                method=method.value,
                csrf=csrf_token,
            ),
            no_store=True,
        )

    @app.post("/accounts/{public}/labels")
    async def labels(
        request: Request,
        public: str,
        labels: str = Form(""),
        csrf_token: str = Form(),
    ) -> Response:
        await require_form(request, csrf_token)
        await state(request).services.set_labels(public, labels.split(","))
        return RedirectResponse(f"/accounts/{public}", 303)

    @app.post("/accounts/{public}/enabled")
    async def enabled(
        request: Request, public: str, enabled: bool = Form(), csrf_token: str = Form()
    ) -> Response:
        await require_form(request, csrf_token)
        await state(request).services.set_enabled(public, enabled)
        return RedirectResponse(f"/accounts/{public}", 303)

    @app.post("/accounts/{public}/delete")
    async def delete(
        request: Request,
        public: str,
        confirmation: str = Form(),
        admin_password: str = Form(),
        csrf_token: str = Form(),
    ) -> Response:
        token = await require_form(request, csrf_token)
        await require_reauthentication(request, token, admin_password)
        await state(request).services.delete_account(public, confirmation)
        return RedirectResponse("/", 303)

    @app.get("/operations/{operation_id}", response_class=HTMLResponse)
    async def operation(request: Request, operation_id: str) -> Response:
        await require_session(request)
        value = await state(request).services.operation(operation_id)
        try:
            value["result"] = json.loads(value["result_json"] or "{}")
        except (json.JSONDecodeError, TypeError):
            value["result"] = {}
        return render("operation.html", operation=value)

    @app.get("/incidents", response_class=HTMLResponse)
    async def incidents(request: Request) -> Response:
        await require_session(request)
        return render("incidents.html", incidents=await state(request).services.incidents())

    @app.get("/logs", response_class=HTMLResponse)
    async def logs(request: Request, level: str = "", q: str = "") -> Response:
        await require_session(request)
        return render(
            "logs.html",
            logs=state(request).logbook.recent(level=level or None, query=q),
            level=level,
            q=q,
        )

    @app.get("/logs/export")
    async def logs_export(request: Request, level: str = "", q: str = "") -> Response:
        await require_session(request)
        body = "\n".join(
            json.dumps(item, sort_keys=True, separators=(",", ":"))
            for item in state(request).logbook.recent(level=level or None, query=q, limit=2_000)
        )
        return _security_headers(
            Response(
                body + ("\n" if body else ""),
                media_type="application/x-ndjson",
                headers={"Content-Disposition": "attachment; filename=windowkeeper-logs.jsonl"},
            ),
            no_store=True,
        )

    @app.get("/settings", response_class=HTMLResponse)
    async def settings_page(request: Request) -> Response:
        await require_session(request)
        current = state(request)
        return render(
            "settings.html",
            settings=current.settings,
            vault_configured=current.vault_configured,
            compatibility=current.compatibility,
            destinations=await current.webhooks.destinations(),
            csrf=request.cookies.get(CSRF_COOKIE, ""),
        )

    @app.post("/settings/webhooks")
    async def webhook_create(
        request: Request,
        display_name: str = Form(),
        url: str = Form(),
        signing_secret: str = Form(""),
        kind: str = Form("generic"),
        admin_password: str = Form(),
        csrf_token: str = Form(),
    ) -> Response:
        token = await require_form(request, csrf_token)
        current = state(request)
        await require_reauthentication(request, token, admin_password)
        if not current.vault_configured:
            raise WindowkeeperError(
                "VAULT_KEY_REQUIRED", "Configure the vault key before adding webhooks", 503
            )
        await current.webhooks.create_destination(
            display_name, url, signing_secret.strip() or None, kind
        )
        return RedirectResponse("/settings", 303)

    @app.post("/settings/webhooks/{destination_id}/test")
    async def webhook_test(
        request: Request, destination_id: str, csrf_token: str = Form()
    ) -> Response:
        await require_form(request, csrf_token)
        await state(request).webhooks.test(destination_id)
        return RedirectResponse("/settings", 303)

    @app.post("/settings/webhooks/{destination_id}/enabled")
    async def webhook_enabled(
        request: Request,
        destination_id: str,
        enabled: bool = Form(),
        admin_password: str = Form(),
        csrf_token: str = Form(),
    ) -> Response:
        token = await require_form(request, csrf_token)
        await require_reauthentication(request, token, admin_password)
        await state(request).webhooks.set_enabled(destination_id, enabled)
        return RedirectResponse("/settings", 303)

    @app.post("/settings/webhooks/{destination_id}/delete")
    async def webhook_delete(
        request: Request,
        destination_id: str,
        admin_password: str = Form(),
        csrf_token: str = Form(),
    ) -> Response:
        token = await require_form(request, csrf_token)
        await require_reauthentication(request, token, admin_password)
        await state(request).webhooks.delete_destination(destination_id)
        return RedirectResponse("/settings", 303)

    @app.get("/api/internal/v1/dashboard")
    async def api_dashboard(request: Request) -> dict[str, Any]:
        await require_session(request)
        return {
            "api_version": "windowkeeper.dev/internal/v1",
            "kind": "Dashboard",
            "data": [account_view(account) for account in await state(request).services.accounts()],
        }

    @app.get("/api/internal/v1/operations/{operation_id}")
    async def api_operation(request: Request, operation_id: str) -> dict[str, Any]:
        await require_session(request)
        return {
            "api_version": "windowkeeper.dev/internal/v1",
            "kind": "Operation",
            "data": await state(request).services.operation(operation_id),
        }

    @app.get("/api/internal/v1/login-attempts/{attempt_id}/interaction")
    async def api_interaction(
        request: Request,
        attempt_id: str,
        x_interaction_nonce: str = Header(alias="X-Interaction-Nonce"),
    ) -> Response:
        await require_session(request)
        token = request.cookies.get(SESSION_COOKIE, "")
        value = await state(request).services.interaction(attempt_id, token, x_interaction_nonce)
        return _security_headers(
            JSONResponse(
                {
                    "api_version": "windowkeeper.dev/internal/v1",
                    "kind": "LoginInteraction",
                    "data": value,
                }
            ),
            no_store=True,
        )

    @app.post("/api/internal/v1/login-attempts/{attempt_id}/browser-callback")
    async def api_callback(
        request: Request,
        attempt_id: str,
        x_csrf_token: str = Header(alias="X-CSRF-Token"),
        x_interaction_nonce: str = Header(alias="X-Interaction-Nonce"),
    ) -> Response:
        token = await require_form(request, x_csrf_token)
        body = await request.json()
        callback_url = body.get("callback_url") if isinstance(body, dict) else None
        if not isinstance(callback_url, str):
            raise WindowkeeperError("BROWSER_CALLBACK_INVALID", "Provide the full callback URL")
        await state(request).services.forward_callback(
            attempt_id, token, x_interaction_nonce, callback_url
        )
        return _security_headers(
            JSONResponse(
                {"kind": "BrowserCallbackAccepted", "data": {"status": "forwarding"}},
                status_code=202,
            ),
            no_store=True,
        )

    @app.post("/api/internal/v1/login-attempts/{attempt_id}/cancel")
    async def api_cancel_login(
        request: Request,
        attempt_id: str,
        x_csrf_token: str = Header(alias="X-CSRF-Token"),
    ) -> JSONResponse:
        token = await require_form(request, x_csrf_token)
        operation_id = await state(request).services.cancel_login(attempt_id, token)
        return JSONResponse(
            {"kind": "Operation", "data": {"operation_id": operation_id}}, status_code=202
        )

    @app.get("/api/internal/v1/events/state")
    async def state_events(
        request: Request, last_event_id: str | None = Header(None)
    ) -> StreamingResponse:
        await require_session(request)
        try:
            sequence = int(last_event_id) if last_event_id else None
        except ValueError:
            sequence = None
        return StreamingResponse(
            state(request).events.subscribe(sequence),
            media_type="text/event-stream",
            headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"},
        )

    return app


app = create_app()
