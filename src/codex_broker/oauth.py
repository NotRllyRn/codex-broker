import hashlib
import hmac
from dataclasses import dataclass
from urllib.parse import parse_qs, urlsplit

from codex_broker.errors import BrokerError


@dataclass(frozen=True, slots=True)
class BrowserContract:
    scheme: str
    host: str
    port: int
    path: str
    state_hash: bytes


def browser_contract(authorization_url: str, allowed_ports: tuple[int, ...]) -> BrowserContract:
    try:
        auth = urlsplit(authorization_url)
        query = parse_qs(auth.query, strict_parsing=True)
        redirects = query.get("redirect_uri", [])
        states = query.get("state", [])
        if auth.scheme != "https" or auth.username or auth.password or auth.fragment:
            raise ValueError("unsafe authorization URL")
        if len(redirects) != 1 or len(states) != 1:
            raise ValueError("missing redirect contract")
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
        raise BrokerError(
            "CODEX_BROWSER_AUTH_CONTRACT_CHANGED",
            "Codex returned an unsupported browser sign-in contract",
            409,
        ) from error
    assert redirect.hostname and redirect.port
    return BrowserContract(
        "http",
        redirect.hostname,
        redirect.port,
        redirect.path,
        hashlib.sha256(states[0].encode()).digest(),
    )


def validate_callback(value: str, contract: BrowserContract) -> str:
    if len(value.encode()) > 16_384:
        raise BrokerError("BROWSER_CALLBACK_INVALID", "The callback URL is too large")
    try:
        callback = urlsplit(value)
        query = parse_qs(callback.query, strict_parsing=True)
        code = query.get("code", [])
        state = query.get("state", [])
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
        raise BrokerError(
            "BROWSER_CALLBACK_INVALID", "The callback URL is not valid", 400
        ) from error
    if not hmac.compare_digest(hashlib.sha256(state[0].encode()).digest(), contract.state_hash):
        raise BrokerError(
            "BROWSER_CALLBACK_STATE_MISMATCH", "The callback belongs to another sign-in", 409
        )
    return f"{contract.scheme}://{contract.host}:{contract.port}{contract.path}?code={code[0]}&state={state[0]}"
