package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "windowkeeper.db")
	for range 2 {
		database, err := Open(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		version, err := database.SchemaVersion(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if version != 13 {
			t.Fatalf("schema version = %d", version)
		}
		var foreignKeys int
		var integrity string
		_ = database.DB.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys)
		_ = database.DB.QueryRow("PRAGMA quick_check").Scan(&integrity)
		if foreignKeys != 1 || integrity != "ok" {
			t.Fatalf("foreign_keys=%d quick_check=%q", foreignKeys, integrity)
		}
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrationThirteenPreservesPulseState(t *testing.T) {
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "windowkeeper.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		version, parseErr := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if parseErr != nil || version > 12 {
			continue
		}
		script, _ := migrationFiles.ReadFile("migrations/" + entry.Name())
		if _, err := database.ExecContext(ctx, string(script)); err != nil {
			t.Fatal(err)
		}
		if _, err := database.ExecContext(ctx, "INSERT INTO schema_migrations VALUES(?,?,?,0)", version, strings.TrimSuffix(entry.Name(), ".sql"), "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.ExecContext(ctx, "INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms) VALUES('a','p','Account',1,'ACTIVE',1,1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, "INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,last_success_at_ms,next_pulse_at_ms) VALUES('a',10,20,30)"); err != nil {
		t.Fatal(err)
	}
	_ = database.Close()
	migrated, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var state string
	var attempt, success, next int64
	if err := migrated.DB.QueryRowContext(ctx, "SELECT cycle_state,last_attempt_at_ms,last_success_at_ms,next_pulse_at_ms FROM window_pulse_state WHERE account_id='a'").Scan(&state, &attempt, &success, &next); err != nil {
		t.Fatal(err)
	}
	if state != "IN_CYCLE" || attempt != 10 || success != 20 || next != 30 {
		t.Fatalf("migrated pulse = %s, %d, %d, %d", state, attempt, success, next)
	}
}

func TestMigrationSixPreservesCredentialGenerations(t *testing.T) {
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "windowkeeper.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_initial.sql", "002_unmanaged_auth_export.sql", "003_single_login_token_fork.sql", "004_manual_token_login.sql", "005_open_login_method.sql"} {
		script, _ := migrationFiles.ReadFile("migrations/" + name)
		if _, err := database.ExecContext(ctx, string(script)); err != nil {
			t.Fatal(err)
		}
		version := int(name[2] - '0')
		if _, err := database.ExecContext(ctx, "INSERT INTO schema_migrations VALUES(?,?,?,0)", version, name[:len(name)-4], "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.ExecContext(ctx, "INSERT INTO accounts VALUES('a','p','Account','chatgpt','CHATGPT_DEVICE_CODE',NULL,NULL,1,'ACTIVE',0,0,NULL)"); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, state, nonce string }{{"active", "ACTIVE", "a"}, {"export", "EXPORT", "e"}, {"retired", "RETIRED", "r"}} {
		if _, err := database.ExecContext(ctx, "INSERT INTO credential_bundles VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", row.id, "a", row.state, 1, 1, "primary", []byte(row.nonce), []byte("cipher"), []byte("aad"), "test", 0, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	_ = database.Close()
	migrated, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var count int
	if err := migrated.DB.QueryRowContext(ctx, "SELECT count(*) FROM credential_bundles WHERE account_id='a'").Scan(&count); err != nil || count != 3 {
		t.Fatalf("credential count=%d, error=%v", count, err)
	}
}
