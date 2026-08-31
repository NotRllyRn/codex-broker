import asyncio
import sqlite3
from pathlib import Path

from click.testing import CliRunner

from windowkeeper.cli.main import cli
from windowkeeper.config import get_settings
from windowkeeper.database import Database
from windowkeeper.vault import Envelope, Vault, decode_key, generate_key
from windowkeeper.webhooks import WebhookDispatcher


def test_vault_rotation_includes_webhook_secrets(tmp_path: Path) -> None:
    data = tmp_path / "data"
    database = Database(data / "windowkeeper.db")
    database.start()
    instance = asyncio.run(
        database.call(
            lambda connection: str(
                connection.execute("SELECT instance_uuid FROM instance_metadata").fetchone()[0]
            )
        )
    )
    old_encoded = generate_key()
    old_vault = Vault(decode_key(old_encoded), instance)
    destination = asyncio.run(
        WebhookDispatcher(database, old_vault).create_destination(
            "Protected", "https://example.test/hook", "signing-secret"
        )
    )
    source = tmp_path / "export"
    source.mkdir()
    source.joinpath("auth.json").write_text('{"refresh_token":"export"}', encoding="utf-8")
    export = old_vault.encrypt("account-export", old_vault.capture(source, "test"))

    def seed_export(connection: sqlite3.Connection) -> None:
        connection.execute(
            "INSERT INTO accounts VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
            (
                "account-export",
                "acct_export",
                "Export account",
                "chatgpt",
                "CHATGPT_DEVICE_CODE",
                None,
                None,
                1,
                "ACTIVE",
                1,
                1,
                None,
            ),
        )
        connection.execute(
            "INSERT INTO credential_bundles VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (
                export.bundle_id,
                export.account_id,
                "EXPORT",
                export.envelope_version,
                export.payload_schema_version,
                export.key_id,
                export.nonce,
                export.ciphertext,
                export.aad,
                "test",
                1,
                None,
                None,
            ),
        )

    asyncio.run(database.transaction(seed_export))
    asyncio.run(database.close())
    old_file = tmp_path / "old.key"
    old_file.write_text(old_encoded, encoding="utf-8")
    old_file.chmod(0o600)
    new_file = tmp_path / "new.key"
    get_settings.cache_clear()
    result = CliRunner().invoke(
        cli,
        [
            "vault",
            "rotate",
            "--old-key-file",
            str(old_file),
            "--new-key-file",
            str(new_file),
        ],
        env={"WINDOWKEEPER_DATA_DIR": str(data)},
    )
    assert result.exit_code == 0, result.output
    database = Database(data / "windowkeeper.db")
    database.start()
    row, export_row, sentinel = asyncio.run(
        database.call(
            lambda connection: (
                connection.execute(
                    "SELECT encrypted_url,encrypted_signing_secret FROM webhook_destinations WHERE destination_id=?",
                    (destination,),
                ).fetchone(),
                connection.execute(
                    "SELECT * FROM credential_bundles WHERE account_id='account-export' AND state='EXPORT'"
                ).fetchone(),
                connection.execute(
                    "SELECT sentinel_ciphertext FROM vault_state WHERE singleton_id=1"
                ).fetchone()[0],
            )
        )
    )
    asyncio.run(database.close())
    new_vault = Vault(decode_key(new_file.read_text(encoding="utf-8").strip()), instance)
    assert new_vault.open_text(row[0]) == "https://example.test/hook"
    assert new_vault.open_text(row[1]) == "signing-secret"
    export_payload = new_vault.decrypt(
        Envelope(
            export_row["bundle_id"],
            export_row["account_id"],
            export_row["key_id"],
            export_row["nonce"],
            export_row["ciphertext"],
            export_row["aad"],
            export_row["payload_schema_version"],
            export_row["envelope_version"],
        )
    )
    assert new_vault.auth_json(export_payload) == b'{"refresh_token":"export"}'
    assert new_vault.open_text(sentinel) == f"windowkeeper:{instance}"
