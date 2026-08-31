import base64
import hashlib
import json
import os
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import Any

from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.kdf.hkdf import HKDF

from windowkeeper.ids import new_id

PREFIX = "wk1_"
LEGACY_ALLOWED_FILES = {"auth.json", "config.toml"}


def generate_key() -> str:
    return PREFIX + base64.urlsafe_b64encode(os.urandom(32)).rstrip(b"=").decode()


def decode_key(encoded: str) -> bytes:
    if not encoded.startswith(PREFIX):
        raise ValueError("vault key must use wk1_ format")
    value = encoded.removeprefix(PREFIX)
    try:
        raw = base64.urlsafe_b64decode(value + "=" * (-len(value) % 4))
    except (ValueError, TypeError) as error:
        raise ValueError("vault key is not valid base64url") from error
    if len(raw) != 32:
        raise ValueError("vault key must contain exactly 32 random bytes")
    return raw


@dataclass(frozen=True, slots=True)
class Envelope:
    bundle_id: str
    account_id: str
    key_id: str
    nonce: bytes
    ciphertext: bytes
    aad: bytes
    payload_schema_version: int = 1
    envelope_version: int = 1


class Vault:
    def __init__(self, root_key: bytes, instance_id: str, key_id: str = "primary") -> None:
        if len(root_key) != 32:
            raise ValueError("root key must be 32 bytes")
        self._root_key = root_key
        self.instance_id = instance_id
        self.key_id = key_id

    def _account_key(self, account_id: str, key_id: str) -> bytes:
        return HKDF(
            algorithm=hashes.SHA256(),
            length=32,
            salt=self.instance_id.encode(),
            info=f"windowkeeper/credential-bundle/v1:{key_id}:{account_id}".encode(),
        ).derive(self._root_key)

    def encrypt(
        self, account_id: str, payload: dict[str, Any], bundle_id: str | None = None
    ) -> Envelope:
        bundle_id = bundle_id or new_id()
        aad = json.dumps(
            {
                "instance_id": self.instance_id,
                "account_id": account_id,
                "bundle_id": bundle_id,
                "key_id": self.key_id,
                "envelope_version": 1,
                "payload_schema_version": 1,
            },
            sort_keys=True,
            separators=(",", ":"),
        ).encode()
        nonce = os.urandom(12)
        plaintext = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()
        ciphertext = AESGCM(self._account_key(account_id, self.key_id)).encrypt(
            nonce, plaintext, aad
        )
        return Envelope(bundle_id, account_id, self.key_id, nonce, ciphertext, aad)

    def decrypt(self, envelope: Envelope) -> dict[str, Any]:
        plaintext = AESGCM(self._account_key(envelope.account_id, envelope.key_id)).decrypt(
            envelope.nonce, envelope.ciphertext, envelope.aad
        )
        try:
            value = json.loads(plaintext)
        except json.JSONDecodeError as error:
            raise ValueError("credential payload is not valid JSON") from error
        if not isinstance(value, dict) or value.get("schema_version") != 1:
            raise ValueError("unsupported credential payload")
        return value

    def capture(
        self, codex_home: Path, codex_version: str, workspace: str | None = None
    ) -> dict[str, Any]:
        path = codex_home / "auth.json"
        if (
            not path.exists()
            or path.is_symlink()
            or not path.is_file()
            or path.stat().st_size > 2 * 1024 * 1024
        ):
            raise ValueError("unsafe or missing credential file: auth.json")
        content = path.read_bytes()
        try:
            value = json.loads(content)
        except json.JSONDecodeError as error:
            raise ValueError("auth.json is not valid JSON") from error
        if not isinstance(value, dict):
            raise ValueError("auth.json must contain an object")
        return {
            "schema_version": 1,
            "codex_version": codex_version,
            "files": [
                {
                    "relative_path": "auth.json",
                    "mode": 0o600,
                    "sha256": hashlib.sha256(content).hexdigest(),
                    "content_base64": base64.b64encode(content).decode(),
                }
            ],
            "workspace_constraint": workspace,
        }

    def auth_json(self, payload: dict[str, Any]) -> bytes:
        for item in payload.get("files", []):
            if item.get("relative_path") != "auth.json":
                continue
            content = base64.b64decode(item["content_base64"], validate=True)
            if hashlib.sha256(content).hexdigest() != item["sha256"]:
                raise ValueError("credential payload digest mismatch")
            try:
                value = json.loads(content)
            except json.JSONDecodeError as error:
                raise ValueError("auth.json is not valid JSON") from error
            if not isinstance(value, dict):
                raise ValueError("auth.json must contain an object")
            return content
        raise ValueError("credential bundle has no auth.json")

    def auth_fingerprint(self, payload: dict[str, Any]) -> str:
        self.auth_json(payload)
        for item in payload.get("files", []):
            if item.get("relative_path") == "auth.json":
                return str(item["sha256"])
        raise ValueError("credential bundle has no auth.json")

    def seal_text(self, scope: str, value: str) -> bytes:
        envelope = self.encrypt(scope, {"schema_version": 1, "value": value})
        return json.dumps(
            {
                "bundle_id": envelope.bundle_id,
                "account_id": envelope.account_id,
                "key_id": envelope.key_id,
                "nonce": base64.b64encode(envelope.nonce).decode(),
                "ciphertext": base64.b64encode(envelope.ciphertext).decode(),
                "aad": base64.b64encode(envelope.aad).decode(),
            },
            separators=(",", ":"),
        ).encode()

    def open_text(self, value: bytes) -> str:
        try:
            stored = json.loads(value)
            envelope = Envelope(
                stored["bundle_id"],
                stored["account_id"],
                stored["key_id"],
                base64.b64decode(stored["nonce"], validate=True),
                base64.b64decode(stored["ciphertext"], validate=True),
                base64.b64decode(stored["aad"], validate=True),
            )
            payload = self.decrypt(envelope)
        except (KeyError, TypeError, json.JSONDecodeError, ValueError) as error:
            raise ValueError("sealed text is invalid") from error
        text = payload.get("value")
        if not isinstance(text, str):
            raise ValueError("sealed text has no value")
        return text

    def materialize(self, payload: dict[str, Any], destination: Path) -> None:
        self.auth_json(payload)
        destination.mkdir(parents=True, exist_ok=True, mode=0o700)
        os.chmod(destination, 0o700)
        for item in payload.get("files", []):
            relative = PurePosixPath(item["relative_path"])
            if (
                str(relative) not in LEGACY_ALLOWED_FILES
                or relative.is_absolute()
                or ".." in relative.parts
            ):
                raise ValueError("credential payload contains an unsafe path")
            content = base64.b64decode(item["content_base64"], validate=True)
            if hashlib.sha256(content).hexdigest() != item["sha256"]:
                raise ValueError("credential payload digest mismatch")
            if str(relative) == "config.toml":
                continue
            path = destination / "auth.json"
            descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            try:
                remaining = memoryview(content)
                while remaining:
                    written = os.write(descriptor, remaining)
                    if written <= 0:
                        raise OSError("credential file write did not progress")
                    remaining = remaining[written:]
                os.fsync(descriptor)
            finally:
                os.close(descriptor)
