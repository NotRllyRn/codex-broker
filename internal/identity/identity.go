// Package identity owns usage identities, upload credentials and a numerical
// event ledger. Account access and inference credentials remain independent.
package identity

import (
	"context"
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
