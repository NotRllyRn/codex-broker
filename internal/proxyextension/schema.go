package proxyextension

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/NotRllyRn/codex-broker/extensions/settings"
)

func readSchema(def Definition) (settings.Schema, error) {
	worker := newWorker()
	defer worker.close()
	reply, _, err := worker.call(context.Background(), 0, map[string]any{"op": "schema", "code": def.code, "filename": def.filename}, nil)
	var schema settings.Schema
	if err == nil {
		if len(reply["schema"]) > settings.MaxBytes {
			return schema, fmt.Errorf("invalid schema")
		}
		err = json.Unmarshal(reply["schema"], &schema)
	}
	if err == nil {
		err = schema.Check()
	}
	if err != nil {
		return settings.Schema{}, fmt.Errorf("extension %s has an invalid settings declaration or module initialization: %w", def.ID, err)
	}
	return schema, nil
}
func decodeSettings(raw string) settings.Values {
	values := settings.Values{}
	_ = json.Unmarshal([]byte(raw), &values)
	return values
}
func (m *Manager) settingsJSON(id string) string {
	if m != nil {
		m.settingsMu.RLock()
		raw, ok := m.configuration[id]
		m.settingsMu.RUnlock()
		if ok {
			return raw
		}
	}
	schema, _ := m.Schema(id)
	raw, _ := json.Marshal(schema.Defaults())
	return string(raw)
}

// RefreshSettings serializes reads and publication, preventing an older in-flight
// database read from overwriting a configuration just saved by the dashboard.
func (m *Manager) RefreshSettings(ctx context.Context, read func(context.Context) (Configuration, error)) error {
	if m == nil || read == nil {
		return nil
	}
	m.settingsMu.Lock()
	defer m.settingsMu.Unlock()
	configuration, err := read(ctx)
	if err != nil {
		return err
	}
	next := map[string]string{}
	failures := map[string]error{}
	for _, entry := range m.List(nil) {
		values, err := validateSettings(entry.ID, entry.Settings, configuration[entry.ID])
		if err != nil {
			failures[entry.ID] = err
			values = entry.Settings.Defaults()
		}
		raw, err := json.Marshal(values)
		if err != nil {
			return err
		}
		next[entry.ID] = string(raw)
	}
	m.configuration = next
	m.configurationErrors = failures
	return nil
}

func (m *Manager) configurationError(id string) error {
	if m == nil {
		return nil
	}
	m.settingsMu.RLock()
	defer m.settingsMu.RUnlock()
	return m.configurationErrors[id]
}
