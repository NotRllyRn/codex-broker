package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestFakeAppServerProcess(t *testing.T) {
	if os.Getenv("CODEX_FAKE_APP_SERVER") != "1" {
		return
	}
	decoder := json.NewDecoder(bufio.NewReader(os.Stdin))
	encoder := json.NewEncoder(os.Stdout)
	for {
		var request map[string]any
		if decoder.Decode(&request) != nil {
			os.Exit(0)
		}
		id, hasID := request["id"]
		if !hasID {
			continue
		}
		switch request["method"] {
		case "initialize":
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"server": "fake"}})
		case "account/read":
			_ = encoder.Encode(map[string]any{"method": "test/notification", "params": map[string]any{"ok": true}})
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"account": map[string]any{"type": "chatgpt", "email": "owner@example.com"}}})
		case "model/list":
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"data": []any{
				map[string]any{"model": "alpha", "inputModalities": []string{"text"}, "supportedReasoningEfforts": []any{map[string]string{"reasoningEffort": "low"}}},
				map[string]any{"model": "test-mini", "inputModalities": []string{"text"}, "supportedReasoningEfforts": []any{map[string]string{"reasoningEffort": "low"}, map[string]string{"reasoningEffort": "minimal"}}},
			}}})
		case "thread/start":
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"thread": map[string]string{"id": "thread"}}})
		case "turn/start":
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"turn": map[string]string{"id": "turn"}}})
			_ = encoder.Encode(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "other-thread", "turnId": "turn", "item": map[string]string{"type": "agentMessage", "text": "unrelated"}}})
			status := os.Getenv("CODEX_FAKE_PULSE_STATUS")
			if status == "" {
				status = "completed"
			}
			item := map[string]string{"type": "agentMessage", "text": "OK"}
			if os.Getenv("CODEX_FAKE_PULSE_ITEMS_ONLY") != "1" {
				_ = encoder.Encode(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread", "turnId": "turn", "item": item}})
			}
			_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]any{"id": "turn", "status": status, "items": []any{item}, "error": map[string]string{"message": os.Getenv("CODEX_FAKE_PULSE_ERROR")}}}})
		case "account/rateLimits/read":
			_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{"rateLimits": map[string]any{"primary": map[string]any{"usedPercent": 1}}}})
		case "account/login/start":
			_ = encoder.Encode(map[string]any{"id": id, "error": map[string]any{"message": "device auth failed with status 429 Too Many Requests"}})
		case "auth/test":
			_ = encoder.Encode(map[string]any{"id": id, "error": map[string]any{"message": "authentication required"}})
		}
	}
}

func TestClientProtocolAndAuthClassification(t *testing.T) {
	client, err := Spawn(context.Background(), os.Args[0], t.TempDir(), []string{"PATH=" + os.Getenv("PATH"), "CODEX_FAKE_APP_SERVER=1"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.Request(context.Background(), "account/read", map[string]any{"refreshToken": false})
	if err != nil || result["account"] == nil {
		t.Fatalf("account/read = %#v, %v", result, err)
	}
	select {
	case notification := <-client.Notifications():
		if notification["method"] != "test/notification" {
			t.Fatalf("notification = %#v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("notification was not delivered")
	}
	if _, err := client.Request(context.Background(), "auth/test", nil); err == nil || err.Error() != "Codex authentication must be renewed" {
		t.Fatalf("auth error = %v", err)
	}
}

func TestClientPreservesLoginStartError(t *testing.T) {
	client, err := Spawn(context.Background(), os.Args[0], t.TempDir(), []string{"PATH=" + os.Getenv("PATH"), "CODEX_FAKE_APP_SERVER=1"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.Request(context.Background(), "account/login/start", map[string]any{"type": "chatgptDeviceCode"})
	if err == nil || err.Error() != "device auth failed with status 429 Too Many Requests" {
		t.Fatalf("login error=%v", err)
	}
}
