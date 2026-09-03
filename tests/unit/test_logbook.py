import logging
from pathlib import Path

import pytest

import codex_broker.logbook as logbook_module
from codex_broker.logbook import LogBook


def test_logbook_repairs_partial_tail_and_rotates(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    active = tmp_path / "windowkeeper.jsonl"
    active.write_bytes(b'{"complete":true}\npartial')
    monkeypatch.setattr(logbook_module, "MAX_FILE_BYTES", 180)
    logbook = LogBook(tmp_path)
    logbook.start()
    try:
        for index in range(8):
            logbook.emit(
                logging.LogRecord(
                    "test",
                    logging.INFO,
                    __file__,
                    1,
                    "event %s with enough content to rotate",
                    (index,),
                    None,
                )
            )
    finally:
        logbook.close()
    assert list(tmp_path.glob("windowkeeper-*.jsonl"))
    assert active.read_bytes().endswith(b"\n")
