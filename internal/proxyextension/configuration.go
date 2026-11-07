package proxyextension

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/NotRllyRn/codex-broker/extensions/requestsize"
	"github.com/NotRllyRn/codex-broker/extensions/settings"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

type Configuration map[string]settings.Values
type record struct {
	Values   settings.Values   `json:"values"`
	Secrets  map[string][]byte `json:"secrets,omitempty"`
	Revision string            `json:"revision"`
}
type SettingsView struct {
	Schema    settings.Schema `json:"schema"`
	Values    settings.Values `json:"values"`
	SecretSet map[string]bool `json:"secretSet"`
	Revision  string          `json:"revision"`
}

const configurationPrefix = "proxy_extension_settings:"

func (m *Manager) Schema(id string) (settings.Schema, bool) {
	for _, entry := range m.List(nil) {
		if entry.ID == id {
			return entry.Settings, true
		}
	}
	return settings.Schema{}, false
}
func validateSettings(id string, schema settings.Schema, values settings.Values) (settings.Values, error) {
	values, err := schema.Validate(values)
	if err == nil && id == requestsize.ID {
		err = requestsize.ValidateSettings(values)
	}
	return values, err
}
func decodeRecord(raw string, secrets *vault.Vault) (record, error) {
	result := record{Values: settings.Values{}}
	if raw == "" {
		return result, nil
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return result, err
	}
	if result.Values == nil {
		result.Values = settings.Values{}
	}
	for key, encrypted := range result.Secrets {
		if secrets == nil {
			return result, errors.New("extension secret vault unavailable")
		}
		value, err := secrets.OpenText(encrypted)
		if err != nil {
			return result, err
		}
		result.Values[key] = value
	}
	return result, nil
}
func readRecord(ctx context.Context, db store.Executor, secrets *vault.Vault, id string) (record, error) {
	var raw string
	err := db.QueryRowContext(ctx, "SELECT value_json FROM settings WHERE setting_key=?", configurationPrefix+id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return record{}, err
	}
	return decodeRecord(raw, secrets)
}

// Configurations returns decrypted settings only to the trusted host/runtime.
func Configurations(ctx context.Context, db *store.Store, secrets *vault.Vault) (Configuration, error) {
	rows, err := db.DB.QueryContext(ctx, "SELECT setting_key,value_json FROM settings WHERE setting_key GLOB 'proxy_extension_settings:*'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := Configuration{}
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		entry, err := decodeRecord(raw, secrets)
		if err != nil {
			return nil, err
		}
		result[key[len(configurationPrefix):]] = entry.Values
	}
	return result, rows.Err()
}
func (m *Manager) SettingsView(ctx context.Context, db *store.Store, secrets *vault.Vault, id string) (SettingsView, error) {
	schema, ok := m.Schema(id)
	if !ok {
		return SettingsView{}, core.NewError("EXTENSION_NOT_FOUND", "Proxy extension not found", 404)
	}
	entry, err := readRecord(ctx, db.DB, secrets, id)
	if err != nil {
		return SettingsView{}, err
	}
	values, err := validateSettings(id, schema, entry.Values)
	if err != nil {
		return SettingsView{}, err
	}
	view := SettingsView{Schema: schema, Values: values, Revision: entry.Revision, SecretSet: map[string]bool{}}
	// The public schema and values must never echo password defaults or values.
	view.Schema.Fields = append([]settings.Field(nil), schema.Fields...)
	for i, f := range view.Schema.Fields {
		if f.Type == "password" {
			text, _ := values[f.Key].(string)
			view.SecretSet[f.Key] = text != ""
			delete(view.Values, f.Key)
			view.Schema.Fields[i].Default = settings.JSON("")
		}
	}
	return view, nil
}
func (m *Manager) SaveSettings(ctx context.Context, db *store.Store, secrets *vault.Vault, id, revision string, input settings.Values, reset bool) error {
	schema, ok := m.Schema(id)
	if !ok {
		return core.NewError("EXTENSION_NOT_FOUND", "Proxy extension not found", 404)
	}
	return db.Write(ctx, func(tx store.Executor) error {
		current, err := readRecord(ctx, tx, secrets, id)
		if err != nil {
			return err
		}
		if revision != current.Revision {
			return core.NewError("SETTINGS_CONFLICT", "Settings changed elsewhere. Reopen settings before applying again.", 409)
		}
		if input == nil {
			input = settings.Values{}
		}
		if reset {
			defaults := schema.Defaults()
			for key, value := range input {
				defaults[key] = value
			}
			input = defaults
		} else {
			for _, f := range schema.Fields {
				if f.Type == "password" {
					if _, set := input[f.Key]; !set {
						if value, exists := current.Values[f.Key]; exists {
							input[f.Key] = value
						}
					}
				}
			}
		}
		values, err := validateSettings(id, schema, input)
		if err != nil {
			return core.NewError("INVALID_EXTENSION_SETTINGS", err.Error(), 400)
		}
		next := record{Values: values, Revision: core.NewID(), Secrets: map[string][]byte{}}
		for _, f := range schema.Fields {
			if f.Type == "password" {
				text := values[f.Key].(string)
				if text != "" {
					if secrets == nil {
						return core.NewError("VAULT_KEY_REQUIRED", "Configure the vault before saving password settings", 503)
					}
					sealed, err := secrets.SealText("extension:"+id+":"+f.Key, text)
					if err != nil {
						return err
					}
					next.Secrets[f.Key] = sealed
				}
				if text != "" {
					delete(next.Values, f.Key)
				}
			}
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if len(raw) > settings.MaxBytes*4 {
			return fmt.Errorf("encrypted settings exceed limit")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO settings(setting_key,value_json,updated_at_ms) VALUES(?,?,?) ON CONFLICT(setting_key) DO UPDATE SET value_json=excluded.value_json,updated_at_ms=excluded.updated_at_ms`, configurationPrefix+id, string(raw), time.Now().UnixMilli())
		return err
	})
}
