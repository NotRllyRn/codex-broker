import json
from pathlib import Path

from click.testing import CliRunner

from windowkeeper.cli.main import cli

PASSWORD = "correct horse battery staple"  # noqa: S105


def test_cli_initialization_vault_verification_and_version(tmp_path: Path) -> None:
    key_file = tmp_path / "vault.key"
    environment = {
        "WINDOWKEEPER_DATA_DIR": str(tmp_path / "data"),
        "WINDOWKEEPER_RUNTIME_DIR": str(tmp_path / "run"),
    }
    runner = CliRunner()
    generated = runner.invoke(cli, ["vault", "generate-key"], env=environment)
    assert generated.exit_code == 0, generated.output
    assert generated.output.strip().startswith("wk1_")
    initialized = runner.invoke(
        cli,
        ["init", "--key-file", str(key_file)],
        input=f"{PASSWORD}\n{PASSWORD}\n",
        env=environment,
    )
    assert initialized.exit_code == 0, initialized.output
    assert key_file.stat().st_mode & 0o077 == 0

    verified = runner.invoke(
        cli,
        ["vault", "verify", "--key-file", str(key_file)],
        env=environment,
    )
    assert verified.exit_code == 0, verified.output
    status = runner.invoke(cli, ["status", "--json"], env=environment)
    assert status.exit_code == 0, status.output
    assert json.loads(status.output)["accounts"] == []

    backup_file = tmp_path / "backup.sqlite"
    backed_up = runner.invoke(cli, ["backup", "--output", str(backup_file)], env=environment)
    assert backed_up.exit_code == 0, backed_up.output
    assert backup_file.stat().st_mode & 0o077 == 0

    restored_environment = {
        **environment,
        "WINDOWKEEPER_DATA_DIR": str(tmp_path / "restored-data"),
    }
    restored = runner.invoke(
        cli,
        ["restore", "--input", str(backup_file), "--confirm", "RESTORE"],
        env=restored_environment,
    )
    assert restored.exit_code == 0, restored.output
    restored_key = runner.invoke(
        cli,
        ["vault", "verify", "--key-file", str(key_file)],
        env=restored_environment,
    )
    assert restored_key.exit_code == 0, restored_key.output

    version = runner.invoke(cli, ["version", "--json"], env=environment)
    assert version.exit_code == 0, version.output
    payload = json.loads(version.output)
    assert payload["api_version"] == "windowkeeper.dev/cli/v1"
    assert payload["data"]["version"]

    executable = tmp_path / "fake_codex.py"
    executable.write_bytes((Path(__file__).parents[1] / "fake_codex.py").read_bytes())
    executable.chmod(0o700)
    compatible_environment = {
        **environment,
        "WINDOWKEEPER_CODEX_EXECUTABLE": str(executable),
    }
    doctor = runner.invoke(cli, ["doctor"], env=compatible_environment)
    assert doctor.exit_code == 0, doctor.output
    assert "PASS  managed codex: Managed Codex is available (codex-cli 0.145.0)" in doctor.output

    health = runner.invoke(
        cli,
        ["health", "--json"],
        env={**environment, "WINDOWKEEPER_PORT": "1"},
    )
    assert health.exit_code == 5, health.output
    assert json.loads(health.output)["data"]["status"] == "unavailable"
