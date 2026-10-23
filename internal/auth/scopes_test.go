package auth

import (
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/store"
)

func TestClientKeyScopes(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.DB.Exec(`INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms) VALUES('a','a','A',1,'ACTIVE',1,1),('b','b','B',1,'ACTIVE',1,1),('c','c','C',1,'ACTIVE',1,1),('d','d','D',1,'ACTIVE',1,1);
 INSERT INTO labels VALUES('work','Work',1),('team','Team',1),('blocked','Blocked',1);
 INSERT INTO account_labels VALUES('a','work'),('b','team'),('c','work'),('c','blocked');`)
	if err != nil {
		t.Fatal(err)
	}
	keys := NewClientKeys(db)
	key, err := keys.Create(ctx, "existing")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		scopes []LabelScope
		want   []string
	}{
		{"unrestricted", nil, []string{"a", "b", "c", "d"}},
		{"allow", []LabelScope{{ID: "work", Mode: "allow"}}, []string{"a", "c"}},
		{"allow any", []LabelScope{{ID: "work", Mode: "allow"}, {ID: "team", Mode: "allow"}}, []string{"a", "b", "c"}},
		{"block wins", []LabelScope{{ID: "work", Mode: "allow"}, {ID: "blocked", Mode: "block"}}, []string{"a"}},
		{"block only includes unlabeled", []LabelScope{{ID: "work", Mode: "block"}}, []string{"b", "d"}},
		{"clear restores all", nil, []string{"a", "b", "c", "d"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := keys.SetScopes(ctx, key.ID, tt.scopes); err != nil {
				t.Fatal(err)
			}
			got, err := keys.AllowedAccountIDs(ctx, key.ID)
			slices.Sort(got)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v want %v: %v", got, tt.want, err)
			}
		})
	}
	scoped, err := keys.Create(ctx, "scoped", LabelScope{ID: "team", Mode: "allow"})
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []LabelScope{{ID: "missing", Mode: "allow"}, {ID: "team", Mode: "invalid"}} {
		if err := keys.SetScopes(ctx, scoped.ID, []LabelScope{invalid}); err == nil {
			t.Fatal("invalid scope accepted")
		}
		if _, err := keys.Create(ctx, "invalid", invalid); err == nil {
			t.Fatal("invalid creation accepted")
		}
		got, err := keys.AllowedAccountIDs(ctx, scoped.ID)
		if err != nil || !reflect.DeepEqual(got, []string{"b"}) {
			t.Fatalf("failed update changed access: %v %v", got, err)
		}
	}
	listed, err := keys.List(ctx)
	if err != nil || len(listed) != 2 {
		t.Fatalf("keys=%+v err=%v", listed, err)
	}
	for _, item := range listed {
		if item.ID == scoped.ID && (len(item.Scopes) != 1 || item.Scopes[0].Name != "Team") {
			t.Fatalf("scopes=%+v", item.Scopes)
		}
	}
	if _, err := keys.Revoke(ctx, scoped.ID); err != nil {
		t.Fatal(err)
	}
	if err := keys.SetScopes(ctx, scoped.ID, nil); err == nil {
		t.Fatal("edited revoked key")
	}
	if _, err := keys.DeleteRevoked(ctx, scoped.ID); err != nil {
		t.Fatal(err)
	}
	var rules int
	if err := db.DB.QueryRow("SELECT count(*) FROM client_key_labels").Scan(&rules); err != nil || rules != 0 {
		t.Fatalf("orphan rules=%d: %v", rules, err)
	}
}
