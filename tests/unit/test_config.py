from pathlib import Path

import pytest
from fastapi.testclient import TestClient
from pydantic import ValidationError

from codex_broker.config import Settings
from codex_broker.vault import generate_key
from codex_broker.web.app import _read_secret, create_app

PASSWORD = "correct horse battery staple"  # noqa: S105


def test_session_defaults_are_persistent_but_bounded(tmp_path: Path) -> None:
    settings = Settings(data_dir=tmp_path / "data", runtime_dir=tmp_path / "run")
    assert settings.session_idle_minutes == 31 * 24 * 60
    assert settings.session_absolute_hours == 90 * 24


def test_dotenv_secrets_are_loaded(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    key = generate_key()
    (tmp_path / ".env").write_text(
        f'WINDOWKEEPER_ADMIN_PASSWORD="{PASSWORD}"\nWINDOWKEEPER_VAULT_KEY={key}\n',
        encoding="utf-8",
    )
    monkeypatch.chdir(tmp_path)
    settings = Settings(data_dir=tmp_path / "data", runtime_dir=tmp_path / "run")
    assert settings.admin_password == PASSWORD
    assert settings.vault_key == key


def test_secret_file_precedes_environment_value_and_requires_private_mode(tmp_path: Path) -> None:
    source = tmp_path / "secret"
    source.write_text("from-file", encoding="utf-8")
    source.chmod(0o600)
    assert _read_secret(source, "from-environment") == "from-file"
    source.chmod(0o644)
    with pytest.raises(RuntimeError):
        _read_secret(source, None)


def test_trusted_proxy_configuration_is_validated(tmp_path: Path) -> None:
    with pytest.raises(ValidationError):
        Settings(
            data_dir=tmp_path / "data-2",
            runtime_dir=tmp_path / "run-2",
            trusted_proxies="*",
        )


def test_wrong_vault_key_fails_startup_without_mutating_data(tmp_path: Path) -> None:
    data, runtime = tmp_path / "data", tmp_path / "run"
    first = Settings(
        data_dir=data,
        runtime_dir=runtime,
        vault_key=generate_key(),
        admin_password=PASSWORD,
    )
    with TestClient(create_app(first)):
        pass
    second = Settings(
        data_dir=data,
        runtime_dir=runtime,
        vault_key=generate_key(),
        admin_password=PASSWORD,
    )
    with pytest.raises(RuntimeError, match="vault key"), TestClient(create_app(second)):
        pass
