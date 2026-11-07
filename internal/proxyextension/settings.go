package proxyextension

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/store"
)

// Disabled is additive snapshot data, so older relay caches default to enabled.
func Disabled(ctx context.Context, db *store.Store) (map[string]bool, error) {
	var raw string
	err := db.DB.QueryRowContext(ctx, "SELECT value_json FROM settings WHERE setting_key='proxy_disabled_extensions'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	if err = json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, err
	}
	result := map[string]bool{}
	for _, id := range ids {
		result[id] = true
	}
	return result, nil
}
func SetEnabled(ctx context.Context, db *store.Store, id string, enabled bool) error {
	// SQL performs the read/modify/write atomically, including concurrent toggles.
	_, err := db.DB.ExecContext(ctx, `INSERT INTO settings(setting_key,value_json,updated_at_ms)
 VALUES('proxy_disabled_extensions', CASE WHEN ? THEN '[]' ELSE json_array(?) END, ?)
 ON CONFLICT(setting_key) DO UPDATE SET value_json=(
 SELECT json_group_array(value) FROM (
 SELECT value FROM json_each(settings.value_json) WHERE value != ?
 UNION SELECT ? WHERE NOT ?)),updated_at_ms=excluded.updated_at_ms`, enabled, id, time.Now().UnixMilli(), id, id, enabled)
	return err
}
