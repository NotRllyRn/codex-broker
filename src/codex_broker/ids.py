import secrets
import time


def new_id() -> str:
    return f"{time.time_ns():016x}{secrets.token_hex(8)}"


def public_token() -> str:
    return secrets.token_urlsafe(18)
