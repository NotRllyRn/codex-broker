import os
from pathlib import Path

import pytest

from windowkeeper.container_entrypoint import GROUP_ID, USER_ID, prepare_volumes


def test_prepare_volumes_changes_ownership_then_drops_privileges(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    paths = (tmp_path / "data", tmp_path / "run")
    ownership: list[tuple[Path, int, int]] = []
    dropped: list[tuple[str, object]] = []
    monkeypatch.setattr(os, "geteuid", lambda: 0)
    monkeypatch.setattr(os, "chown", lambda path, uid, gid: ownership.append((path, uid, gid)))
    monkeypatch.setattr(os, "setgroups", lambda groups: dropped.append(("groups", groups)))
    monkeypatch.setattr(os, "setgid", lambda gid: dropped.append(("gid", gid)))
    monkeypatch.setattr(os, "setuid", lambda uid: dropped.append(("uid", uid)))

    prepare_volumes(paths)

    assert all(path.is_dir() for path in paths)
    assert ownership == [(path, USER_ID, GROUP_ID) for path in paths]
    assert dropped == [("groups", []), ("gid", GROUP_ID), ("uid", USER_ID)]
