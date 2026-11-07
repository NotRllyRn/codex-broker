package settings

import (
	"strings"
	"testing"
)

func TestFieldValidation(t *testing.T) {
	for _, tc := range []struct {
		kind      string
		good, bad any
	}{
		{"text", "hello", 4}, {"textarea", "hello", false}, {"password", "value", nil},
		{"email", "example@example.com", "bad"}, {"url", "https://example.com", "relative"},
		{"tel", "+10000000000", 42}, {"path", "/tmp/example", false},
		{"number", 1.5, "1"}, {"integer", 2, 1.5}, {"slider", 50, 101},
		{"percentage", 70, -1}, {"ratio", 0.5, 2}, {"duration", 1000, -1},
		{"boolean", true, "true"}, {"select", "a", "b"}, {"radio", "a", "b"},
		{"multiselect", []string{"a"}, []string{"a", "a"}},
		{"color", "#aabbCC", "red"}, {"date", "2026-11-07", "2026-02-30"},
		{"time", "14:30", "25:01"}, {"datetime", "2026-11-07T14:30", "today"},
		{"string-list", []string{"one"}, []any{1}}, {"key-value", map[string]string{"a": "b"}, map[string]any{"a": 1}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			schema := Schema{Fields: []Field{{Key: "value", Type: tc.kind, Label: "Value", Default: JSON(tc.good), Options: []Option{{Value: "a", Label: "A"}}}}}
			if err := schema.Check(); err != nil {
				t.Fatal(err)
			}
			if _, err := schema.Validate(Values{"value": tc.good}); err != nil {
				t.Fatal(err)
			}
			if _, err := schema.Validate(Values{"value": tc.bad}); err == nil {
				t.Fatal("accepted invalid value")
			}
		})
	}
	schema := Schema{Fields: []Field{{Key: "data", Type: "json", Label: "Data", Default: JSON(nil)}}}
	if err := schema.Check(); err != nil {
		t.Fatal(err)
	}
	if _, err := schema.Validate(Values{"data": map[string]any{"list": []any{true, nil, 1}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := schema.Validate(Values{"unknown": true}); err == nil {
		t.Fatal("unknown key")
	}
	if _, err := schema.Validate(Values{"data": strings.Repeat("x", MaxBytes)}); err == nil {
		t.Fatal("oversized input")
	}
}
func TestSchemaConstraintsAndCopies(t *testing.T) {
	field := Field{Key: "count", Type: "integer", Label: "Count", Default: JSON(4), Min: Number(2), Max: Number(8), Step: Number(2)}
	s := Schema{Fields: []Field{field}}
	for _, v := range []any{1, 3, 9, nil} {
		if _, err := s.Validate(Values{"count": v}); err == nil {
			t.Fatal(v)
		}
	}
	if err := s.Check(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Field){
		func(f *Field) { f.Key = "bad-key" }, func(f *Field) { f.Type = "unknown" }, func(f *Field) { f.Default = nil },
		func(f *Field) { f.Step = Number(0) }, func(f *Field) { f.Max = Number(1) }, func(f *Field) { f.Pattern = "[" },
		func(f *Field) { f.VisibleWhen = &Condition{Key: "missing", Equals: true} }, func(f *Field) { f.Default = JSON(3) },
	} {
		f := field
		mutate(&f)
		if err := (Schema{Fields: []Field{f}}).Check(); err == nil {
			t.Fatal(f)
		}
	}
	if err := (Schema{Fields: []Field{field, field}}).Check(); err == nil {
		t.Fatal("duplicate")
	}
	text := Schema{Fields: []Field{{Key: "text", Type: "text", Label: "Text", Default: JSON("ab"), Required: true, MinLength: 2, MaxLength: 3, Pattern: "^[a-z]+$"}}}
	for _, v := range []string{"", "a", "abcd", "AB"} {
		if _, err := text.Validate(Values{"text": v}); err == nil {
			t.Fatal(v)
		}
	}
	values := Values{"data": map[string]any{"a": "before"}}
	nested := Schema{Fields: []Field{{Key: "data", Type: "json", Label: "Data", Default: JSON(nil)}}}
	output, err := nested.Validate(values)
	if err != nil {
		t.Fatal(err)
	}
	output["data"].(map[string]any)["a"] = "after"
	if values["data"].(map[string]any)["a"] != "before" {
		t.Fatal("shared state")
	}
}
