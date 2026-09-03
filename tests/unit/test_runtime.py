import base64
import hashlib
import json
from pathlib import Path
from typing import Any

import pytest

from codex_broker.config import Settings
from codex_broker.runtime import RuntimeManager
from codex_broker.vault import Vault, decode_key, generate_key


def credential_payload(workspace: str | None = None) -> dict[str, Any]:
    content = b'{"auth_mode":"chatgpt"}'
    return {
        "schema_version": 1,
        "codex_version": "test",
        "workspace_constraint": workspace,
        "files": [
            {
                "relative_path": "auth.json",
                "mode": 0o600,
                "sha256": hashlib.sha256(content).hexdigest(),
                "content_base64": base64.b64encode(content).decode(),
            }
        ],
    }


def test_runtime_generates_authoritative_workspace_config(tmp_path: Path) -> None:
    settings = Settings(data_dir=tmp_path / "data", runtime_dir=tmp_path / "run")
    vault = Vault(decode_key(generate_key()), "instance")
    manager = RuntimeManager(settings, vault)
    root = manager._prepare_tree(
        "account", "generation", credential_payload(), 'workspace-"quoted"'
    )
    config = (root / "codex-home" / "config.toml").read_text(encoding="utf-8")
    assert 'cli_auth_credentials_store = "file"' in config
    assert 'web_search = "disabled"' in config
    assert 'forced_chatgpt_workspace_id = "workspace-\\"quoted\\""' in config
    assert json.loads((root / "codex-home" / "auth.json").read_text())["auth_mode"] == "chatgpt"


@pytest.mark.asyncio
async def test_runtime_removes_plaintext_when_preparation_fails(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    settings = Settings(data_dir=tmp_path / "data", runtime_dir=tmp_path / "run")
    manager = RuntimeManager(settings, Vault(decode_key(generate_key()), "instance"))

    def fail_config(*args: Any) -> None:
        del args
        raise OSError("config failed")

    monkeypatch.setattr(manager, "_write_config", fail_config)
    with pytest.raises(OSError, match="config failed"):
        await manager.start_fresh("account", credential_payload())
    assert not list((tmp_path / "run" / "accounts").glob("account/*"))


@pytest.mark.asyncio
async def test_runtime_refuses_to_reuse_an_existing_generation(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    class Client:
        async def close(self) -> None:
            return None

    async def spawn(*args: Any, **kwargs: Any) -> Client:
        del args, kwargs
        return Client()

    monkeypatch.setattr("codex_broker.runtime.AppServerClient.spawn", spawn)
    settings = Settings(data_dir=tmp_path / "data", runtime_dir=tmp_path / "run")
    manager = RuntimeManager(settings, Vault(decode_key(generate_key()), "instance"))
    await manager.start_fresh("account", credential_payload())
    with pytest.raises(RuntimeError, match="runtime already exists"):
        await manager.start_fresh("account", credential_payload())
    await manager.stop("account")
