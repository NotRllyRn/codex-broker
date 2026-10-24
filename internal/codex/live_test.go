package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt in with a real executable; ordinary unit tests remain offline.
func TestManagedCodexLive(t *testing.T) {
	executable := os.Getenv("CODEX_SMOKE_EXECUTABLE")
	if executable == "" {
		t.Skip("CODEX_SMOKE_EXECUTABLE is unset")
	}
	if compatibility := Inspect(executable); !compatibility.Compatible {
		t.Fatal(compatibility.Detail)
	}
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex")
	if err := os.Mkdir(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := Spawn(ctx, executable, home, []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home,
		"CODEX_HOME=" + codexHome, "TMPDIR=" + home,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	account, err := (Adapter{Client: client}).Account(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	value, exists := account["account"]
	if !exists || value != nil {
		t.Fatalf("expected explicit null account in isolated unauthenticated home: %#v", account)
	}
	if _, ok := account["requiresOpenaiAuth"].(bool); !ok {
		t.Fatalf("missing boolean requiresOpenaiAuth: %#v", account)
	}
}
