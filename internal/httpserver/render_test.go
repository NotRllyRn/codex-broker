package httpserver

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/webassets"
)

func TestTemplatesCompile(t *testing.T) {
	renderer, err := newRenderer("templates")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(webassets.Files, "templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".html") {
			continue
		}
		if _, err := renderer.templates.FromFile(entry.Name()); err != nil {
			t.Errorf("%s: %v", entry.Name(), err)
		}
	}
}
