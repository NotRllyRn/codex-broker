import shutil
import subprocess
from dataclasses import dataclass

from codex_broker.config import Settings

SUPPORTED_CODEX_VERSION = "codex-cli 0.145.0"


@dataclass(frozen=True)
class Compatibility:
    compatible: bool
    code: str
    detail: str
    observed_version: str | None = None


def inspect_codex(settings: Settings) -> Compatibility:
    executable = shutil.which(settings.codex_executable)
    if not executable:
        return Compatibility(False, "CODEX_NOT_FOUND", "The managed Codex executable was not found")
    try:
        observed_version = subprocess.run(  # noqa: S603
            [executable, "--version"],
            capture_output=True,
            text=True,
            timeout=10,
            check=True,
        ).stdout.strip()
    except (OSError, subprocess.SubprocessError) as error:
        return Compatibility(False, "CODEX_INSPECTION_FAILED", type(error).__name__)
    if observed_version != SUPPORTED_CODEX_VERSION:
        return Compatibility(
            False,
            "CODEX_UNSUPPORTED",
            f"Expected managed {SUPPORTED_CODEX_VERSION}, found {observed_version or 'no version'}",
            observed_version or None,
        )
    return Compatibility(
        True,
        "CODEX_AVAILABLE",
        f"Managed Codex is available ({observed_version})",
        observed_version,
    )
