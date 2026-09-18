package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/core"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Store struct {
	DB   *sql.DB
	Path string
}

func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{DB: db, Path: path}
	if err := s.configure(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) configure(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=FULL",
		"PRAGMA trusted_schema=OFF",
	} {
		if _, err := s.DB.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	current := 0
	if err := s.DB.QueryRowContext(ctx, "SELECT COALESCE(max(version),0) FROM schema_migrations").Scan(&current); err != nil && !strings.Contains(err.Error(), "no such table") {
		return err
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if err != nil || version <= current {
			continue
		}
		script, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		backup := strings.TrimSuffix(s.Path, filepath.Ext(s.Path)) + fmt.Sprintf(".pre-v%d.db", version)
		if info, statErr := os.Stat(s.Path); statErr == nil && info.Size() > 0 {
			if _, err := s.DB.ExecContext(ctx, "PRAGMA wal_checkpoint(FULL)"); err != nil {
				return err
			}
			if err := copyFile(s.Path, backup); err != nil {
				return err
			}
		}
		if _, err := s.DB.ExecContext(ctx, string(script)); err != nil {
			s.DB.Close()
			if _, statErr := os.Stat(backup); statErr == nil {
				_ = os.Remove(s.Path + "-wal")
				_ = os.Remove(s.Path + "-shm")
				if restoreErr := copyFile(backup, s.Path); restoreErr != nil {
					return fmt.Errorf("migration %d failed: %w; restore failed: %v", version, err, restoreErr)
				}
			}
			return fmt.Errorf("migration %d failed: %w", version, err)
		}
		checksum := sha256.Sum256(script)
		name := strings.TrimSuffix(entry.Name(), ".sql")
		if _, err := s.DB.ExecContext(ctx, "INSERT INTO schema_migrations VALUES(?,?,?,?)", version, name, hex.EncodeToString(checksum[:]), time.Now().UnixMilli()); err != nil {
			return err
		}
		current = version
	}
	if _, err := s.DB.ExecContext(ctx, "INSERT OR IGNORE INTO instance_metadata VALUES(1,?,?,?)", core.NewID(), time.Now().UnixMilli(), "0.1.0"); err != nil {
		return err
	}
	var check string
	if err := s.DB.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		if err != nil {
			return err
		}
		return fmt.Errorf("database integrity check failed: %s", check)
	}
	return nil
}

func (s *Store) Write(ctx context.Context, work func(Executor) error) (err error) {
	connection, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	if _, err = connection.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_, _ = connection.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	if err = work(connection); err != nil {
		return err
	}
	_, err = connection.ExecContext(ctx, "COMMIT")
	return err
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) InstanceID(ctx context.Context) (string, error) {
	var value string
	err := s.DB.QueryRowContext(ctx, "SELECT instance_uuid FROM instance_metadata WHERE singleton_id=1").Scan(&value)
	return value, err
}

func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var value int
	err := s.DB.QueryRowContext(ctx, "SELECT COALESCE(max(version),0) FROM schema_migrations").Scan(&value)
	return value, err
}

func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(output, input)
	if err == nil {
		err = output.Sync()
	}
	if closeErr := output.Close(); err == nil {
		err = closeErr
	}
	return err
}
