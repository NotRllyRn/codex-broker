import asyncio
import json
import os
import shutil
from contextlib import suppress
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .codex.adapter import CodexAdapter
from .codex.client import AppServerClient
from .config import Settings
from .ids import new_id
from .vault import Vault


@dataclass(slots=True)
class AccountRuntime:
    account_id: str
    generation_id: str
    root: Path
    client: AppServerClient
    adapter: CodexAdapter
    lock: asyncio.Lock

    @property
    def codex_home(self) -> Path:
        return self.root / "codex-home"

    @property
    def workspace(self) -> Path:
        return self.root / "workspace"


class RuntimeManager:
    """Owns one short-lived isolated Codex runtime per account operation."""

    def __init__(self, settings: Settings, vault: Vault | None = None) -> None:
        self.settings = settings
        self.vault = vault
        self._runtimes: dict[str, AccountRuntime] = {}
        self._quarantined: dict[str, AccountRuntime] = {}
        self._manager_lock = asyncio.Lock()
        self._start_semaphore = asyncio.Semaphore(settings.process_start_concurrency)

    def _tree(self, account_id: str, generation: str) -> Path:
        return self.settings.runtime_dir / "accounts" / account_id / generation

    def _write_config(self, codex_home: Path, workspace_constraint: str | None) -> None:
        lines = ['cli_auth_credentials_store = "file"', 'web_search = "disabled"']
        if workspace_constraint:
            lines.append(f"forced_chatgpt_workspace_id = {json.dumps(workspace_constraint)}")
        path = codex_home / "config.toml"
        path.write_text("\n".join(lines) + "\n", encoding="utf-8")
        os.chmod(path, 0o600)

    def _prepare_tree(
        self,
        account_id: str,
        generation: str,
        payload: dict[str, Any] | None,
        workspace_constraint: str | None,
    ) -> Path:
        root = self._tree(account_id, generation)
        for child in ("home", "codex-home", "tmp", "workspace"):
            path = root / child
            path.mkdir(parents=True, mode=0o700)
            os.chmod(path, 0o700)
        if payload:
            if not self.vault:
                raise RuntimeError("vault is unavailable")
            self.vault.materialize(payload, root / "codex-home")
        self._write_config(root / "codex-home", workspace_constraint)
        return root

    async def start_fresh(
        self,
        account_id: str,
        payload: dict[str, Any] | None = None,
        workspace_constraint: str | None = None,
    ) -> AccountRuntime:
        async with self._manager_lock:
            if account_id in self._runtimes or account_id in self._quarantined:
                raise RuntimeError(f"runtime already exists for account {account_id}")
            generation = new_id()
            root = self._tree(account_id, generation)
            try:
                root = self._prepare_tree(account_id, generation, payload, workspace_constraint)
            except BaseException:
                shutil.rmtree(root, ignore_errors=True)
                raise
            environment = {
                "PATH": "/usr/local/bin:/usr/bin:/bin",
                "HOME": str(root / "home"),
                "CODEX_HOME": str(root / "codex-home"),
                "TMPDIR": str(root / "tmp"),
                "LANG": "C.UTF-8",
                "LC_ALL": "C.UTF-8",
                "TZ": "UTC",
                "NO_COLOR": "1",
            }
            try:
                async with self._start_semaphore:
                    client = await AppServerClient.spawn(
                        self.settings.codex_executable,
                        cwd=str(root / "workspace"),
                        environment=environment,
                    )
            except BaseException:
                shutil.rmtree(root, ignore_errors=True)
                raise
            runtime = AccountRuntime(
                account_id, generation, root, client, CodexAdapter(client), asyncio.Lock()
            )
            self._runtimes[account_id] = runtime
            return runtime

    async def get_existing(self, account_id: str) -> AccountRuntime | None:
        async with self._manager_lock:
            return self._runtimes.get(account_id)

    async def discard(self, runtime: AccountRuntime) -> None:
        async with self._manager_lock:
            if self._runtimes.get(runtime.account_id) is runtime:
                self._runtimes.pop(runtime.account_id)
        try:
            shutil.rmtree(runtime.root)
        except FileNotFoundError as missing:
            del missing

    async def archive(self, runtime: AccountRuntime) -> None:
        """Detach stopped non-authoritative evidence without blocking the account."""
        async with self._manager_lock:
            if self._runtimes.get(runtime.account_id) is runtime:
                self._runtimes.pop(runtime.account_id)

    async def preserve(self, runtime: AccountRuntime) -> None:
        """Quarantine authoritative checkpoint evidence while retaining ownership."""
        with suppress(BaseException):
            await runtime.client.close()
        async with self._manager_lock:
            if self._runtimes.get(runtime.account_id) is runtime:
                self._runtimes.pop(runtime.account_id)
            self._quarantined[runtime.account_id] = runtime

    async def stop(self, account_id: str) -> None:
        runtime = await self.get_existing(account_id)
        if not runtime:
            return
        async with runtime.lock:
            await runtime.client.close()
            await self.discard(runtime)

    async def close(self) -> None:
        for account_id in list(self._runtimes):
            await self.stop(account_id)
        for runtime in list(self._quarantined.values()):
            with suppress(BaseException):
                await runtime.client.close()
