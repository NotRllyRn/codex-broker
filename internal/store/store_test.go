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
	"time"

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
		if version != 20 {
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

func TestMigrationNineteenRemovesQuotaMeasurementsAndPreservesTokenTotals(t *testing.T) {
	ctx, path := context.Background(), filepath.Join(t.TempDir(), "broker.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		version, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if err != nil || version > 18 {
			continue
		}
		script, _ := migrationFiles.ReadFile("migrations/" + entry.Name())
		if _, err := db.Exec(string(script)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO schema_migrations VALUES(?,?,?,0)", version, entry.Name(), "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UnixMilli()
	_, err = db.Exec(`
		INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms) VALUES('a','a','A',1,'ACTIVE',1,1);
		INSERT INTO proxy_responses(proxy_request_id,account_id,endpoint,terminal_status,upstream_attempts,upstream_http_status,request_started_at_ms,upstream_started_at_ms,response_headers_at_ms,first_response_event_at_ms,terminal_event_at_ms,total_tokens,stats_model,active_duration_ms,quota_isolated,short_usage_delta) VALUES('response','a','/v1/responses','completed',1,200,1,2,3,4,?,123,'model',100,1,5);
		INSERT INTO proxy_response_attempts(proxy_request_id,ordinal,account_id,model,started_at_ms,ended_at_ms,upstream_http_status,quota_eligible,quota_isolated) VALUES('response',1,'a','model',1,2,200,1,1);
		INSERT INTO proxy_usage_totals(period,model,response_count,active_duration_ms,total_tokens,short_usage_samples,short_usage_percent,updated_at_ms) VALUES('LIFETIME','model',1,100,123,1,5,?);
	`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var responses, tokens, active int64
	if err := migrated.DB.QueryRow("SELECT response_count,total_tokens,active_duration_ms FROM proxy_usage_totals WHERE period='LIFETIME' AND model='model'").Scan(&responses, &tokens, &active); err != nil || responses != 1 || tokens != 123 || active != 100 {
		t.Fatalf("totals=%d/%d/%d err=%v", responses, tokens, active, err)
	}
	var count int
	if err := migrated.DB.QueryRow("SELECT count(*) FROM proxy_responses WHERE total_tokens=123").Scan(&count); err != nil || count != 1 {
		t.Fatalf("responses=%d err=%v", count, err)
	}
	if err := migrated.DB.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='proxy_response_attempts'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("attempt table=%d err=%v", count, err)
	}
	for _, query := range []string{"SELECT quota_isolated FROM proxy_responses", "SELECT short_usage_samples FROM proxy_usage_totals"} {
		if _, err := migrated.DB.Exec(query); err == nil {
			t.Fatalf("obsolete measurement survived: %s", query)
		}
	}
}

func TestMigrationEighteenSeedsLifetimeBeforePruning(t *testing.T) {
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
		if parseErr != nil || version > 17 {
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
	now := time.Now().UnixMilli()
	insert := func(id, responseModel string, terminal int64, tokens int) {
		t.Helper()
		_, err := database.ExecContext(ctx, `INSERT INTO proxy_responses(
			proxy_request_id,account_id,endpoint,request_model,response_model,terminal_status,upstream_attempts,
			upstream_http_status,request_started_at_ms,upstream_started_at_ms,response_headers_at_ms,
			first_response_event_at_ms,terminal_event_at_ms,total_tokens
		) VALUES(?, 'a','/v1/responses','requested',?,'completed',1,200,?,?,?,?,?,?)`,
			id, responseModel, terminal-1000, terminal-800, terminal-700, terminal-600, terminal, tokens)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("old", "gpt-a", now-8*24*time.Hour.Milliseconds(), 10)
	insert("recent", "gpt-a", now-24*time.Hour.Milliseconds(), 20)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var lifetimeCount, lifetimeTokens, rollingCount, rollingTokens, retained int64
	if err := migrated.DB.QueryRowContext(ctx, "SELECT response_count,total_tokens FROM proxy_usage_totals WHERE period='LIFETIME' AND model='gpt-a'").Scan(&lifetimeCount, &lifetimeTokens); err != nil {
		t.Fatal(err)
	}
	if err := migrated.DB.QueryRowContext(ctx, "SELECT response_count,total_tokens FROM proxy_usage_totals WHERE period='ROLLING_7D' AND model='gpt-a'").Scan(&rollingCount, &rollingTokens); err != nil {
		t.Fatal(err)
	}
	if err := migrated.DB.QueryRowContext(ctx, "SELECT count(*) FROM proxy_responses").Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if lifetimeCount != 2 || lifetimeTokens != 30 || rollingCount != 1 || rollingTokens != 20 || retained != 1 {
		t.Fatalf("lifetime=%d/%d rolling=%d/%d retained=%d", lifetimeCount, lifetimeTokens, rollingCount, rollingTokens, retained)
	}
}

func TestMigrationFourteenRepairsZeroUsageCycle(t *testing.T) {
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
		if parseErr != nil || version > 13 {
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
	_, err = database.ExecContext(ctx, `
		INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms)
		VALUES('a','p','Account',1,'ACTIVE',1,1);
		INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms)
		VALUES('a','VERIFIED','STOPPED','HEALTHY','FRESH',1,1);
		INSERT INTO usage_current(account_id,weekly_used_percent_raw) VALUES('a',0);
		INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,last_success_at_ms,next_pulse_at_ms,cycle_state,weekly_started_at_ms)
		VALUES('a',1,2,3,'IN_CYCLE',4);
	`)
	if err != nil {
		t.Fatal(err)
	}
	_ = database.Close()
	migrated, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var state string
	var started, next, routed sql.NullInt64
	if err := migrated.DB.QueryRowContext(ctx, "SELECT p.cycle_state,p.weekly_started_at_ms,p.next_pulse_at_ms,s.last_routed_at_ms FROM window_pulse_state p JOIN account_state s USING(account_id) WHERE p.account_id='a'").Scan(&state, &started, &next, &routed); err != nil {
		t.Fatal(err)
	}
	if state != "HELD" || started.Valid || next.Valid || routed.Valid {
		t.Fatalf("migration repair = %s, %#v, %#v, %#v", state, started, next, routed)
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
