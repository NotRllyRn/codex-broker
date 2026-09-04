# pyright: reportMissingImports=false, reportFunctionMemberAccess=false

import asyncio
import ipaddress
import json
import os
import shutil
import sqlite3
import stat
import time
from contextlib import closing
from pathlib import Path
from typing import Any

import click
import httpx
import uvicorn

from codex_broker.client_auth import ClientKeyService
from codex_broker.compatibility import inspect_codex
from codex_broker.config import Settings, get_settings
from codex_broker.database import Database
from codex_broker.security import AdminSecurity
from codex_broker.singleton import SingletonLock
from codex_broker.vault import Envelope, Vault, decode_key, generate_key
from codex_broker.version import __version__ as VERSION


def _settings() -> Settings:
    return get_settings()


def _run(coroutine: Any) -> Any:
    return asyncio.run(coroutine)


def _database(settings: Settings) -> tuple[SingletonLock, Database]:
    lock = SingletonLock(settings.data_dir / "windowkeeper.lock")
    lock.acquire()
    database = Database(settings.data_dir / "windowkeeper.db")
    try:
        database.start()
    except BaseException:
        lock.release()
        raise
    return lock, database


async def _close(database: Database, lock: SingletonLock) -> None:
    await database.close()
    lock.release()


def _fsync_directory(path: Path) -> None:
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


@click.group(context_settings={"help_option_names": ["-h", "--help"]})
@click.version_option(VERSION)
def cli() -> None:
    """Operate the Codex Broker service and its durable state."""
    get_settings.cache_clear()


@cli.command()
@click.option("--host", default=None, help="Override the configured bind host.")
@click.option("--port", type=int, default=None, help="Override the configured bind port.")
def serve(host: str | None, port: int | None) -> None:
    """Run the authenticated dashboard and scheduler."""
    settings = _settings()
    bind_host = host or settings.host
    try:
        loopback = ipaddress.ip_address(bind_host).is_loopback
    except ValueError:
        loopback = bind_host.lower() == "localhost"
    if not loopback and not (settings.tls_cert_file and settings.tls_key_file):
        raise click.ClickException("non-loopback binds require TLS certificate and key files")
    uvicorn.run(
        "codex_broker.web.app:app",
        host=bind_host,
        port=port or settings.port,
        ssl_certfile=settings.tls_cert_file,
        ssl_keyfile=settings.tls_key_file,
        proxy_headers=bool(settings.trusted_proxies),
        forwarded_allow_ips=settings.trusted_proxies or "",
        server_header=False,
        log_level=settings.log_level.lower(),
    )


@cli.command("init")
@click.option("--key-file", type=click.Path(path_type=Path), default=Path("windowkeeper-vault.key"))
@click.password_option(confirmation_prompt=True)
def initialize(key_file: Path, password: str) -> None:
    """Create the data store, vault key, and first administrator password."""
    settings = _settings()
    if key_file.exists():
        raise click.ClickException(f"key file already exists: {key_file}")
    key_file.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    encoded_key = generate_key()
    descriptor = os.open(
        key_file,
        os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW,
        0o600,
    )
    try:
        os.write(descriptor, (encoded_key + "\n").encode())
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    try:
        lock, database = _database(settings)
    except BaseException:
        key_file.unlink(missing_ok=True)
        raise

    async def apply() -> None:
        try:
            instance = await database.call(
                lambda connection: str(
                    connection.execute(
                        "SELECT instance_uuid FROM instance_metadata WHERE singleton_id=1"
                    ).fetchone()[0]
                )
            )
            now = int(time.time() * 1000)
            vault = Vault(decode_key(encoded_key), instance)
            sentinel = vault.seal_text("vault-sentinel", f"windowkeeper:{instance}")
            await database.transaction(
                lambda connection: connection.execute(
                    "INSERT INTO vault_state VALUES(1,?,?,?,?) ON CONFLICT(singleton_id) DO NOTHING",
                    (vault.key_id, os.urandom(12), sentinel, now),
                )
            )
            await AdminSecurity(database, settings).set_password(password)
        finally:
            await _close(database, lock)

    try:
        _run(apply())
    except BaseException:
        key_file.unlink(missing_ok=True)
        raise
    click.echo("Codex Broker initialized.")
    click.echo(f"Set WINDOWKEEPER_VAULT_KEY_FILE={key_file.resolve()}")


@cli.command("password-set")
@click.password_option(confirmation_prompt=True)
def password_set(password: str) -> None:
    """Replace the administrator password and revoke all sessions."""
    settings = _settings()
    lock, database = _database(settings)

    async def apply() -> None:
        try:
            await AdminSecurity(database, settings).set_password(password)
        finally:
            await _close(database, lock)

    _run(apply())
    click.echo("Administrator password updated; existing sessions were revoked.")


@cli.command("version")
@click.option("--json", "as_json", is_flag=True, help="Emit machine-readable JSON.")
def version_command(as_json: bool) -> None:
    """Show the Codex Broker CLI version."""
    if as_json:
        click.echo(
            json.dumps(
                {
                    "api_version": "windowkeeper.dev/cli/v1",
                    "kind": "Version",
                    "data": {"version": VERSION},
                }
            )
        )
        return
    click.echo(VERSION)


@cli.command()
@click.option("--json", "as_json", is_flag=True, help="Emit machine-readable JSON.")
def health(as_json: bool) -> None:
    """Check the primary service readiness endpoint."""
    settings = _settings()
    try:
        configured_address = ipaddress.ip_address(settings.host)
        host = "127.0.0.1" if configured_address.is_unspecified else settings.host
    except ValueError:
        host = settings.host
    authority = f"[{host}]" if ":" in host else host
    url = f"http://{authority}:{settings.port}/health/ready"
    try:
        with httpx.Client(trust_env=False, timeout=3) as client:
            response = client.get(url)
        data = response.json()
    except (httpx.HTTPError, ValueError) as error:
        if as_json:
            click.echo(
                json.dumps(
                    {
                        "api_version": "windowkeeper.dev/cli/v1",
                        "kind": "Health",
                        "data": {"status": "unavailable", "error": type(error).__name__},
                    }
                )
            )
        else:
            click.echo("Codex Broker service is unavailable.", err=True)
        raise click.exceptions.Exit(5) from error
    payload = {
        "api_version": "windowkeeper.dev/cli/v1",
        "kind": "Health",
        "data": data,
    }
    click.echo(json.dumps(payload) if as_json else f"Service status: {data['status']}")
    if response.status_code != 200:
        raise click.exceptions.Exit(1)


@cli.command()
@click.option(
    "--json", "--json-output", "as_json", is_flag=True, help="Emit machine-readable JSON."
)
def status(as_json: bool) -> None:
    """Show durable service and account status while the service is stopped."""
    settings = _settings()
    lock, database = _database(settings)

    async def read() -> dict[str, Any]:
        try:
            accounts = await database.call(
                lambda connection: [
                    dict(row)
                    for row in connection.execute(
                        "SELECT a.public_token,a.display_name,a.enabled,s.overall_state,s.auth_state,s.usage_state FROM accounts a JOIN account_state s USING(account_id) WHERE a.deleted_at_ms IS NULL ORDER BY lower(a.display_name)"
                    )
                ]
            )
            incidents = await database.call(
                lambda connection: int(
                    connection.execute(
                        "SELECT count(*) FROM incidents WHERE state='OPEN'"
                    ).fetchone()[0]
                )
            )
            return {"version": VERSION, "accounts": accounts, "open_incidents": incidents}
        finally:
            await _close(database, lock)

    value = _run(read())
    if as_json:
        click.echo(json.dumps(value, indent=2))
        return
    click.echo(
        f"Codex Broker {value['version']} | {len(value['accounts'])} accounts | {value['open_incidents']} open incidents"
    )
    for account in value["accounts"]:
        click.echo(
            f"  {account['display_name']:<24} {account['overall_state']:<16} {account['usage_state']}"
        )


@cli.group("client-key")
def client_key() -> None:
    """Create and revoke machine API keys."""


@client_key.command("create")
@click.argument("name")
def client_key_create(name: str) -> None:
    """Create a key and print its secret once."""
    settings = _settings()
    lock, database = _database(settings)

    async def apply() -> str:
        try:
            return (await ClientKeyService(database).create(name)).token
        finally:
            await _close(database, lock)

    click.echo(_run(apply()))


@client_key.command("list")
def client_key_list() -> None:
    """List key metadata without secrets."""
    settings = _settings()
    lock, database = _database(settings)

    async def read() -> list[dict[str, object]]:
        try:
            return await ClientKeyService(database).list()
        finally:
            await _close(database, lock)

    for key in _run(read()):
        status = "revoked" if key["revoked_at_ms"] else "active"
        click.echo(f"{key['key_id']}  {key['key_prefix']}  {status}  {key['name']}")


@client_key.command("revoke")
@click.argument("key_id")
def client_key_revoke(key_id: str) -> None:
    """Revoke one machine API key."""
    settings = _settings()
    lock, database = _database(settings)

    async def apply() -> bool:
        try:
            return await ClientKeyService(database).revoke(key_id)
        finally:
            await _close(database, lock)

    if not _run(apply()):
        raise click.ClickException("active client key not found")
    click.echo("Client key revoked.")


@cli.command()
def doctor() -> None:
    """Validate configuration, filesystem boundaries, and managed Codex."""
    settings = _settings()
    checks: list[tuple[str, bool, str]] = []
    checks.append(
        (
            "directory separation",
            settings.data_dir.resolve() != settings.runtime_dir.resolve(),
            "persistent and runtime roots differ",
        )
    )
    compatibility = inspect_codex(settings)
    checks.append(("managed codex", compatibility.compatible, compatibility.detail))
    failed = False
    for name, ok, detail in checks:
        failed |= not ok
        click.echo(f"{'PASS' if ok else 'FAIL'}  {name}: {detail}")
    if failed:
        raise click.exceptions.Exit(1)


@cli.command()
@click.option("--output", required=True, type=click.Path(path_type=Path))
def backup(output: Path) -> None:
    """Create an offline, integrity-checked SQLite backup."""
    settings = _settings()
    source = settings.data_dir / "windowkeeper.db"
    if not source.exists():
        raise click.ClickException("database does not exist")
    output.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = output.with_name(f".{output.name}.{os.getpid()}.tmp")
    lock = SingletonLock(settings.data_dir / "windowkeeper.lock")
    lock.acquire()
    try:
        descriptor = os.open(
            temporary,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW,
            0o600,
        )
        os.close(descriptor)
        with (
            closing(sqlite3.connect(source)) as current,
            closing(sqlite3.connect(temporary)) as destination,
        ):
            current.backup(destination)
            if destination.execute("PRAGMA integrity_check").fetchone()[0] != "ok":
                raise click.ClickException("backup integrity check failed")
        descriptor = os.open(temporary, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
        os.replace(temporary, output)
        _fsync_directory(output.parent)
    finally:
        temporary.unlink(missing_ok=True)
        lock.release()
    click.echo(f"Backup written to {output}")


@cli.command()
@click.option("--input", "input_file", required=True, type=click.Path(exists=True, path_type=Path))
@click.option("--confirm", required=True, help="Type RESTORE to replace local state.")
def restore(input_file: Path, confirm: str) -> None:
    """Replace offline state from an integrity-checked backup."""
    if confirm != "RESTORE":
        raise click.ClickException("confirmation must be exactly RESTORE")
    try:
        metadata = input_file.lstat()
    except OSError as error:
        raise click.ClickException("backup could not be inspected") from error
    if input_file.is_symlink() or not stat.S_ISREG(metadata.st_mode) or metadata.st_mode & 0o077:
        raise click.ClickException("backup must be a protected regular file")
    with closing(
        sqlite3.connect(f"{input_file.resolve().as_uri()}?mode=ro&immutable=1", uri=True)
    ) as source:
        if source.execute("PRAGMA integrity_check").fetchone()[0] != "ok":
            raise click.ClickException("backup integrity check failed")
        version = source.execute("SELECT max(version) FROM schema_migrations").fetchone()[0]
        latest = len(
            tuple((Path(__file__).parents[1] / "migrations").glob("[0-9][0-9][0-9]_*.sql"))
        )
        if version != latest:
            raise click.ClickException("backup schema is not supported by this release")
    settings = _settings()
    destination = settings.data_dir / "windowkeeper.db"
    destination.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = destination.with_name(f".{destination.name}.{os.getpid()}.restore")
    lock = SingletonLock(settings.data_dir / "windowkeeper.lock")
    lock.acquire()
    try:
        descriptor = os.open(
            temporary,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW,
            0o600,
        )
        with os.fdopen(descriptor, "wb") as target, input_file.open("rb") as source:
            shutil.copyfileobj(source, target, length=1024 * 1024)
            target.flush()
            os.fsync(target.fileno())
        os.replace(temporary, destination)
        for suffix in ("-wal", "-shm"):
            destination.with_name(destination.name + suffix).unlink(missing_ok=True)
        _fsync_directory(destination.parent)
    finally:
        temporary.unlink(missing_ok=True)
        lock.release()
    click.echo("Backup restored. Verify the vault key before starting Codex Broker.")


@cli.group()
def vault() -> None:
    """Inspect and rotate the encrypted credential vault."""


@vault.command("generate-key")
@click.option("--output", type=click.Path(path_type=Path))
def vault_generate_key(output: Path | None) -> None:
    """Print a new vault key, or write it to a protected file."""
    if output is None:
        click.echo(generate_key())
        return
    output.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    descriptor = os.open(
        output,
        os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW,
        0o600,
    )
    try:
        os.write(descriptor, (generate_key() + "\n").encode())
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    click.echo(f"Vault key written to {output}")


@vault.command("verify")
@click.option("--key-file", required=True, type=click.Path(exists=True, path_type=Path))
def vault_verify(key_file: Path) -> None:
    """Verify a protected key against the offline database sentinel."""
    try:
        metadata = key_file.lstat()
        if key_file.is_symlink() or not stat.S_ISREG(metadata.st_mode) or metadata.st_mode & 0o077:
            raise click.ClickException("key file must be a protected regular file")
        key = decode_key(key_file.read_text(encoding="utf-8").strip())
    except (OSError, ValueError) as error:
        raise click.ClickException("key file could not be read") from error
    settings = _settings()
    lock, database = _database(settings)

    async def verify() -> bool:
        try:
            instance, sentinel = await database.call(
                lambda connection: (
                    str(
                        connection.execute(
                            "SELECT instance_uuid FROM instance_metadata WHERE singleton_id=1"
                        ).fetchone()[0]
                    ),
                    connection.execute(
                        "SELECT sentinel_ciphertext FROM vault_state WHERE singleton_id=1"
                    ).fetchone(),
                )
            )
            if not sentinel:
                raise click.ClickException("database has no vault sentinel")
            return Vault(key, instance).open_text(bytes(sentinel[0])) == f"windowkeeper:{instance}"
        finally:
            await _close(database, lock)

    try:
        valid = _run(verify())
    except Exception as error:
        raise click.ClickException("vault key verification failed") from error
    if not valid:
        raise click.ClickException("vault key verification failed")
    click.echo("Vault key verified.")


@vault.command("rotate")
@click.option("--old-key-file", required=True, type=click.Path(exists=True, path_type=Path))
@click.option("--new-key-file", required=True, type=click.Path(path_type=Path))
def vault_rotate(old_key_file: Path, new_key_file: Path) -> None:
    """Re-encrypt all retained credential bundles under a newly generated key."""
    if new_key_file.exists():
        raise click.ClickException("new key file already exists")
    settings = _settings()
    try:
        metadata = old_key_file.lstat()
        if (
            old_key_file.is_symlink()
            or not stat.S_ISREG(metadata.st_mode)
            or metadata.st_mode & 0o077
        ):
            raise click.ClickException("old key file must be a protected regular file")
        old_key = decode_key(old_key_file.read_text(encoding="utf-8").strip())
    except (OSError, ValueError) as error:
        raise click.ClickException("old key file could not be read") from error
    new_encoded = generate_key()
    new_key = decode_key(new_encoded)
    descriptor = os.open(
        new_key_file,
        os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW,
        0o600,
    )
    try:
        os.write(descriptor, (new_encoded + "\n").encode())
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    try:
        lock, database = _database(settings)
    except Exception:
        new_key_file.unlink(missing_ok=True)
        raise

    async def rotate() -> int:
        try:
            instance = await database.call(
                lambda connection: str(
                    connection.execute(
                        "SELECT instance_uuid FROM instance_metadata WHERE singleton_id=1"
                    ).fetchone()[0]
                )
            )
            old_vault, new_vault = (
                Vault(old_key, instance, "previous"),
                Vault(new_key, instance, "rotated"),
            )

            def work(connection: sqlite3.Connection) -> int:
                rows = connection.execute(
                    "SELECT * FROM credential_bundles WHERE state IN('ACTIVE','EXPORT')"
                ).fetchall()
                destinations = connection.execute(
                    "SELECT destination_id,encrypted_url,encrypted_signing_secret FROM webhook_destinations"
                ).fetchall()
                rotated: list[tuple[sqlite3.Row, Envelope]] = []
                rotated_destinations: list[tuple[str, bytes, bytes | None]] = []
                for row in rows:
                    envelope = Envelope(
                        row["bundle_id"],
                        row["account_id"],
                        row["key_id"],
                        row["nonce"],
                        row["ciphertext"],
                        row["aad"],
                        row["payload_schema_version"],
                        row["envelope_version"],
                    )
                    payload = old_vault.decrypt(envelope)
                    rotated.append((row, new_vault.encrypt(row["account_id"], payload)))
                for destination in destinations:
                    encrypted_secret = destination["encrypted_signing_secret"]
                    rotated_destinations.append(
                        (
                            str(destination["destination_id"]),
                            new_vault.seal_text(
                                f"webhook:{destination['destination_id']}:url",
                                old_vault.open_text(destination["encrypted_url"]),
                            ),
                            new_vault.seal_text(
                                f"webhook:{destination['destination_id']}:secret",
                                old_vault.open_text(encrypted_secret),
                            )
                            if encrypted_secret
                            else None,
                        )
                    )
                now = __import__("time").time_ns() // 1_000_000
                for row, envelope in rotated:
                    state = str(row["state"])
                    if state == "ACTIVE":
                        connection.execute(
                            "UPDATE credential_bundles SET state='RETIRED',retired_at_ms=? WHERE bundle_id=?",
                            (now, row["bundle_id"]),
                        )
                    else:
                        connection.execute(
                            "DELETE FROM credential_bundles WHERE bundle_id=?", (row["bundle_id"],)
                        )
                    connection.execute(
                        "INSERT INTO credential_bundles VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
                        (
                            envelope.bundle_id,
                            envelope.account_id,
                            state,
                            envelope.envelope_version,
                            envelope.payload_schema_version,
                            envelope.key_id,
                            envelope.nonce,
                            envelope.ciphertext,
                            envelope.aad,
                            row["codex_version"],
                            row["created_at_ms"] if state == "EXPORT" else now,
                            now if state == "ACTIVE" else None,
                            None,
                        ),
                    )
                for destination_id, url, secret in rotated_destinations:
                    connection.execute(
                        "UPDATE webhook_destinations SET encrypted_url=?,encrypted_signing_secret=?,updated_at_ms=? WHERE destination_id=?",
                        (url, secret, now, destination_id),
                    )
                sentinel = new_vault.seal_text("vault-sentinel", f"windowkeeper:{instance}")
                connection.execute(
                    "INSERT INTO vault_state VALUES(1,?,?,?,?) ON CONFLICT(singleton_id) DO UPDATE SET active_key_id=excluded.active_key_id,sentinel_nonce=excluded.sentinel_nonce,sentinel_ciphertext=excluded.sentinel_ciphertext,updated_at_ms=excluded.updated_at_ms",
                    (new_vault.key_id, os.urandom(12), sentinel, now),
                )
                return len(rotated) + len(rotated_destinations)

            return await database.transaction(work)
        finally:
            await _close(database, lock)

    try:
        count = _run(rotate())
    except Exception:
        new_key_file.unlink(missing_ok=True)
        raise
    click.echo(f"Rotated {count} encrypted object(s). Replace the configured key file atomically.")


if __name__ == "__main__":
    cli()
