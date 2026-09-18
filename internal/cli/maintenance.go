package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/platform"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

func backup(ctx context.Context, settings config.Config, args []string) error {
	set := flags("backup")
	output := set.String("output", "", "backup path")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *output == "" {
		return errors.New("--output is required")
	}
	source := filepath.Join(settings.DataDir, "windowkeeper.db")
	if _, err := os.Stat(source); err != nil {
		return errors.New("database does not exist")
	}
	lock, err := platform.AcquireLock(filepath.Join(settings.DataDir, "windowkeeper.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	database, err := sql.Open("sqlite", source)
	if err != nil {
		return err
	}
	_, _ = database.ExecContext(ctx, "PRAGMA wal_checkpoint(FULL)")
	_ = database.Close()
	if err = platform.AtomicCopy(source, *output, 0o600); err != nil {
		return err
	}
	check, err := openReadOnly(*output)
	if err != nil {
		return err
	}
	defer check.Close()
	var integrity string
	if err = check.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		_ = os.Remove(*output)
		return errors.New("backup integrity check failed")
	}
	fmt.Printf("Backup written to %s\n", *output)
	return nil
}

func restore(ctx context.Context, settings config.Config, args []string) error {
	set := flags("restore")
	input := set.String("input", "", "backup path")
	confirm := set.String("confirm", "", "confirmation")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *confirm != "RESTORE" {
		return errors.New("confirmation must be exactly RESTORE")
	}
	if err := platform.RequireProtectedRegular(*input); err != nil {
		return errors.New("backup must be a protected regular file")
	}
	source, err := openReadOnly(*input)
	if err != nil {
		return err
	}
	var integrity string
	var version int
	err = source.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity)
	if err == nil {
		err = source.QueryRowContext(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version)
	}
	source.Close()
	if err != nil || integrity != "ok" {
		return errors.New("backup integrity check failed")
	}
	if version != 11 {
		return errors.New("backup schema is not supported by this release")
	}
	destination := filepath.Join(settings.DataDir, "windowkeeper.db")
	if err = os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	lock, err := platform.AcquireLock(filepath.Join(settings.DataDir, "windowkeeper.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = platform.AtomicCopy(*input, destination, 0o600); err != nil {
		return err
	}
	_ = os.Remove(destination + "-wal")
	_ = os.Remove(destination + "-shm")
	fmt.Println("Backup restored. Verify the vault key before starting Codex Broker.")
	return nil
}
func openReadOnly(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return sql.Open("sqlite", "file:"+absolute+"?mode=ro&immutable=1")
}

func vaultCommand(ctx context.Context, settings config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("vault subcommand is required")
	}
	switch args[0] {
	case "generate-key":
		return generateKey(args[1:])
	case "verify":
		return verifyKey(ctx, settings, args[1:])
	case "rotate":
		return rotateKey(ctx, settings, args[1:])
	}
	return errors.New("unknown vault subcommand")
}
func generateKey(args []string) error {
	set := flags("vault generate-key")
	output := set.String("output", "", "output file")
	if err := set.Parse(args); err != nil {
		return err
	}
	value := vault.GenerateKey()
	if *output == "" {
		fmt.Println(value)
		return nil
	}
	if err := platform.WriteExclusive(*output, []byte(value+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Printf("Vault key written to %s\n", *output)
	return nil
}
func verifyKey(ctx context.Context, settings config.Config, args []string) error {
	set := flags("vault verify")
	path := set.String("key-file", "", "key file")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("--key-file is required")
	}
	encoded, err := platform.ReadProtected(*path, "")
	if err != nil {
		return err
	}
	key, err := vault.DecodeKey(encoded)
	if err != nil {
		return errors.New("key file could not be read")
	}
	lock, database, err := openDatabase(ctx, settings)
	if err != nil {
		return err
	}
	defer closeDatabase(lock, database)
	instance, _ := database.InstanceID(ctx)
	var sentinel []byte
	if err = database.DB.QueryRowContext(ctx, "SELECT sentinel_ciphertext FROM vault_state WHERE singleton_id=1").Scan(&sentinel); err != nil {
		return errors.New("database has no vault sentinel")
	}
	secrets, _ := vault.New(key, instance)
	value, err := secrets.OpenText(sentinel)
	if err != nil || value != "windowkeeper:"+instance {
		return errors.New("vault key verification failed")
	}
	fmt.Println("Vault key verified.")
	return nil
}

type credentialRow struct {
	bundleID, accountID, state, keyID, codexVersion string
	envelopeVersion, payloadVersion                 int
	nonce, ciphertext, aad                          []byte
	created                                         int64
}
type destinationRow struct {
	id          string
	url, secret []byte
}

func rotateKey(ctx context.Context, settings config.Config, args []string) error {
	set := flags("vault rotate")
	oldPath := set.String("old-key-file", "", "old key file")
	newPath := set.String("new-key-file", "", "new key file")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *oldPath == "" || *newPath == "" {
		return errors.New("--old-key-file and --new-key-file are required")
	}
	oldEncoded, err := platform.ReadProtected(*oldPath, "")
	if err != nil {
		return err
	}
	oldKey, err := vault.DecodeKey(oldEncoded)
	if err != nil {
		return err
	}
	newEncoded := vault.GenerateKey()
	if err = platform.WriteExclusive(*newPath, []byte(newEncoded+"\n"), 0o600); err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			_ = os.Remove(*newPath)
		}
	}()
	newKey, _ := vault.DecodeKey(newEncoded)
	lock, database, err := openDatabase(ctx, settings)
	if err != nil {
		return err
	}
	defer closeDatabase(lock, database)
	instance, _ := database.InstanceID(ctx)
	oldVault, _ := vault.New(oldKey, instance, "previous")
	newVault, _ := vault.New(newKey, instance, "rotated")
	count := 0
	err = database.Write(ctx, func(db store.Executor) error {
		rows, err := db.QueryContext(ctx, "SELECT bundle_id,account_id,state,envelope_version,payload_schema_version,key_id,nonce,ciphertext,aad,codex_version,created_at_ms FROM credential_bundles WHERE state IN('ACTIVE','EXPORT')")
		if err != nil {
			return err
		}
		var credentials []credentialRow
		for rows.Next() {
			var row credentialRow
			if err := rows.Scan(&row.bundleID, &row.accountID, &row.state, &row.envelopeVersion, &row.payloadVersion, &row.keyID, &row.nonce, &row.ciphertext, &row.aad, &row.codexVersion, &row.created); err != nil {
				rows.Close()
				return err
			}
			credentials = append(credentials, row)
		}
		rows.Close()
		destinationRows, err := db.QueryContext(ctx, "SELECT destination_id,encrypted_url,encrypted_signing_secret FROM webhook_destinations")
		if err != nil {
			return err
		}
		var destinations []destinationRow
		for destinationRows.Next() {
			var row destinationRow
			var secret sql.Null[[]byte]
			if err := destinationRows.Scan(&row.id, &row.url, &secret); err != nil {
				destinationRows.Close()
				return err
			}
			if secret.Valid {
				row.secret = secret.V
			}
			destinations = append(destinations, row)
		}
		destinationRows.Close()
		now := core.NowMS()
		for _, row := range credentials {
			payload, err := oldVault.Decrypt(vault.Envelope{BundleID: row.bundleID, AccountID: row.accountID, KeyID: row.keyID, Nonce: row.nonce, Ciphertext: row.ciphertext, AAD: row.aad, PayloadSchemaVersion: row.payloadVersion, EnvelopeVersion: row.envelopeVersion})
			if err != nil {
				return err
			}
			envelope, err := newVault.Encrypt(row.accountID, payload)
			if err != nil {
				return err
			}
			if row.state == "ACTIVE" {
				if _, err = db.ExecContext(ctx, "UPDATE credential_bundles SET state='RETIRED',retired_at_ms=? WHERE bundle_id=?", now, row.bundleID); err != nil {
					return err
				}
			} else {
				if _, err = db.ExecContext(ctx, "DELETE FROM credential_bundles WHERE bundle_id=?", row.bundleID); err != nil {
					return err
				}
			}
			created := now
			var promoted any = now
			if row.state == "EXPORT" {
				created = row.created
				promoted = nil
			}
			if _, err = db.ExecContext(ctx, "INSERT INTO credential_bundles VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", envelope.BundleID, envelope.AccountID, row.state, envelope.EnvelopeVersion, envelope.PayloadSchemaVersion, envelope.KeyID, envelope.Nonce, envelope.Ciphertext, envelope.AAD, row.codexVersion, created, promoted, nil); err != nil {
				return err
			}
			count++
		}
		for _, row := range destinations {
			target, err := oldVault.OpenText(row.url)
			if err != nil {
				return err
			}
			sealedURL, err := newVault.SealText("webhook:"+row.id+":url", target)
			if err != nil {
				return err
			}
			var sealedSecret any
			if row.secret != nil {
				secret, err := oldVault.OpenText(row.secret)
				if err != nil {
					return err
				}
				sealedSecret, err = newVault.SealText("webhook:"+row.id+":secret", secret)
				if err != nil {
					return err
				}
			}
			urlHash := sha256.Sum256(sealedURL)
			var secretHash any
			if value, ok := sealedSecret.([]byte); ok {
				digest := sha256.Sum256(value)
				secretHash = digest[:12]
			}
			if _, err = db.ExecContext(ctx, "UPDATE webhook_destinations SET url_nonce=?,encrypted_url=?,secret_nonce=?,encrypted_signing_secret=?,updated_at_ms=? WHERE destination_id=?", urlHash[:12], sealedURL, secretHash, sealedSecret, now, row.id); err != nil {
				return err
			}
			count++
		}
		sentinel, err := newVault.SealText("vault-sentinel", "windowkeeper:"+instance)
		if err != nil {
			return err
		}
		nonce := make([]byte, 12)
		_, _ = rand.Read(nonce)
		_, err = db.ExecContext(ctx, "INSERT INTO vault_state VALUES(1,?,?,?,?) ON CONFLICT(singleton_id) DO UPDATE SET active_key_id=excluded.active_key_id,sentinel_nonce=excluded.sentinel_nonce,sentinel_ciphertext=excluded.sentinel_ciphertext,updated_at_ms=excluded.updated_at_ms", newVault.KeyID, nonce, sentinel, now)
		return err
	})
	if err != nil {
		return err
	}
	success = true
	fmt.Printf("Rotated %d encrypted object(s). Replace the configured key file atomically.\n", count)
	return nil
}
