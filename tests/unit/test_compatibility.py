from pathlib import Path

from windowkeeper.compatibility import inspect_codex
from windowkeeper.config import Settings


def test_unmanaged_codex_version_is_rejected(tmp_path: Path) -> None:
    executable = tmp_path / "codex"
    executable.write_text("#!/bin/sh\necho 'codex-cli 0.1.0'\n", encoding="utf-8")
    executable.chmod(0o700)

    compatibility = inspect_codex(Settings(codex_executable=str(executable)))

    assert not compatibility.compatible
    assert compatibility.code == "CODEX_UNSUPPORTED"
