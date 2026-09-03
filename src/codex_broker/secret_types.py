from dataclasses import dataclass


@dataclass(frozen=True, slots=True)
class Secret:
    _value: str

    def reveal(self) -> str:
        return self._value

    def __repr__(self) -> str:
        return "Secret('[REDACTED]')"

    def __str__(self) -> str:
        return "[REDACTED]"
