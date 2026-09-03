import base64
import hashlib
from pathlib import Path

import pytest

from codex_broker.vault import Vault, decode_key, generate_key


def test_envelope_round_trip_and_account_binding() -> None:
    vault = Vault(decode_key(generate_key()), "instance")
    payload = {"schema_version": 1, "files": []}
    envelope = vault.encrypt("account-a", payload)
    assert vault.decrypt(envelope) == payload
    assert (
        vault.open_text(vault.seal_text("webhook:a", "https://example.test/hook"))
        == "https://example.test/hook"
    )


def test_capture_and_materialize_reject_unsafe_paths(tmp_path: Path) -> None:
    source = tmp_path / "source"
    source.mkdir()
    (source / "auth.json").write_text('{"token":"secret"}', encoding="utf-8")
    (source / "config.toml").write_text('web_search = "enabled"\n', encoding="utf-8")
    vault = Vault(decode_key(generate_key()), "instance")
    payload = vault.capture(source, "test")
    assert vault.auth_json(payload) == b'{"token":"secret"}'
    assert vault.auth_fingerprint(payload) == hashlib.sha256(b'{"token":"secret"}').hexdigest()
    assert [item["relative_path"] for item in payload["files"]] == ["auth.json"]
    legacy_config = b'web_search = "enabled"\n'
    payload["files"].append(
        {
            "relative_path": "config.toml",
            "mode": 0o600,
            "sha256": hashlib.sha256(legacy_config).hexdigest(),
            "content_base64": base64.b64encode(legacy_config).decode(),
        }
    )
    destination = tmp_path / "destination"
    vault.materialize(payload, destination)
    assert (destination / "auth.json").read_text() == '{"token":"secret"}'
    assert not (destination / "config.toml").exists()
    payload["files"][0]["relative_path"] = "../auth.json"
    with pytest.raises(ValueError):
        vault.auth_json(payload)
    with pytest.raises(ValueError):
        vault.materialize(payload, tmp_path / "bad")
