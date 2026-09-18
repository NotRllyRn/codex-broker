package broker

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/store"
)

func TestAccountDetailReadsLoginMethodFromAccount(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "windowkeeper.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	_, err = database.DB.ExecContext(ctx, `
		INSERT INTO accounts(account_id,public_token,display_name,last_successful_login_method,enabled,lifecycle_state,created_at_ms,updated_at_ms)
		VALUES('account','public','Account','CHATGPT_BROWSER',1,'ACTIVE',1,1);
		INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms)
		VALUES('account','VERIFIED','STOPPED','HEALTHY','FRESH',1,1);
		INSERT INTO usage_current(account_id,short_resets_at_s,weekly_resets_at_s)
		VALUES('account',1789714203,1790185173);
	`)
	if err != nil {
		t.Fatal(err)
	}

	detail, err := (&Service{Store: database}).AccountDetail(ctx, "public")
	if err != nil {
		t.Fatal(err)
	}
	account := detail["account"].(map[string]any)
	if account["last_successful_login_method"] != "CHATGPT_BROWSER" {
		t.Fatalf("last login method = %#v", account["last_successful_login_method"])
	}
}
