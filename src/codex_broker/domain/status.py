def overall_state(
    *,
    enabled: bool,
    auth_state: str,
    worker_state: str,
    usage_state: str,
    has_open_error: bool = False,
) -> str:
    if not enabled:
        return "DISABLED"
    if auth_state in {"AUTH_REQUIRED", "WORKSPACE_MISMATCH", "CREDENTIAL_ERROR"}:
        return "ACTION_REQUIRED"
    if has_open_error or worker_state == "CRASHED":
        return "ERROR"
    if worker_state == "STARTING" or auth_state in {"UNCONFIGURED", "ENROLLING"}:
        return "STARTING"
    if usage_state in {"AGING", "STALE", "ERROR"}:
        return "WARNING"
    return "HEALTHY"
