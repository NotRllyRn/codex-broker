import ipaddress
from functools import lru_cache
from pathlib import Path
from typing import Literal

from pydantic import Field, field_validator, model_validator
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(
        env_prefix="WINDOWKEEPER_", env_file=".env", env_file_encoding="utf-8", extra="ignore"
    )

    data_dir: Path = Path(".windowkeeper/data")
    runtime_dir: Path = Path(".windowkeeper/run")
    log_dir: Path | None = None
    host: str = "127.0.0.1"
    port: int = Field(default=8787, ge=1, le=65535)
    timezone: str = "UTC"
    root_path: str = ""
    trusted_proxies: str = ""
    tls_cert_file: Path | None = None
    tls_key_file: Path | None = None
    tls_ca_file: Path | None = None
    vault_key_file: Path | None = None
    vault_key: str | None = None
    admin_password_file: Path | None = None
    admin_password: str | None = None
    cookie_secure: Literal["auto", "true", "false"] = "auto"
    session_idle_minutes: int = Field(default=15, ge=1)
    session_absolute_hours: int = Field(default=8, ge=1)
    reauth_minutes: int = Field(default=5, ge=1)
    usage_poll_seconds: int = Field(default=300, ge=60)
    usage_timeout_seconds: int = Field(default=15, ge=1)
    usage_refresh_concurrency: int = Field(default=4, ge=1, le=16)
    auth_concurrency: int = Field(default=2, ge=1, le=8)
    process_start_concurrency: int = Field(default=2, ge=1, le=8)
    codex_idle_seconds: int = Field(default=30, ge=0)  # Deprecated; runtimes are always fresh.
    reset_padding_seconds: int = Field(default=10, ge=0, le=300)
    default_chatgpt_login_method: Literal["device-code", "browser"] = "device-code"
    # pi-lens-ignore: python-hardcoded-secrets
    browser_oauth_mode: Literal["disabled", "manual", "host-loopback"] = "manual"
    # pi-lens-ignore: python-hardcoded-secrets
    browser_oauth_callback_ports: str = "1455,1457"
    login_timeout_seconds: int = Field(default=900, ge=60, le=3600)
    browser_callback_max_bytes: int = Field(default=16384, ge=1024, le=65536)
    codex_executable: str = "codex"
    codex_version: str = "unknown"
    log_level: str = "INFO"

    @field_validator("root_path")
    @classmethod
    def valid_root_path(cls, value: str) -> str:
        if value == "/":
            return ""
        if value and (not value.startswith("/") or value.endswith("/")):
            raise ValueError("root_path must start with / and cannot end with /")
        return value

    @field_validator("trusted_proxies")
    @classmethod
    def valid_trusted_proxies(cls, value: str) -> str:
        if "*" in value:
            raise ValueError("trusted proxy wildcard is forbidden")
        try:
            for item in filter(None, (part.strip() for part in value.split(","))):
                ipaddress.ip_network(item, strict=False)
        except ValueError as error:
            raise ValueError("trusted proxies must be IP addresses or CIDR ranges") from error
        return value

    @field_validator("browser_oauth_callback_ports")
    @classmethod
    def valid_callback_ports(cls, value: str) -> str:
        try:
            ports = {int(part.strip()) for part in value.split(",")}
        except ValueError as error:
            raise ValueError("callback ports must be integers") from error
        if ports - {1455, 1457}:
            raise ValueError("callback ports must match the pinned compatibility profile")
        return value

    @model_validator(mode="after")
    def validate_paths_and_keys(self) -> "Settings":
        if self.data_dir.resolve() == self.runtime_dir.resolve():
            raise ValueError("persistent and runtime directories must differ")
        if self.vault_key_file and self.data_dir.resolve() in self.vault_key_file.resolve().parents:
            raise ValueError("vault key file cannot be under the data directory")
        self.log_dir = self.log_dir or self.data_dir / "logs"
        return self

    @property
    def callback_ports(self) -> tuple[int, ...]:
        try:
            return tuple(int(part.strip()) for part in self.browser_oauth_callback_ports.split(","))
        except ValueError as error:
            raise RuntimeError("validated callback ports became invalid") from error


@lru_cache(maxsize=1)
def get_settings() -> Settings:
    return Settings()
