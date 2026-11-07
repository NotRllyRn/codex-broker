// Package settings defines declarative, host-rendered extension settings.
package settings

import (
	"encoding/json"
	"fmt"
	"math"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxBytes = 64 << 10

var keyPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,63}$`)
var kinds = map[string]bool{}

func init() {
	for _, kind := range strings.Fields("text textarea password email url tel path number integer slider percentage ratio duration boolean select radio multiselect color date time datetime string-list key-value json") {
		kinds[kind] = true
	}
}

type Values map[string]any
type Schema struct {
	Title       string  `json:"title,omitempty"`
	Description string  `json:"description,omitempty"`
	Fields      []Field `json:"fields"`
}
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}
type Condition struct {
	Key    string `json:"key"`
	Equals any    `json:"equals"`
}
type Field struct {
	Key         string          `json:"key"`
	Type        string          `json:"type"`
	Label       string          `json:"label"`
	Default     json.RawMessage `json:"default"`
	Description string          `json:"description,omitempty"`
	Placeholder string          `json:"placeholder,omitempty"`
	Group       string          `json:"group,omitempty"`
	Unit        string          `json:"unit,omitempty"`
	Required    bool            `json:"required,omitempty"`
	Min         *float64        `json:"min,omitempty"`
	Max         *float64        `json:"max,omitempty"`
	Step        *float64        `json:"step,omitempty"`
	MinLength   int             `json:"minLength,omitempty"`
	MaxLength   int             `json:"maxLength,omitempty"`
	Pattern     string          `json:"pattern,omitempty"`
	Options     []Option        `json:"options,omitempty"`
	VisibleWhen *Condition      `json:"visibleWhen,omitempty"`
}

func JSON(value any) json.RawMessage { b, _ := json.Marshal(value); return b }
func Number(value float64) *float64  { return &value }
func (s Schema) Defaults() Values {
	values := Values{}
	for _, f := range s.Fields {
		var value any
		_ = json.Unmarshal(f.Default, &value)
		values[f.Key] = value
	}
	return values
}
func (s Schema) Check() error {
	raw, err := json.Marshal(s)
	if err != nil || len(raw) > MaxBytes || len(s.Fields) > 64 {
		return fmt.Errorf("settings schema exceeds limits")
	}
	known := map[string]Field{}
	for _, f := range s.Fields {
		if !keyPattern.MatchString(f.Key) || !kinds[f.Type] || f.Label == "" || len(f.Default) == 0 {
			return fmt.Errorf("invalid settings field %q", f.Key)
		}
		if _, ok := known[f.Key]; ok {
			return fmt.Errorf("duplicate settings field %q", f.Key)
		}
		known[f.Key] = f
		if f.MinLength < 0 || f.MaxLength < 0 || f.MaxLength > MaxBytes || f.MinLength > MaxBytes || f.MaxLength > 0 && f.MinLength > f.MaxLength {
			return fmt.Errorf("invalid length constraints for %s", f.Key)
		}
		if f.Min != nil && (!finite(*f.Min)) || f.Max != nil && !finite(*f.Max) || f.Step != nil && (!finite(*f.Step) || *f.Step <= 0) || f.Min != nil && f.Max != nil && *f.Min > *f.Max {
			return fmt.Errorf("invalid numeric constraints for %s", f.Key)
		}
		if _, err := regexp.Compile(f.Pattern); err != nil {
			return fmt.Errorf("invalid pattern for %s", f.Key)
		}
		if len(f.Options) > 256 {
			return fmt.Errorf("too many options for %s", f.Key)
		}
		seen := map[string]bool{}
		for _, option := range f.Options {
			if seen[option.Value] || option.Label == "" {
				return fmt.Errorf("invalid options for %s", f.Key)
			}
			seen[option.Value] = true
		}
		if (f.Type == "select" || f.Type == "radio" || f.Type == "multiselect") && len(f.Options) == 0 {
			return fmt.Errorf("options required for %s", f.Key)
		}
	}
	for _, f := range s.Fields {
		if c := f.VisibleWhen; c != nil {
			other, ok := known[c.Key]
			if !ok || c.Key == f.Key || other.Type == "password" {
				return fmt.Errorf("invalid condition for %s", f.Key)
			}
			if err := other.validate(c.Equals); err != nil {
				return fmt.Errorf("invalid condition for %s", f.Key)
			}
		}
	}
	_, err = s.Validate(s.Defaults())
	return err
}
func (s Schema) Validate(input Values) (Values, error) {
	raw, err := json.Marshal(input)
	if err != nil || len(raw) > MaxBytes {
		return nil, fmt.Errorf("settings exceed 64 KiB")
	}
	var normalizedInput Values
	if err := json.Unmarshal(raw, &normalizedInput); err != nil {
		return nil, err
	}
	input = normalizedInput
	result := s.Defaults()
	known := map[string]bool{}
	for _, f := range s.Fields {
		known[f.Key] = true
		if value, ok := input[f.Key]; ok {
			result[f.Key] = value
		}
		if err := f.validate(result[f.Key]); err != nil {
			return nil, fmt.Errorf("%s: %s", f.Label, err)
		}
	}
	for key := range input {
		if !known[key] {
			return nil, fmt.Errorf("unknown setting %q", key)
		}
	}
	raw, err = json.Marshal(result)
	if err != nil || len(raw) > MaxBytes {
		return nil, fmt.Errorf("settings exceed 64 KiB")
	}
	// Never return maps or arrays owned by the caller.
	var normalized Values
	err = json.Unmarshal(raw, &normalized)
	return normalized, err
}
func finite(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) }
func (f Field) bounds() (float64, float64) {
	lo, hi := math.Inf(-1), math.Inf(1)
	switch f.Type {
	case "slider", "percentage":
		lo, hi = 0, 100
	case "ratio":
		lo, hi = 0, 1
	case "duration":
		lo = 0
	}
	if f.Min != nil {
		lo = *f.Min
	}
	if f.Max != nil {
		hi = *f.Max
	}
	return lo, hi
}
func (f Field) validate(value any) error {
	switch f.Type {
	case "number", "integer", "slider", "percentage", "ratio", "duration":
		number, ok := value.(float64)
		if !ok || !finite(number) {
			return fmt.Errorf("enter a finite number")
		}
		lo, hi := f.bounds()
		if number < lo || number > hi {
			return fmt.Errorf("number is outside the allowed range")
		}
		if f.Type == "integer" && math.Trunc(number) != number {
			return fmt.Errorf("enter a whole number")
		}
		if f.Step != nil {
			base := 0.0
			if finite(lo) {
				base = lo
			}
			steps := (number - base) / *f.Step
			if math.Abs(steps-math.Round(steps)) > 1e-7*math.Max(1, math.Abs(steps)) {
				return fmt.Errorf("number does not match the step")
			}
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("choose on or off")
		}
	case "json": // Any bounded JSON value is supported, including null.
	case "key-value":
		values, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("enter an object of text values")
		}
		for _, v := range values {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("map values must be text")
			}
		}
		if f.Required && len(values) == 0 {
			return fmt.Errorf("a value is required")
		}
	case "string-list", "multiselect":
		values, ok := value.([]any)
		if !ok {
			return fmt.Errorf("enter a list of text values")
		}
		if f.Required && len(values) == 0 {
			return fmt.Errorf("a selection is required")
		}
		seen := map[string]bool{}
		for _, v := range values {
			str, ok := v.(string)
			if !ok {
				return fmt.Errorf("list values must be text")
			}
			if f.Type == "multiselect" && (!f.hasOption(str) || seen[str]) {
				return fmt.Errorf("choose valid, unique options")
			}
			seen[str] = true
		}
	default:
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("enter text")
		}
		length := utf8.RuneCountInString(text)
		if f.Required && strings.TrimSpace(text) == "" {
			return fmt.Errorf("a value is required")
		}
		if length < f.MinLength || f.MaxLength > 0 && length > f.MaxLength {
			return fmt.Errorf("text length is outside the allowed range")
		}
		if text == "" && !f.Required {
			return nil
		}
		if pattern, err := regexp.Compile(f.Pattern); err != nil || f.Pattern != "" && !pattern.MatchString(text) {
			return fmt.Errorf("text does not match the required pattern")
		}
		switch f.Type {
		case "select", "radio":
			if !f.hasOption(text) {
				return fmt.Errorf("choose a valid option")
			}
		case "color":
			if !regexp.MustCompile(`^#[0-9a-fA-F]{6}$`).MatchString(text) {
				return fmt.Errorf("use a six-digit hex color")
			}
		case "email":
			address, err := mail.ParseAddress(text)
			if err != nil || address.Address != text {
				return fmt.Errorf("enter an email address")
			}
		case "url":
			parsed, err := url.ParseRequestURI(text)
			if err != nil || parsed.Scheme == "" {
				return fmt.Errorf("enter an absolute URL")
			}
		case "date":
			if !parseTime(text, "2006-01-02") {
				return fmt.Errorf("enter a date")
			}
		case "time":
			if !parseTime(text, "15:04", "15:04:05") {
				return fmt.Errorf("enter a time")
			}
		case "datetime":
			if !parseTime(text, "2006-01-02T15:04", "2006-01-02T15:04:05") {
				return fmt.Errorf("enter a local date and time")
			}
		}
	}
	return nil
}
func (f Field) hasOption(value string) bool {
	for _, option := range f.Options {
		if option.Value == value {
			return true
		}
	}
	return false
}
func parseTime(value string, layouts ...string) bool {
	for _, layout := range layouts {
		if _, err := time.Parse(layout, value); err == nil {
			return true
		}
	}
	return false
}
