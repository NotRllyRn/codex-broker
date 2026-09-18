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
