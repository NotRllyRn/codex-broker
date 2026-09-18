package broker

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/codex"
	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/events"
	"github.com/NotRllyRn/codex-broker/internal/logbook"
	"github.com/NotRllyRn/codex-broker/internal/platform"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/vault"
	"github.com/NotRllyRn/codex-broker/internal/webhook"
)

type Application struct {
	Config                 config.Config
	Store                  *store.Store
	Vault                  *vault.Vault
	Admin                  *auth.Admin
	ClientKeys             *auth.ClientKeys
	Service                *Service
	Router                 Router
	Events                 *events.Bus
	Webhooks               *webhook.Dispatcher
	Logs                   *logbook.Book
	Compatibility          codex.Compatibility
	VaultConfigured, Ready bool
	lock                   *platform.Lock
}

func OpenApplication(ctx context.Context, settings config.Config) (*Application, error) {
	if err := os.MkdirAll(settings.DataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(settings.RuntimeDir, 0o700); err != nil {
		return nil, err
	}
	lock, err := platform.AcquireLock(filepath.Join(settings.DataDir, "windowkeeper.lock"))
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Application, error) { _ = lock.Close(); return nil, err }
	database, err := store.Open(ctx, filepath.Join(settings.DataDir, "windowkeeper.db"))
	if err != nil {
		return fail(err)
	}
	instance, err := database.InstanceID(ctx)
	if err != nil {
		database.Close()
		return fail(err)
	}
	encoded, err := platform.ReadProtected(settings.VaultKeyFile, settings.VaultKey)
	if err != nil {
		database.Close()
		return fail(err)
	}
	configured := encoded != ""
	var key []byte
	if configured {
		key, err = vault.DecodeKey(encoded)
		if err != nil {
			database.Close()
			return fail(err)
		}
	} else {
		key = make([]byte, 32)
		_, _ = rand.Read(key)
	}
	secrets, err := vault.New(key, instance)
	if err != nil {
		database.Close()
		return fail(err)
	}
	if configured {
		if err = verifySentinel(ctx, database, secrets, instance); err != nil {
			database.Close()
			return fail(err)
		}
	}
	compatibility := codex.Inspect(settings.CodexExecutable)
	if compatibility.ObservedVersion != "" {
		settings.CodexVersion = compatibility.ObservedVersion
	}
	admin := auth.NewAdmin(database, settings.SessionIdleMinutes, settings.SessionAbsoluteHours)
	password, err := platform.ReadProtected(settings.AdminPasswordFile, settings.AdminPassword)
	if err != nil {
		database.Close()
		return fail(err)
	}
	if password != "" {
		if err = admin.Bootstrap(ctx, password); err != nil {
			database.Close()
			return fail(err)
		}
	}
	adminConfigured, err := admin.Configured(ctx)
	if err != nil {
		database.Close()
		return fail(err)
	}
	book, err := logbook.Open(settings.LogDir)
	if err != nil {
		database.Close()
		return fail(err)
	}
	bus := events.New(2000, 256)
	runtime := codex.NewRuntimeManager(settings, secrets)
	dispatcher := webhook.New(database, secrets)
	service := NewService(database, settings, secrets, runtime, bus)
	service.Webhooks = dispatcher
	application := &Application{Config: settings, Store: database, Vault: secrets, Admin: admin, ClientKeys: auth.NewClientKeys(database), Service: service, Events: bus, Webhooks: dispatcher, Logs: book, Compatibility: compatibility, VaultConfigured: configured, Ready: configured && adminConfigured && compatibility.Compatible, lock: lock}
	application.Router = Router{database, service, settings.ResetPaddingSeconds}
	if err = service.Reconcile(ctx); err != nil {
		application.Close()
		return nil, err
	}
	if application.Ready {
		service.StartBackground(ctx)
		dispatcher.Start()
	}
	return application, nil
}

func verifySentinel(ctx context.Context, database *store.Store, secrets *vault.Vault, instance string) error {
	var sealed []byte
	err := database.DB.QueryRowContext(ctx, "SELECT sentinel_ciphertext FROM vault_state WHERE singleton_id=1").Scan(&sealed)
	expected := "windowkeeper:" + instance
	if err == nil {
		value, openErr := secrets.OpenText(sealed)
		if openErr != nil || value != expected {
			return errors.New("configured vault key does not match this instance")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	sealed, err = secrets.SealText("vault-sentinel", expected)
	if err != nil {
		return err
	}
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	_, err = database.DB.ExecContext(ctx, "INSERT INTO vault_state VALUES(1,?,?,?,?)", secrets.KeyID, nonce, sealed, core.NowMS())
	return err
}

func (a *Application) Close() {
	if a == nil {
		return
	}
	a.Ready = false
	if a.Service != nil {
		a.Service.Close()
	}
	if a.Webhooks != nil {
		a.Webhooks.Close()
	}
	if a.Store != nil {
		_ = a.Store.Close()
	}
	if a.Logs != nil {
		_ = a.Logs.Close()
	}
	if a.lock != nil {
		_ = a.lock.Close()
	}
}
