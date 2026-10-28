package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestQuotaWindows(t *testing.T) {
	window := func(used float64) map[string]any {
		return map[string]any{"usedPercent": used, "resetsAt": float64(time.Now().Add(time.Hour).Unix())}
	}
	for _, tc := range []struct {
		name    string
		limits  map[string]any
		wantErr bool
	}{
		{"both available", map[string]any{"primary": window(10), "secondary": window(20)}, false},
		{"primary only", map[string]any{"primary": window(10), "secondary": nil}, false},
		{"secondary only", map[string]any{"primary": nil, "secondary": window(20)}, false},
		{"no windows", map[string]any{}, true},
		{"exhausted secondary", map[string]any{"primary": window(10), "secondary": window(100)}, true},
		{"malformed supplied window", map[string]any{"primary": window(10), "secondary": "invalid"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := quotaAvailable(map[string]any{"rateLimits": tc.limits})
			if (err != nil) != tc.wantErr {
				t.Fatalf("quota validation = %v, want error=%t", err, tc.wantErr)
			}
		})
	}
}

func TestAuthenticatedWorkflow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("wrapper uses POSIX shell")
	}
	for _, scenario := range []string{"renewed", "unchanged", "denied"} {
		t.Run(scenario, func(t *testing.T) {
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
			wrapper := filepath.Join(t.TempDir(), "codex")
			script := "#!/bin/sh\ncase \"$1\" in\n--version) echo 'codex-cli 1.2.3';;\napp-server) exec " + quote(binary) + " -test.run=TestFakeAppServer -- " + scenario + ";;\nesac\n"
			if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var out bytes.Buffer
			err = run(ctx, wrapper, "1.2.3", &out)
			switch scenario {
			case "renewed":
				if err != nil || !strings.Contains(out.String(), "all authenticated checks completed") {
					t.Fatalf("workflow failed: %v\n%s", err, out.String())
				}
			case "unchanged":
				if err == nil || !strings.Contains(err.Error(), "inconclusive") {
					t.Fatalf("unchanged credentials must not pass renewal: %v", err)
				}
			case "denied":
				if err == nil || !strings.Contains(err.Error(), "rejected") {
					t.Fatalf("denied login must fail: %v", err)
				}
			}
			for _, secret := range []string{"example-initial-access", "example-initial-refresh", "example-renewed-access", "example-renewed-refresh", "user@example.com"} {
				if strings.Contains(out.String(), secret) || (err != nil && strings.Contains(err.Error(), secret)) {
					t.Fatal("output exposed a credential or identity")
				}
			}
		})
	}
}

// The final fake process requires the renewed token from the checkpoint, which
// detects accidentally restoring the original login credential after refresh.
func TestFakeAppServer(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	scenario := os.Args[len(os.Args)-1]
	encoder := json.NewEncoder(os.Stdout)
	send := func(v any) { _ = encoder.Encode(v) }
	notify := func(method string, params any) { send(map[string]any{"method": method, "params": params}) }
	authPath := filepath.Join(os.Getenv("CODEX_HOME"), "auth.json")
	writeAuth := func(renewed bool) {
		prefix := "example-initial-"
		if renewed {
			prefix = "example-renewed-"
		}
		raw, _ := json.Marshal(map[string]any{"tokens": map[string]any{"access_token": prefix + "access", "refresh_token": prefix + "refresh"}})
		if os.WriteFile(authPath, raw, 0600) != nil {
			os.Exit(2)
		}
	}
	for scanner := bufio.NewScanner(os.Stdin); scanner.Scan(); {
		var request struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(2)
		}
		if request.ID == nil {
			continue
		}
		result := map[string]any{}
		switch request.Method {
		case "initialize":
		case "account/login/start":
			result = map[string]any{"loginId": "example-login", "verificationUrl": "https://example.com/device", "userCode": "EXAMPLE-CODE"}
		case "account/read":
			if request.Params["refreshToken"] == true && scenario == "renewed" {
				writeAuth(true)
			}
			raw, err := os.ReadFile(authPath)
			if err != nil || (filepath.Base(filepath.Dir(os.Getenv("CODEX_HOME"))) == "restored" && !bytes.Contains(raw, []byte("example-renewed-access"))) {
				os.Exit(2)
			}
			result = map[string]any{"account": map[string]any{"type": "chatgpt", "email": "user@example.com", "planType": "plus"}}
		case "account/rateLimits/read":
			window := map[string]any{"usedPercent": 10, "resetsAt": time.Now().Add(time.Hour).Unix()}
			result = map[string]any{"rateLimits": map[string]any{"primary": window, "secondary": window}}
		case "model/list":
			result = map[string]any{"data": []any{map[string]any{"model": "example-mini", "inputModalities": []string{"text"}, "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "low"}}}}}
		case "thread/start":
			result = map[string]any{"thread": map[string]any{"id": "example-thread"}}
		case "turn/start":
			result = map[string]any{"turn": map[string]any{"id": "example-turn"}}
		default:
			os.Exit(2)
		}
		send(map[string]any{"id": request.ID, "result": result})
		if request.Method == "account/login/start" {
			if scenario != "denied" {
				writeAuth(false)
			}
			notify("account/login/completed", map[string]any{"loginId": "example-login", "success": scenario != "denied"})
		}
		if request.Method == "turn/start" {
			notify("turn/completed", map[string]any{"turn": map[string]any{"id": "example-turn", "status": "completed", "items": []any{map[string]any{"type": "agentMessage", "text": "OK"}}}})
		}
	}
	os.Exit(0)
}
