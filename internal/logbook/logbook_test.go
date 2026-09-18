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
