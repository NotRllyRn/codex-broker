import os
from pathlib import Path

USER_ID = GROUP_ID = 10001
VOLUME_PATHS = (Path("/data"), Path("/run/windowkeeper"))


def prepare_volumes(paths: tuple[Path, ...] = VOLUME_PATHS) -> None:
    if os.geteuid() != 0:
        return
    for path in paths:
        path.mkdir(parents=True, exist_ok=True)
        os.chown(path, USER_ID, GROUP_ID)
    os.setgroups([])
    os.setgid(GROUP_ID)
    os.setuid(USER_ID)


def main() -> None:
    prepare_volumes()
    from codex_broker.cli.main import cli

    cli(prog_name="codex-broker")


if __name__ == "__main__":
    main()
