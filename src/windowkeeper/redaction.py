import re
from collections.abc import Mapping, Sequence
from typing import Any
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit

DENIED_KEYS = {
    "authorization",
    "password",
    "token",
    "access_token",
    "refresh_token",
    "id_token",
    "csrf",
    "session",
    "authurl",
    "auth_url",
    "verificationurl",
    "verification_url",
    "usercode",
    "user_code",
    "callbackurl",
    "callback_url",
    "code",
    "state",
    "code_verifier",
    "code_challenge",
}
TOKEN_PATTERN = re.compile(r"(?i)(bearer\s+)?(?:sk-[A-Za-z0-9_-]{16,}|wk1_[A-Za-z0-9_-]{20,})")
URL_PATTERN = re.compile(r"https?://[^\s<>\"']+", re.IGNORECASE)


def sanitize_url(value: str) -> str:
    try:
        parts = urlsplit(value)
    except ValueError:
        return "[REDACTED_URL]"
    if not parts.scheme or not parts.netloc:
        return TOKEN_PATTERN.sub("[REDACTED]", value)
    query = urlencode(
        [(key, "[REDACTED]") for key, _ in parse_qsl(parts.query, keep_blank_values=True)]
    )
    return urlunsplit((parts.scheme, parts.netloc, parts.path, query, ""))


def redact(value: Any, known_secrets: Sequence[str] = ()) -> Any:
    if isinstance(value, Mapping):
        return {
            str(key): "[REDACTED]"
            if str(key).lower() in DENIED_KEYS
            else redact(item, known_secrets)
            for key, item in value.items()
        }
    if isinstance(value, (list, tuple)):
        return [redact(item, known_secrets) for item in value]
    if isinstance(value, str):
        result = value.replace("\r", " ").replace("\n", " ")[:8192]
        for secret in known_secrets:
            if secret:
                result = result.replace(secret, "[REDACTED]")
        result = TOKEN_PATTERN.sub("[REDACTED]", result)
        return URL_PATTERN.sub(lambda match: sanitize_url(match.group(0)), result)
    return value
