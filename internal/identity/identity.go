// Package identity owns usage identities, upload credentials and a numerical
// event ledger. Account access and inference credentials remain independent.
package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

type Identity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Device struct {
	ID           string        `json:"id"`
	IdentityID   string        `json:"identity_id"`
	Name         string        `json:"name"`
	LastSync     sql.NullInt64 `json:"-"`
	LastSyncText string        `json:"-"`
	Revoked      sql.NullInt64 `json:"-"`
}

// InputTokens includes both cache reads and writes. Reasoning is included in output.
type Event struct {
	ID                string `json:"id"`
	Source            string `json:"source"`
	TimestampMS       int64  `json:"timestamp_ms"`
	Model             string `json:"model"`
	ServiceTier       string `json:"service_tier"`
	InputTokens       int64  `json:"input_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
	CacheWriteTokens  int64  `json:"cache_write_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	ReasoningTokens   int64  `json:"reasoning_tokens"`
	UsageKnown        bool   `json:"usage_known"`
}

func invalid(detail string) error { return core.NewError("INVALID_USAGE", detail, 422) }

func Name(value string) (string, error) {
	value = strings.Join(strings.Fields(value), " ")
	if n := utf8.RuneCountInString(value); n < 1 || n > 80 {
		return "", invalid("Names must contain 1–80 characters")
	}
	return value, nil
}

func Create(ctx context.Context, db *store.Store, name string) (Identity, string, error) {
	name, err := Name(name)
	if err != nil {
		return Identity{}, "", err
	}
	i := Identity{ID: core.NewID(), Name: name}
	code := "cbi_" + core.RandomToken(32)
	err = db.Write(ctx, func(tx store.Executor) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM usage_identities WHERE name=?)", name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return core.Conflict("IDENTITY_EXISTS", "An identity with this name already exists")
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO usage_identities VALUES(?,?,?,?)", i.ID, i.Name, auth.KeyDigest(code), core.NowMS())
		return err
	})
	return i, code, err
}

func List(ctx context.Context, db *sql.DB) ([]Identity, error) {
	rows, err := db.QueryContext(ctx, "SELECT identity_id,name FROM usage_identities ORDER BY lower(name)")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Identity{}
	for rows.Next() {
		var i Identity
		if err := rows.Scan(&i.ID, &i.Name); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	return items, rows.Err()
}

func Devices(ctx context.Context, db *sql.DB) ([]Device, error) {
	rows, err := db.QueryContext(ctx, "SELECT device_id,identity_id,name,last_sync_at_ms,revoked_at_ms FROM usage_devices ORDER BY created_at_ms")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.IdentityID, &d.Name, &d.LastSync, &d.Revoked); err != nil {
			return nil, err
		}
		if d.LastSync.Valid {
			d.LastSyncText = time.UnixMilli(d.LastSync.Int64).UTC().Format(time.RFC3339)
		}
		items = append(items, d)
	}
	return items, rows.Err()
}

func Rotate(ctx context.Context, db *store.Store, id string) (string, error) {
	code := "cbi_" + core.RandomToken(32)
	r, err := db.DB.ExecContext(ctx, "UPDATE usage_identities SET code_hash=? WHERE identity_id=?", auth.KeyDigest(code), id)
	if err != nil {
		return "", err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return "", core.NewError("IDENTITY_NOT_FOUND", "Identity not found", 404)
	}
	return code, nil
}

// The client generates and saves its credential before enrollment. Retrying an
// enrollment with the same installation and credential is safe after a lost reply.
func Enroll(ctx context.Context, db *store.Store, code, installation, name, token string) (Device, error) {
	name, err := Name(name)
	if err != nil {
		return Device{}, err
	}
	if len(installation) != 32 || len(token) != 47 || !strings.HasPrefix(token, "cbu_") {
		return Device{}, invalid("Invalid device credential or installation ID")
	}
	if _, err := hex.DecodeString(installation); err != nil {
		return Device{}, invalid("Invalid installation ID")
	}
	var d Device
	err = db.Write(ctx, func(tx store.Executor) error {
		var owner string
		if err := tx.QueryRowContext(ctx, "SELECT identity_id FROM usage_identities WHERE code_hash=?", auth.KeyDigest(code)).Scan(&owner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return core.NewError("IDENTITY_CODE_INVALID", "Identity enrollment failed", 401)
			}
			return err
		}
		digest := auth.KeyDigest(token)
		_, err := tx.ExecContext(ctx, `INSERT INTO usage_devices(device_id,identity_id,installation_id,name,token_hash,created_at_ms)
			VALUES(?,?,?,?,?,?) ON CONFLICT(identity_id,installation_id) DO NOTHING`, core.NewID(), owner, installation, name, digest, core.NowMS())
		if err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `SELECT device_id,identity_id,name FROM usage_devices
			WHERE identity_id=? AND installation_id=? AND token_hash=? AND revoked_at_ms IS NULL`, owner, installation, digest).Scan(&d.ID, &d.IdentityID, &d.Name)
		if errors.Is(err, sql.ErrNoRows) {
			return core.Conflict("DEVICE_ENROLLMENT_CONFLICT", "This installation is already enrolled or revoked; use a new installation")
		}
		return err
	})
	return d, err
}

func Authenticate(ctx context.Context, db store.Executor, token string) (Device, error) {
	var d Device
	if !strings.HasPrefix(token, "cbu_") {
		return d, core.NewError("USAGE_DEVICE_INVALID", "Device authentication failed", 401)
	}
	err := db.QueryRowContext(ctx, "SELECT device_id,identity_id,name FROM usage_devices WHERE token_hash=? AND revoked_at_ms IS NULL", auth.KeyDigest(token)).Scan(&d.ID, &d.IdentityID, &d.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return d, core.NewError("USAGE_DEVICE_INVALID", "Device authentication failed", 401)
	}
	return d, err
}

func (e Event) Validate(now time.Time) error {
	if len(e.ID) != 64 {
		return invalid("Event IDs must be SHA-256 hex digests")
	}
	if _, err := hex.DecodeString(e.ID); err != nil {
		return invalid("Invalid event ID")
	}
	if e.Source != "codex" && e.Source != "pi" {
		return invalid("Unsupported usage source")
	}
	if e.TimestampMS < 946684800000 || e.TimestampMS > now.Add(24*time.Hour).UnixMilli() {
		return invalid("Invalid usage timestamp")
	}
	if len(e.Model) < 1 || len(e.Model) > 160 || len(e.ServiceTier) > 32 {
		return invalid("Invalid model or service tier")
	}
	for _, v := range []int64{e.InputTokens, e.CachedInputTokens, e.CacheWriteTokens, e.OutputTokens, e.ReasoningTokens} {
		if v < 0 || v > 1_000_000_000_000 {
			return invalid("Invalid token count")
		}
	}
	if e.CachedInputTokens > e.InputTokens || e.CacheWriteTokens > e.InputTokens-e.CachedInputTokens || e.ReasoningTokens > e.OutputTokens {
		return invalid("Token subsets exceed their totals")
	}
	if !e.UsageKnown {
		return invalid("Uploaded events must include token usage")
	}
	return nil
}

func Insert(ctx context.Context, tx store.Executor, owner, device string, e Event) (bool, error) {
	var deviceID any
	if device != "" {
		deviceID = device
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO identity_usage VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(identity_id,event_id) DO NOTHING`,
		owner, e.ID, deviceID, e.Source, e.TimestampMS, e.Model, e.ServiceTier, e.InputTokens, e.CachedInputTokens, e.CacheWriteTokens, e.OutputTokens, e.ReasoningTokens, e.UsageKnown)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	if n == 1 {
		return true, nil
	}
	var same bool
	err = tx.QueryRowContext(ctx, `SELECT source=? AND model=? AND service_tier=? AND input_tokens=? AND cached_input_tokens=? AND cache_write_tokens=? AND output_tokens=? AND reasoning_tokens=? AND usage_known=?
		FROM identity_usage WHERE identity_id=? AND event_id=?`, e.Source, e.Model, e.ServiceTier, e.InputTokens, e.CachedInputTokens, e.CacheWriteTokens, e.OutputTokens, e.ReasoningTokens, e.UsageKnown, owner, e.ID).Scan(&same)
	if err == nil && !same {
		return false, core.Conflict("USAGE_EVENT_CONFLICT", "An existing event has different usage; the batch was not saved")
	}
	if err == nil {
		// Copied histories may be re-stamped. Keep the earliest observed time,
		// without counting the provider event a second time.
		_, err = tx.ExecContext(ctx, "UPDATE identity_usage SET timestamp_ms=min(timestamp_ms,?) WHERE identity_id=? AND event_id=?", e.TimestampMS, owner, e.ID)
	}
	return false, err
}

func Ingest(ctx context.Context, db *store.Store, token string, events []Event) (int, error) {
	if len(events) > 500 {
		return 0, invalid("Upload at most 500 events per batch")
	}
	for _, e := range events {
		if err := e.Validate(time.Now()); err != nil {
			return 0, err
		}
	}
	inserted := 0
	err := db.Write(ctx, func(tx store.Executor) error {
		d, err := Authenticate(ctx, tx, token)
		if err != nil {
			return err
		}
		for _, e := range events {
			added, err := Insert(ctx, tx, d.IdentityID, d.ID, e)
			if err != nil {
				return err
			}
			if added {
				inserted++
			}
		}
		_, err = tx.ExecContext(ctx, "UPDATE usage_devices SET last_sync_at_ms=? WHERE device_id=?", core.NowMS(), d.ID)
		return err
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

func EventID(value string) string { h := sha256.Sum256([]byte(value)); return hex.EncodeToString(h[:]) }
