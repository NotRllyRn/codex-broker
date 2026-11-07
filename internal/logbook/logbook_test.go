package logbook

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepairsPartialTailAndRotates(t *testing.T) {
	directory := t.TempDir()
	active := filepath.Join(directory, "windowkeeper.jsonl")
	if err := os.WriteFile(active, []byte("{\"complete\":true}\npartial"), 0o600); err != nil {
		t.Fatal(err)
	}
	book, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	book.size = maxFileBytes
	book.Log("INFO", "test.event", "rotating")
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(active)
	if len(content) == 0 || content[len(content)-1] != '\n' {
		t.Fatalf("active log was not repaired: %q", content)
	}
	rotated, _ := filepath.Glob(filepath.Join(directory, "windowkeeper-*.jsonl"))
	if len(rotated) != 1 {
		t.Fatalf("rotated logs = %v", rotated)
	}
}

func TestRejectsSymlinkedActiveLog(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	_ = os.WriteFile(target, nil, 0o600)
	if err := os.Symlink(target, filepath.Join(directory, "windowkeeper.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(directory); err == nil {
		t.Fatal("symlinked active log was accepted")
	}
}

func TestStructuredLogsSurviveRestartAndRotation(t *testing.T) {
	directory := t.TempDir()
	book, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	book.LogFields("WARNING", "proxy.extension.log", "example message", map[string]any{"extension_id": "example", "fields": map[string]any{"token": "secret", "response_id": "response"}, "level": "forged", "message": "forged"})
	book.size = maxFileBytes
	book.Log("INFO", "after.rotation", "next")
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	logs := reopened.Recent("WARNING", "example", 10)
	if len(logs) != 1 || logs[0]["level"] != "WARNING" || logs[0]["message"] != "example message" || logs[0]["fields"].(map[string]any)["token"] != "[REDACTED]" {
		t.Fatal(logs)
	}
	if got := reopened.Recent("", "", 10); len(got) != 2 || got[0]["event"] != "after.rotation" {
		t.Fatal(got)
	}
}
