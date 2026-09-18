package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

type ClientKeys struct{ store *store.Store }

type IssuedClientKey struct {
	ID, Name, Prefix, Token string
	CreatedAtMS             int64
}

type ClientKey struct {
	ID, Name, Prefix string
	CreatedAtMS      int64
	LastUsedAtMS     sql.NullInt64
	RevokedAtMS      sql.NullInt64
}

func NewClientKeys(database *store.Store) *ClientKeys { return &ClientKeys{database} }

func KeyDigest(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}

func (c *ClientKeys) Create(ctx context.Context, displayName string) (IssuedClientKey, error) {
	name := strings.Join(strings.Fields(displayName), " ")
	if utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 80 {
		return IssuedClientKey{}, errors.New("client key name must contain 1-80 characters")
	}
	issued := IssuedClientKey{ID: core.NewID(), Name: name, Token: "cbk_" + core.RandomToken(32), CreatedAtMS: time.Now().UnixMilli()}
	issued.Prefix = issued.Token[:12]
	err := c.store.Write(ctx, func(connection store.Executor) error {
		_, err := connection.ExecContext(ctx, "INSERT INTO client_api_keys VALUES(?,?,?,?,?,NULL,NULL)", issued.ID, issued.Name, issued.Prefix, KeyDigest(issued.Token), issued.CreatedAtMS)
		return err
	})
	return issued, err
}

func (c *ClientKeys) Authenticate(ctx context.Context, token string) (*ClientKey, error) {
	now := time.Now().UnixMilli()
	var authenticated *ClientKey
	err := c.store.Write(ctx, func(connection store.Executor) error {
		rows, err := connection.QueryContext(ctx, "SELECT key_id,name,key_prefix,secret_hash,created_at_ms,last_used_at_ms,revoked_at_ms FROM client_api_keys WHERE revoked_at_ms IS NULL")
		if err != nil {
			return err
		}
		defer rows.Close()
		supplied := KeyDigest(token)
		for rows.Next() {
			var key ClientKey
			var digest []byte
			if err := rows.Scan(&key.ID, &key.Name, &key.Prefix, &digest, &key.CreatedAtMS, &key.LastUsedAtMS, &key.RevokedAtMS); err != nil {
				return err
			}
			if subtle.ConstantTimeCompare(digest, supplied) == 1 {
				authenticated = &key
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if authenticated != nil {
			_, err = connection.ExecContext(ctx, "UPDATE client_api_keys SET last_used_at_ms=? WHERE key_id=?", now, authenticated.ID)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if authenticated == nil {
		return nil, core.NewError("CLIENT_KEY_INVALID", "Client authentication failed", 401)
	}
	authenticated.LastUsedAtMS = sql.NullInt64{Int64: now, Valid: true}
	return authenticated, nil
}

func (c *ClientKeys) Revoke(ctx context.Context, id string) (bool, error) {
	var changed bool
	err := c.store.Write(ctx, func(connection store.Executor) error {
		result, err := connection.ExecContext(ctx, "UPDATE client_api_keys SET revoked_at_ms=? WHERE key_id=? AND revoked_at_ms IS NULL", time.Now().UnixMilli(), id)
		if err == nil {
			count, _ := result.RowsAffected()
			changed = count == 1
		}
		return err
	})
	return changed, err
}

func (c *ClientKeys) DeleteRevoked(ctx context.Context, id string) (bool, error) {
	var changed bool
	err := c.store.Write(ctx, func(connection store.Executor) error {
		result, err := connection.ExecContext(ctx, "DELETE FROM client_api_keys WHERE key_id=? AND revoked_at_ms IS NOT NULL", id)
		if err == nil {
			count, _ := result.RowsAffected()
			changed = count == 1
		}
		return err
	})
	return changed, err
}

func (c *ClientKeys) List(ctx context.Context) ([]ClientKey, error) {
	rows, err := c.store.DB.QueryContext(ctx, "SELECT key_id,name,key_prefix,created_at_ms,last_used_at_ms,revoked_at_ms FROM client_api_keys ORDER BY created_at_ms,key_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []ClientKey
	for rows.Next() {
		var key ClientKey
		if err := rows.Scan(&key.ID, &key.Name, &key.Prefix, &key.CreatedAtMS, &key.LastUsedAtMS, &key.RevokedAtMS); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}
