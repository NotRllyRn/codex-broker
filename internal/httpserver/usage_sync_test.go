package httpserver

import (
	"bytes"
	"encoding/pem"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/identity"
	"github.com/NotRllyRn/codex-broker/internal/proxytelemetry"
	"github.com/NotRllyRn/codex-broker/internal/usage"
)

// Exercise the packaged executable and actual pinned ccusage against verified TLS.
func TestNativeUsageSyncHTTPS(t *testing.T) {
	collector := os.Getenv("CCUSAGE_TEST_BINARY")
	if collector == "" {
		t.Skip("set CCUSAGE_TEST_BINARY to pinned native ccusage for integration test")
	}
	s, _ := proxyFixture(t, 0)
	owner, code, err := identity.Create(t.Context(), s.App.Store, "Native test")
	if err != nil {
		t.Fatal(err)
	}
	s.App.Config.RootPath = "/broker"
	var offline atomic.Bool
	handler := s.rootPath(s.middleware(s.mux))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			w.WriteHeader(503)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.Mkdir(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(bundle, "codex-broker-usage")
	command := exec.CommandContext(t.Context(), "go", "build", "-o", helper, "../../cmd/codex-broker-usage")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	data, err := os.ReadFile(collector)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "ccusage"), data, 0700); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(root, ".pi", "agent", "sessions", "project")
	if err := os.MkdirAll(logs, 0700); err != nil {
		t.Fatal(err)
	}
	pi := `{"type":"message","id":"event1","timestamp":"2026-09-01T00:00:00Z","message":{"role":"assistant","model":"gpt-5","provider":"openai","usage":{"input":10,"cacheRead":20,"output":7}}}` + "\n"
	if err := os.WriteFile(filepath.Join(logs, "session.jsonl"), []byte(pi), 0600); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(root, ".claude", "projects", "test")
	if err := os.MkdirAll(claude, 0700); err != nil {
		t.Fatal(err)
	}
	row := `{"timestamp":"2026-09-01T00:00:00Z","sessionId":"test","message":{"id":"claude-event","model":"claude-sonnet-4-20250514","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n"
	if err := os.WriteFile(filepath.Join(claude, "session.jsonl"), []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	run := func(args ...string) ([]byte, error) {
		t.Helper()
		command := exec.CommandContext(t.Context(), helper, args...)
		command.Env = []string{"HOME=" + root, "PATH=" + os.Getenv("PATH"), "CODEX_HOME=" + filepath.Join(root, ".codex"), "PI_CODING_AGENT_DIR=" + filepath.Join(root, ".pi", "agent"), "CLAUDE_CONFIG_DIR=" + filepath.Join(root, ".claude"), "XDG_DATA_HOME=" + filepath.Join(root, ".local", "share")}
		command.Stdin = strings.NewReader(code + "\n")
		return command.CombinedOutput()
	}
	if output, err := run("install", "--state-dir", state, "--broker-url", server.URL+"/broker", "--ca", ca, "--code-stdin", "--no-schedule"); err != nil {
		t.Fatalf("install: %v %s", err, output)
	}
	// No source environment is needed now: installation persisted it for scheduled runs.
	for range 2 {
		command := exec.CommandContext(t.Context(), filepath.Join(state, "codex-broker-usage"), "sync", "--state-dir", state)
		command.Env = []string{"HOME=" + root, "PATH=" + os.Getenv("PATH")}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("sync: %v %s", err, output)
		}
	}
	// Old agent-specific data remains recoverable but must not add to ccusage totals.
	if _, err := s.App.Store.DB.Exec(`INSERT INTO identity_usage VALUES(?, 'legacy', NULL, 'codex', 1792497600000, 'old-model', 'default', 10, 0, 0, 999, 0, 1)`, owner.ID); err != nil {
		t.Fatal(err)
	}
	summary, err := usage.ReadIdentitySummary(t.Context(), s.App.Store.DB, nil, owner.ID, "all", 0, time.Now())
	if err != nil || summary.Output != 12 || summary.Reports != 1 || summary.Responses != 0 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	if len(summary.Models) != 2 {
		t.Fatalf("uploaded model breakdown missing: %+v", summary.Models)
	}
	var modelTokens int64
	var modelCost float64
	for _, model := range summary.Models {
		modelTokens += model.ProcessedTokens
		modelCost += model.CostUSD
		if !model.CostKnown {
			t.Fatal("ccusage model cost missing")
		}
	}
	if modelTokens != summary.ProcessedTokens || math.Abs(modelCost-summary.CostUSD) > 1e-9 {
		t.Fatal("model breakdown does not match uploaded totals", summary.Models)
	}
	config, err := os.ReadFile(filepath.Join(state, "config.json"))
	if err != nil || bytes.Contains(config, []byte(code)) {
		t.Fatal("code retained", err)
	}
	offline.Store(true)
	file, err := os.OpenFile(filepath.Join(logs, "session.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(file, strings.ReplaceAll(strings.ReplaceAll(pi, "event1", "event2"), `"output":7`, `"output":3`))
	_ = file.Close()
	if _, err := run("sync", "--state-dir", state); err == nil {
		t.Fatal("offline run succeeded")
	}
	offline.Store(false)
	if output, err := run("sync", "--state-dir", state); err != nil {
		t.Fatalf("recovery: %v %s", err, output)
	}
	if err := proxytelemetry.Prune(t.Context(), s.App.Store, time.Now().Add(time.Hour).UnixMilli(), 100); err != nil {
		t.Fatal(err)
	}
	summary, err = usage.ReadIdentitySummary(t.Context(), s.App.Store.DB, nil, owner.ID, "all", 0, time.Now())
	if err != nil || summary.Output != 15 || summary.Reports != 1 {
		t.Fatalf("recovery summary=%+v err=%v", summary, err)
	}
}
