package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

func TestVaultRotationPreservesManagedCredential(t *testing.T) {
	ctx, root := context.Background(), t.TempDir()
	database, err := store.Open(ctx, filepath.Join(root, "windowkeeper.db"))
	if err != nil {
		t.Fatal(err)
	}
	instance, err := database.InstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldEncoded := vault.GenerateKey()
	oldKey, _ := vault.DecodeKey(oldEncoded)
	oldVault, _ := vault.New(oldKey, instance)
	home := t.TempDir()
	original := []byte(`{"tokens":{"access_token":"access","refresh_token":"refresh","account_id":"account"}}`)
	if err = os.WriteFile(filepath.Join(home, "auth.json"), original, 0600); err != nil {
		t.Fatal(err)
	}
	payload, err := oldVault.Capture(home, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := oldVault.Encrypt("account", payload)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.DB.Exec(`INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms) VALUES('account','public','Account',1,'ACTIVE',1,1)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.DB.Exec("INSERT INTO credential_bundles VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", envelope.BundleID, envelope.AccountID, "ACTIVE", envelope.EnvelopeVersion, envelope.PayloadSchemaVersion, envelope.KeyID, envelope.Nonce, envelope.Ciphertext, envelope.AAD, "test", 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	oldPath, newPath := filepath.Join(root, "old.key"), filepath.Join(root, "new.key")
	if err = os.WriteFile(oldPath, []byte(oldEncoded), 0600); err != nil {
		t.Fatal(err)
	}
	if err = rotateKey(ctx, config.Config{DataDir: root}, []string{"--old-key-file", oldPath, "--new-key-file", newPath}); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	newKey, err := vault.DecodeKey(string(encoded[:len(encoded)-1]))
	if err != nil {
		t.Fatal(err)
	}
	newVault, _ := vault.New(newKey, instance)
	database, err = store.Open(ctx, filepath.Join(root, "windowkeeper.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var active vault.Envelope
	err = database.DB.QueryRow("SELECT bundle_id,account_id,key_id,nonce,ciphertext,aad,payload_schema_version,envelope_version FROM credential_bundles WHERE state='ACTIVE'").Scan(&active.BundleID, &active.AccountID, &active.KeyID, &active.Nonce, &active.Ciphertext, &active.AAD, &active.PayloadSchemaVersion, &active.EnvelopeVersion)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := newVault.Decrypt(active)
	if err != nil {
		t.Fatal(err)
	}
	content, err := newVault.AuthJSON(rotated)
	if err != nil || string(content) != string(original) {
		t.Fatalf("rotation changed credential: %v", err)
	}
	var retired int
	_ = database.DB.QueryRow("SELECT count(*) FROM credential_bundles WHERE state='RETIRED'").Scan(&retired)
	if retired != 1 {
		t.Fatalf("retired generations=%d", retired)
	}
	var sentinel []byte
	if err = database.DB.QueryRow("SELECT sentinel_ciphertext FROM vault_state").Scan(&sentinel); err != nil {
		t.Fatal(err)
	}
	value, err := newVault.OpenText(sentinel)
	if err != nil || value != "windowkeeper:"+instance {
		t.Fatalf("rotated sentinel invalid: %v", err)
	}
}
