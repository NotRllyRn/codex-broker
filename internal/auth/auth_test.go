package auth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/store"
)

func TestAdminAndClientKeyLifecycle(t *testing.T) {
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "windowkeeper.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	admin := NewAdmin(database, 60, 24)
	if err := admin.SetPassword(context.Background(), "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	session, err := admin.Login(context.Background(), "correct horse battery staple", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.RequireCSRF(context.Background(), session.Token, session.CSRF); err != nil {
		t.Fatal(err)
	}
	keys := NewClientKeys(database)
	issued, err := keys.Create(context.Background(), "test client")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Authenticate(context.Background(), issued.Token); err != nil {
		t.Fatal(err)
	}
	if changed, err := keys.Revoke(context.Background(), issued.ID); err != nil || !changed {
		t.Fatalf("revoke = %v, %v", changed, err)
	}
}

func TestVerifiesPythonArgonHash(t *testing.T) {
	encoded := "$argon2id$v=19$m=65536,t=3,p=1$ABarJqY+iAxaBhAfKYX0+Q$63bDexf8wTIxK0Jc3RPKwl+O8vVHxY2Vx6suksJgV+Q"
	if !verifyPassword(encoded, "correct horse battery staple") {
		t.Fatal("Python-generated Argon2id hash was rejected")
	}
}
