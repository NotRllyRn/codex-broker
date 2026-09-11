# pyright: reportMissingImports=false

import base64
import json
import secrets
from dataclasses import dataclass
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

import httpx
from fastapi import FastAPI, Request
from fastapi.responses import HTMLResponse, JSONResponse, Response
from fastapi.staticfiles import StaticFiles
from jinja2 import Environment, FileSystemLoader, select_autoescape

from codex_broker.config import Settings, get_settings
from codex_broker.web.app import LoginThrottle

COOKIE = "cb_public_enrollment"


@dataclass(frozen=True, slots=True)
class EnrollmentHandle:
    enrollment_id: str
    login_attempt_id: str
    session_token: str
    interaction_nonce: str

    def encode(self) -> str:
        payload = {
            "enrollment_id": self.enrollment_id,
            "login_attempt_id": self.login_attempt_id,
            "session_token": self.session_token,
            "interaction_nonce": self.interaction_nonce,
        }
        return (
            base64.urlsafe_b64encode(json.dumps(payload, separators=(",", ":")).encode())
            .rstrip(b"=")
            .decode()
        )

    @classmethod
    def decode(cls, value: str) -> "EnrollmentHandle | None":
        try:
            payload = json.loads(base64.urlsafe_b64decode(value + "=" * (-len(value) % 4)))
            handle = cls(**payload)
            return handle if all(32 <= len(item) <= 200 for item in payload.values()) else None
        except (ValueError, TypeError, json.JSONDecodeError):
            return None

    def status_body(self) -> dict[str, str]:
        return {
            "enrollment_id": self.enrollment_id,
            "login_attempt_id": self.login_attempt_id,
            "session_token": self.session_token,
            "interaction_nonce": self.interaction_nonce,
        }


def _headers(response: Response) -> Response:
    response.headers.update(
        {
            "Cache-Control": "no-store, max-age=0",
            "Pragma": "no-cache",
            "Content-Security-Policy": "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'",
            "Strict-Transport-Security": "max-age=31536000; includeSubDomains",
            "X-Content-Type-Options": "nosniff",
            "Referrer-Policy": "no-referrer",
            "Permissions-Policy": "camera=(), microphone=(), geolocation=()",
        }
    )
    return response


def create_public_app(
    settings: Settings | None = None, transport: httpx.AsyncBaseTransport | None = None
) -> FastAPI:
    resolved = settings or get_settings()
    templates = Environment(
        loader=FileSystemLoader(Path(__file__).parent / "public_templates"),
        autoescape=select_autoescape(("html", "xml")),
    )
    throttle = LoginThrottle(resolved.public_enrollment_attempts_per_hour, 3600)
    status_throttle = LoginThrottle(120, 60)
    app = FastAPI(title="Codex sign-in", docs_url=None, redoc_url=None, openapi_url=None)
    app.mount(
        "/static",
        StaticFiles(directory=Path(__file__).parent / "public_static"),
        name="static",
    )

    async def broker(path: str, body: dict[str, str]) -> dict[str, Any]:
        key = resolved.public_enrollment_key
        if not resolved.public_enrollment_enabled or key is None:
            raise RuntimeError("public enrollment is not configured")
        verify: bool | str = (
            str(resolved.public_enrollment_ca_cert) if resolved.public_enrollment_ca_cert else True
        )
        async with httpx.AsyncClient(
            base_url=resolved.public_enrollment_broker_url,
            headers={"Authorization": f"Bearer {key.get_secret_value()}"},
            verify=verify,
            transport=transport,
            trust_env=False,
            timeout=httpx.Timeout(10, connect=3),
        ) as client:
            response = await client.post(path, json=body)
        if response.status_code >= 400:
            raise RuntimeError("broker rejected public enrollment")
        payload = response.json()
        if not isinstance(payload, dict):
            raise RuntimeError("broker returned an invalid response")
        return payload

    def render(error: str | None = None) -> HTMLResponse:
        return HTMLResponse(templates.get_template("enroll.html").render(error=error))

    @app.middleware("http")
    async def security_headers(_request: Request, call_next: Any) -> Response:
        return _headers(await call_next(_request))

    @app.get("/health/live")
    async def live() -> dict[str, str]:
        return {"status": "ok"}

    @app.get("/", response_class=HTMLResponse)
    async def enrollment_page() -> Response:
        return render()

    @app.post("/start")
    async def enrollment_start(request: Request) -> JSONResponse:
        if EnrollmentHandle.decode(request.cookies.get(COOKIE, "")):
            return JSONResponse({"state": "STARTING"})
        client = request.client.host if request.client else "unknown"
        if not throttle.allow(client):
            return JSONResponse({"state": "FAILED"}, status_code=429)
        session_token = secrets.token_urlsafe(32)
        try:
            started = await broker(
                "/api/private/v1/public-enrollments",
                {"session_token": session_token},
            )
            handle = EnrollmentHandle(
                str(started["enrollment_id"]),
                str(started["login_attempt_id"]),
                session_token,
                str(started["interaction_nonce"]),
            )
        except (httpx.HTTPError, KeyError, RuntimeError, ValueError, TypeError):
            return JSONResponse({"state": "UNAVAILABLE"}, status_code=503)
        response = JSONResponse({"state": "STARTING"})
        response.set_cookie(
            COOKIE,
            handle.encode(),
            httponly=True,
            secure=True,
            samesite="strict",
            path="/",
            max_age=resolved.login_timeout_seconds,
        )
        return response

    @app.get("/status")
    async def enrollment_status(request: Request) -> JSONResponse:
        client = request.client.host if request.client else "unknown"
        if not status_throttle.allow(client):
            return JSONResponse({"state": "UNAVAILABLE"}, status_code=429)
        handle = EnrollmentHandle.decode(request.cookies.get(COOKIE, ""))
        if handle is None:
            return JSONResponse({"state": "FAILED"}, status_code=404)
        try:
            status = await broker(
                "/api/private/v1/public-enrollments/status",
                handle.status_body(),
            )
            if status.get("state") == "WAITING_FOR_USER":
                verification_url = str(status.get("verification_url") or "")
                parsed = urlsplit(verification_url)
                if parsed.scheme != "https" or parsed.netloc != "auth.openai.com":
                    raise RuntimeError("unsafe verification URL")
                status = {
                    "state": "WAITING_FOR_USER",
                    "verification_url": verification_url,
                    "user_code": str(status.get("user_code") or ""),
                    "expires_at_ms": status.get("expires_at_ms"),
                }
            elif status.get("state") == "COMPLETED":
                status = {"state": "COMPLETED", "email": str(status.get("email") or "")}
            else:
                status = {"state": status.get("state", "FAILED")}
            response = JSONResponse(status)
            if status["state"] in {"COMPLETED", "FAILED"}:
                response.delete_cookie(COOKIE, path="/", secure=True, httponly=True)
            return response
        except (httpx.HTTPError, RuntimeError, ValueError, TypeError):
            return JSONResponse({"state": "UNAVAILABLE"}, status_code=503)

    return app


app = create_public_app()
