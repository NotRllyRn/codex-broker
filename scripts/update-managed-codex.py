#!/usr/bin/env python3
"""Check or update generated managed Codex pins; standard library only."""
import argparse
import os
from pathlib import Path
import re
import urllib.request
import json

ROOT = Path(__file__).resolve().parents[1]
VERSION = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")
# Historical research and vault serialization fixtures deliberately retain their versions.
PINS = {
    "Dockerfile": (r"(@openai/codex@)([0-9]+\.[0-9]+\.[0-9]+)", 1),
    "internal/codex/compatibility.go": (r'(const SupportedVersion = "codex-cli )([0-9]+\.[0-9]+\.[0-9]+)', 1),
    "flake.nix": (r'(pinned to version |cfg.codex.version == "|supported version )([0-9]+\.[0-9]+\.[0-9]+)', 3),
    "nix/checks.nix": (r'(version = ")([0-9]+\.[0-9]+\.[0-9]+)', 1),
    "README.md": (r'(Must provide codex-cli )([0-9]+\.[0-9]+\.[0-9]+)', 1),
}


def parse_version(value):
    if not isinstance(value, str) or not VERSION.fullmatch(value):
        raise ValueError(f"Expected a stable numeric version, received {value!r}")
    return tuple(map(int, value.split(".")))


def synchronize(root, version, check=False):
    parse_version(version)
    changes = {}
    for name, (pattern, count) in PINS.items():
        path = root / name
        original = path.read_text()
        updated, found = re.subn(pattern, lambda m: m[1] + version, original)
        if found != count:
            raise ValueError(f"{name}: expected {count} version pins, found {found}; review updater")
        if original != updated:
            changes[path] = updated
    if check and changes:
        raise ValueError("Version pins disagree: " + ", ".join(str(p.relative_to(root)) for p in changes))
    # Validate every file before writing any of them.
    if not check:
        for path, content in changes.items():
            path.write_text(content)
        (root / "managed-codex-version.txt").write_text(version + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--check", action="store_true")
    mode.add_argument("--latest", action="store_true")
    mode.add_argument("--version")
    args = parser.parse_args()
    current = (ROOT / "managed-codex-version.txt").read_text().strip()
    synchronize(ROOT, current, check=True)
    if args.check:
        return
    candidate = args.version
    if args.latest:
        request = urllib.request.Request("https://registry.npmjs.org/@openai%2Fcodex/latest", headers={"User-Agent": "codex-broker-updater"})
        with urllib.request.urlopen(request, timeout=30) as response:
            metadata = json.load(response)
        if metadata.get("name") != "@openai/codex":
            raise ValueError("Unexpected npm package")
        candidate = metadata.get("version")
    newer = parse_version(candidate) > parse_version(current)
    if newer:
        synchronize(ROOT, candidate)
    print(f"Managed Codex: {current}; candidate: {candidate}; upgrade: {newer}")
    if output := os.environ.get("GITHUB_OUTPUT"):
        with open(output, "a") as stream:
            stream.write(f"current={current}\nversion={candidate}\nchanged={str(newer).lower()}\n")


if __name__ == "__main__":
    main()
