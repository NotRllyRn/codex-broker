package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

func TestRuntimeRemovesPlaintextAfterFailureAndShutdown(t *testing.T) {
	root := t.TempDir()
	settings := config.Config{RuntimeDir: filepath.Join(root, "run"), CodexExecutable: filepath.Join(root, "missing"), ProcessStartConcurrency: 1}
	secrets, _ := vault.New(make([]byte, 32), "instance")
	manager := NewRuntimeManager(settings, secrets)
	if _, err := manager.Start(context.Background(), "failed", nil, nil); err == nil {
		t.Fatal("missing executable started")
	}
	failedRoots, _ := filepath.Glob(filepath.Join(settings.RuntimeDir, "accounts", "failed", "*"))
	if len(failedRoots) != 0 {
		t.Fatalf("failed runtime plaintext remains: %v", failedRoots)
	}
	script := filepath.Join(root, "codex")
	command := fmt.Sprintf("#!/bin/sh\nCODEX_FAKE_APP_SERVER=1 exec %s -test.run=TestFakeAppServerProcess -- \"$@\"\n", strconv.Quote(os.Args[0]))
	if err := os.WriteFile(script, []byte(command), 0o700); err != nil {
		t.Fatal(err)
	}
	settings.CodexExecutable = script
	manager = NewRuntimeManager(settings, secrets)
	runtime, err := manager.Start(context.Background(), "active", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.Close()
	if _, err := os.Stat(runtime.Root); !os.IsNotExist(err) {
		t.Fatalf("active runtime was not removed at shutdown: %v", err)
	}
}
