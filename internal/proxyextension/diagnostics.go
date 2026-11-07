package proxyextension

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NotRllyRn/codex-broker/extensions"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/logbook"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

const MaxLogBytes = 16 << 10
const MaxRequestLogs = 100
const MaxRequestLogBytes = 256 << 10
const errorPrefix = "proxy_extension_error:"

type LogEntry struct {
	ID          string `json:"id"`
	ExtensionID string `json:"extension_id"`
	Level       string `json:"level"`
	Hook        string `json:"hook"`
	Message     string `json:"message"`
	AtMS        int64  `json:"at_ms"`
	Context
	Fields  map[string]any `json:"fields,omitempty"`
	Failed  bool           `json:"failed,omitempty"`
	RelayID string         `json:"relay_id,omitempty"`
}
type LogSink func(LogEntry)

func (e LogEntry) LastError() *extensions.LastError {
	return &extensions.LastError{At: time.UnixMilli(e.AtMS).UTC().Format(time.RFC3339Nano), Hook: e.Hook, Message: e.Message}
}
func (e LogEntry) Validate() error {
	raw, err := json.Marshal(e)
	if err != nil || len(raw) > MaxLogBytes+4096 || !validID.MatchString(e.ExtensionID) || e.ID == "" || e.AtMS <= 0 {
		return fmt.Errorf("invalid extension log")
	}
	switch e.Level {
	case "DEBUG", "INFO", "WARNING", "ERROR":
		return nil
	}
	return fmt.Errorf("invalid extension log level")
}
func redactKnown(value any, secrets []string) any {
	switch v := value.(type) {
	case string:
		for _, secret := range secrets {
			if secret != "" {
				v = strings.ReplaceAll(v, secret, "[REDACTED]")
			}
		}
		return v
	case map[string]any:
		for key, item := range v {
			v[key] = redactKnown(item, secrets)
		}
	case []any:
		for i, item := range v {
			v[i] = redactKnown(item, secrets)
		}
	}
	return value
}
func (s *Session) writeLog(in *instance, level, message string, fields map[string]any, failed bool, timestamps ...int64) {
	s.rememberSecrets(in)
	secrets := in.secrets
	data := logbook.Redact(redactKnown(map[string]any{"message": message, "fields": fields}, secrets)).(map[string]any)
	entry := LogEntry{ID: core.NewID(), ExtensionID: in.id, Level: level, Hook: in.hook, Message: data["message"].(string), AtMS: time.Now().UnixMilli(), Context: s.identity, Failed: failed}
	if len(timestamps) > 0 && timestamps[0] > 0 {
		entry.AtMS = timestamps[0]
	}
	entry.Fields, _ = data["fields"].(map[string]any)
	if len(entry.Message) > 8192 {
		entry.Message = entry.Message[:8192]
		for !utf8.ValidString(entry.Message) {
			entry.Message = entry.Message[:len(entry.Message)-1]
		}
	}
	if raw, _ := json.Marshal(entry); len(raw) > MaxLogBytes {
		entry.Fields = map[string]any{"truncated": true}
	}
	for {
		raw, _ := json.Marshal(entry)
		if len(raw) <= MaxLogBytes || len(entry.Message) == 0 {
			break
		}
		entry.Message = entry.Message[:len(entry.Message)/2]
		for !utf8.ValidString(entry.Message) {
			entry.Message = entry.Message[:len(entry.Message)-1]
		}
	}
	s.manager.record(entry, s.sink)
}
func (m *Manager) record(entry LogEntry, sink LogSink) {
	if m != nil && entry.Level == "ERROR" {
		m.diagnosticsMu.Lock()
		if m.latest == nil {
			m.latest = map[string]LogEntry{}
		}
		if entry.AtMS >= m.latest[entry.ExtensionID].AtMS {
			m.latest[entry.ExtensionID] = entry
		}
		m.diagnosticsMu.Unlock()
	}
	// Diagnostics must not turn a skipped extension failure into a proxy failure.
	if sink != nil {
		func() { defer func() { _ = recover() }(); sink(entry) }()
	}
}
func (s *Session) flushLogs(in *instance) {
	for _, entry := range in.logs {
		s.writeLog(in, entry.Level, entry.Message, entry.Fields, false, entry.AtMS)
	}
	in.logs = nil
}
func (s *Session) rememberSecrets(in *instance) {
	schema, _ := s.manager.Schema(in.id)
	values := decodeSettings(in.settings)
	for _, field := range schema.Fields {
		if field.Type != "password" {
			continue
		}
		secret, _ := values[field.Key].(string)
		if secret == "" {
			continue
		}
		found := false
		for _, known := range in.secrets {
			if known == secret {
				found = true
				break
			}
		}
		if !found {
			in.secrets = append(in.secrets, secret)
		}
	}
}
func (in *instance) errorText(err error) string { return err.Error() }
func (s *Session) failed(in *instance, hook string, err error) {
	in.hook = hook
	s.writeLog(in, "ERROR", in.errorText(err), nil, true)
	in.failed = true
}

// SaveLastError retains diagnostics independently of rolling log retention.
func SaveLastError(ctx context.Context, db *store.Store, entry LogEntry) error {
	if entry.Level != "ERROR" {
		return nil
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = db.DB.ExecContext(ctx, `INSERT INTO settings(setting_key,value_json,updated_at_ms) VALUES(?,?,?) ON CONFLICT(setting_key) DO UPDATE SET value_json=excluded.value_json,updated_at_ms=excluded.updated_at_ms WHERE excluded.updated_at_ms >= settings.updated_at_ms`, errorPrefix+entry.ExtensionID, string(raw), entry.AtMS)
	return err
}
func LastErrors(ctx context.Context, db *store.Store) (map[string]LogEntry, error) {
	rows, err := db.DB.QueryContext(ctx, "SELECT value_json FROM settings WHERE setting_key GLOB 'proxy_extension_error:*'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]LogEntry{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var entry LogEntry
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			return nil, err
		}
		result[entry.ExtensionID] = entry
	}
	return result, rows.Err()
}

func logFields(vmValue string) (string, string, map[string]any, error) {
	var entry struct {
		Level, Message string
		Fields         map[string]any
	}
	if len(vmValue) > MaxLogBytes {
		return "", "", nil, fmt.Errorf("log entry exceeds 16 KiB")
	}
	if err := json.Unmarshal([]byte(vmValue), &entry); err != nil {
		return "", "", nil, err
	}
	level := strings.ToUpper(entry.Level)
	if level == "WARN" {
		level = "WARNING"
	}
	switch level {
	case "DEBUG", "INFO", "WARNING", "ERROR":
	default:
		return "", "", nil, fmt.Errorf("invalid log level")
	}
	return level, entry.Message, entry.Fields, nil
}

// ReportLoadErrors publishes discovery failures once a host sink is available.
func (m *Manager) ReportLoadErrors(sink LogSink) {
	if m == nil || sink == nil {
		return
	}
	m.loadReports.Do(func() {
		m.diagnosticsMu.RLock()
		var entries []LogEntry
		for _, entry := range m.latest {
			if entry.Hook == "load" {
				entries = append(entries, entry)
			}
		}
		m.diagnosticsMu.RUnlock()
		for _, entry := range entries {
			m.record(entry, sink)
		}
	})
}
