package codex

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

const SupportedVersion = "codex-cli 0.145.0"

type Compatibility struct {
	Compatible                    bool
	Code, Detail, ObservedVersion string
}

func Inspect(executable string) Compatibility {
	path, err := exec.LookPath(executable)
	if err != nil {
		return Compatibility{Code: "CODEX_NOT_FOUND", Detail: "The managed Codex executable was not found"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return Compatibility{Code: "CODEX_INSPECTION_FAILED", Detail: "The managed Codex executable could not be inspected"}
	}
	observed := strings.TrimSpace(string(output))
	if observed != SupportedVersion {
		return Compatibility{Code: "CODEX_UNSUPPORTED", Detail: "Expected managed " + SupportedVersion + ", found " + observed, ObservedVersion: observed}
	}
	return Compatibility{Compatible: true, Code: "CODEX_AVAILABLE", Detail: "Managed Codex is available (" + observed + ")", ObservedVersion: observed}
}
