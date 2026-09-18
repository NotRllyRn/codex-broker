package httpserver

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"sync"

	"github.com/NotRllyRn/codex-broker/internal/webassets"
	"github.com/flosch/pongo2/v6"
)

type renderer struct{ templates *pongo2.TemplateSet }

var filtersOnce sync.Once

func newRenderer(directory string) (*renderer, error) {
	filtersOnce.Do(func() {
		_ = pongo2.RegisterFilter("humanize", func(input, _ *pongo2.Value) (*pongo2.Value, *pongo2.Error) {
			value := strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(input.String())
			return pongo2.AsValue(value), nil
		})
		_ = pongo2.RegisterFilter("json", func(input, _ *pongo2.Value) (*pongo2.Value, *pongo2.Error) {
			value, _ := json.MarshalIndent(input.Interface(), "", "  ")
			return pongo2.AsValue(string(value)), nil
		})
	})
	root, err := fs.Sub(webassets.Files, directory)
	if err != nil {
		return nil, err
	}
	set := pongo2.NewSet(directory, pongo2.NewFSLoader(root))
	return &renderer{set}, nil
}

func (r *renderer) render(writer http.ResponseWriter, status int, name string, context pongo2.Context) error {
	template, err := r.templates.FromFile(name)
	if err != nil {
		return err
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(status)
	return template.ExecuteWriter(context, writer)
}
