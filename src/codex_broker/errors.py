from dataclasses import dataclass


@dataclass(slots=True)
class WindowkeeperError(Exception):
    code: str
    detail: str
    status: int = 400

    def __str__(self) -> str:
        return self.detail


class Conflict(WindowkeeperError):
    def __init__(self, code: str, detail: str) -> None:
        super().__init__(code, detail, 409)


class Unavailable(WindowkeeperError):
    def __init__(self, code: str, detail: str) -> None:
        super().__init__(code, detail, 503)
