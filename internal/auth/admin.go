package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/store"
)

type Admin struct {
	store         *store.Store
	idleMinutes   int
	absoluteHours int
}

type Session struct {
	Token             string
	CSRF              string
	CreatedAtMS       int64
	IdleExpiresAtMS   int64
	AbsoluteExpiresMS int64
}

type SessionRow struct {
	CSRFHash          []byte
	CreatedAtMS       int64
	LastSeenAtMS      int64
	IdleExpiresAtMS   int64
	AbsoluteExpiresMS int64
}

func NewAdmin(database *store.Store, idleMinutes, absoluteHours int) *Admin {
	return &Admin{store: database, idleMinutes: idleMinutes, absoluteHours: absoluteHours}
}

func Digest(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}

func (a *Admin) Configured(ctx context.Context) (bool, error) {
	var value int
	err := a.store.DB.QueryRowContext(ctx, "SELECT 1 FROM admin_credentials WHERE singleton_id=1 AND bootstrap_complete=1").Scan(&value)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (a *Admin) Bootstrap(ctx context.Context, password string) error {
	if err := validatePassword(password); err != nil {
		return err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	return a.store.Write(ctx, func(connection store.Executor) error {
		_, err := connection.ExecContext(ctx, "INSERT INTO admin_credentials VALUES(1,?,?,1) ON CONFLICT(singleton_id) DO NOTHING", hash, time.Now().UnixMilli())
		return err
	})
}

func (a *Admin) SetPassword(ctx context.Context, password string) error {
	if err := validatePassword(password); err != nil {
		return err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	return a.store.Write(ctx, func(connection store.Executor) error {
		if _, err := connection.ExecContext(ctx, "INSERT INTO admin_credentials VALUES(1,?,?,1) ON CONFLICT(singleton_id) DO UPDATE SET password_hash=excluded.password_hash,password_changed_at_ms=excluded.password_changed_at_ms,bootstrap_complete=1", hash, now); err != nil {
			return err
		}
		_, err := connection.ExecContext(ctx, "UPDATE admin_sessions SET revoked_at_ms=? WHERE revoked_at_ms IS NULL", now)
		return err
	})
}

func (a *Admin) VerifyPassword(ctx context.Context, password string) (bool, error) {
	var encoded string
	err := a.store.DB.QueryRowContext(ctx, "SELECT password_hash FROM admin_credentials WHERE singleton_id=1").Scan(&encoded)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return verifyPassword(encoded, password), nil
}

func (a *Admin) Login(ctx context.Context, password, fingerprint string) (Session, error) {
	valid, err := a.VerifyPassword(ctx, password)
	if err != nil {
		return Session{}, err
	}
	if !valid {
		return Session{}, core.NewError("LOGIN_FAILED", "The password was not accepted", 401)
	}
	now := time.Now().UnixMilli()
	session := Session{
		Token: core.RandomToken(32), CSRF: core.RandomToken(32), CreatedAtMS: now,
		IdleExpiresAtMS:   now + int64(a.idleMinutes)*60_000,
		AbsoluteExpiresMS: now + int64(a.absoluteHours)*3_600_000,
	}
	err = a.store.Write(ctx, func(connection store.Executor) error {
		var fingerprintHash any
		if fingerprint != "" {
			fingerprintHash = Digest(fingerprint)
		}
		_, err := connection.ExecContext(ctx, "INSERT INTO admin_sessions VALUES(?,?,?,?,?,?,?,?,?)", Digest(session.Token), Digest(session.CSRF), now, now, session.IdleExpiresAtMS, session.AbsoluteExpiresMS, now, nil, fingerprintHash)
		return err
	})
	return session, err
}

func (a *Admin) Session(ctx context.Context, token string) (*SessionRow, error) {
	if token == "" {
		return nil, nil
	}
	now := time.Now().UnixMilli()
	var row *SessionRow
	err := a.store.Write(ctx, func(connection store.Executor) error {
		var current SessionRow
		err := connection.QueryRowContext(ctx, "SELECT csrf_token_hash,created_at_ms,last_seen_at_ms,idle_expires_at_ms,absolute_expires_at_ms FROM admin_sessions WHERE session_id_hash=? AND revoked_at_ms IS NULL AND idle_expires_at_ms>? AND absolute_expires_at_ms>?", Digest(token), now, now).Scan(&current.CSRFHash, &current.CreatedAtMS, &current.LastSeenAtMS, &current.IdleExpiresAtMS, &current.AbsoluteExpiresMS)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if now-current.LastSeenAtMS > 60_000 {
			idle := min(now+int64(a.idleMinutes)*60_000, current.AbsoluteExpiresMS)
			if _, err := connection.ExecContext(ctx, "UPDATE admin_sessions SET last_seen_at_ms=?,idle_expires_at_ms=? WHERE session_id_hash=?", now, idle, Digest(token)); err != nil {
				return err
			}
			current.LastSeenAtMS, current.IdleExpiresAtMS = now, idle
		}
		row = &current
		return nil
	})
	return row, err
}

func (a *Admin) RequireCSRF(ctx context.Context, token, supplied string) error {
	row, err := a.Session(ctx, token)
	if err != nil {
		return err
	}
	if supplied == "" || row == nil || subtle.ConstantTimeCompare(row.CSRFHash, Digest(supplied)) != 1 {
		return core.NewError("CSRF_INVALID", "The request could not be verified", 403)
	}
	return nil
}

func (a *Admin) Logout(ctx context.Context, token string) error {
	return a.store.Write(ctx, func(connection store.Executor) error {
		_, err := connection.ExecContext(ctx, "UPDATE admin_sessions SET revoked_at_ms=? WHERE session_id_hash=?", time.Now().UnixMilli(), Digest(token))
		return err
	})
}
