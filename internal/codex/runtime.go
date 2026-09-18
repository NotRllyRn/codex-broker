package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

type Runtime struct {
	AccountID, GenerationID, Root string
	Client                        *Client
	Adapter                       Adapter
	Mu                            sync.Mutex
}

func (r *Runtime) CodexHome() string { return filepath.Join(r.Root, "codex-home") }

type RuntimeManager struct {
	config      config.Config
	vault       *vault.Vault
	mu          sync.Mutex
	runtimes    map[string]*Runtime
	quarantined map[string]*Runtime
	starting    map[string]bool
	starts      chan struct{}
}

func NewRuntimeManager(c config.Config, v *vault.Vault) *RuntimeManager {
	return &RuntimeManager{config: c, vault: v, runtimes: map[string]*Runtime{}, quarantined: map[string]*Runtime{}, starting: map[string]bool{}, starts: make(chan struct{}, c.ProcessStartConcurrency)}
}

func (m *RuntimeManager) Start(ctx context.Context, accountID string, payload *vault.Payload, workspace *string) (*Runtime, error) {
	m.mu.Lock()
	if m.runtimes[accountID] != nil || m.quarantined[accountID] != nil || m.starting[accountID] {
		m.mu.Unlock()
		return nil, fmt.Errorf("runtime already exists for account %s", accountID)
	}
	m.starting[accountID] = true
	defer func() {
		m.mu.Lock()
		delete(m.starting, accountID)
		m.mu.Unlock()
	}()
	generation := core.NewID()
	root := filepath.Join(m.config.RuntimeDir, "accounts", accountID, generation)
	for _, child := range []string{"home", "codex-home", "tmp", "workspace"} {
		if err := os.MkdirAll(filepath.Join(root, child), 0o700); err != nil {
			m.mu.Unlock()
			_ = os.RemoveAll(root)
			return nil, err
		}
		_ = os.Chmod(filepath.Join(root, child), 0o700)
	}
	if payload != nil {
		if m.vault == nil {
			m.mu.Unlock()
			_ = os.RemoveAll(root)
			return nil, errorsNew("vault is unavailable")
		}
		if err := m.vault.Materialize(*payload, filepath.Join(root, "codex-home")); err != nil {
			m.mu.Unlock()
			_ = os.RemoveAll(root)
			return nil, err
		}
	}
	configText := "cli_auth_credentials_store = \"file\"\nweb_search = \"disabled\"\n"
	if workspace != nil {
		configText += "forced_chatgpt_workspace_id = " + strconv.Quote(*workspace) + "\n"
	}
	if err := os.WriteFile(filepath.Join(root, "codex-home", "config.toml"), []byte(configText), 0o600); err != nil {
		m.mu.Unlock()
		_ = os.RemoveAll(root)
		return nil, err
	}
	m.mu.Unlock()
	select {
	case m.starts <- struct{}{}:
	case <-ctx.Done():
		_ = os.RemoveAll(root)
		return nil, ctx.Err()
	}
	environment := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + filepath.Join(root, "home"), "CODEX_HOME=" + filepath.Join(root, "codex-home"), "TMPDIR=" + filepath.Join(root, "tmp"), "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TZ=UTC", "NO_COLOR=1"}
	client, err := Spawn(ctx, m.config.CodexExecutable, filepath.Join(root, "workspace"), environment)
	<-m.starts
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	runtime := &Runtime{AccountID: accountID, GenerationID: generation, Root: root, Client: client}
	runtime.Adapter = Adapter{Client: client}
	m.mu.Lock()
	m.runtimes[accountID] = runtime
	m.mu.Unlock()
	return runtime, nil
}

func (m *RuntimeManager) Get(accountID string) *Runtime {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtimes[accountID]
}

func (m *RuntimeManager) Discard(runtime *Runtime) error {
	m.mu.Lock()
	if m.runtimes[runtime.AccountID] == runtime {
		delete(m.runtimes, runtime.AccountID)
	}
	m.mu.Unlock()
	return os.RemoveAll(runtime.Root)
}

func (m *RuntimeManager) Archive(runtime *Runtime) {
	m.mu.Lock()
	if m.runtimes[runtime.AccountID] == runtime {
		delete(m.runtimes, runtime.AccountID)
	}
	m.mu.Unlock()
}

func (m *RuntimeManager) Preserve(runtime *Runtime) {
	_ = runtime.Client.Close()
	m.mu.Lock()
	delete(m.runtimes, runtime.AccountID)
	m.quarantined[runtime.AccountID] = runtime
	m.mu.Unlock()
}

func (m *RuntimeManager) Stop(accountID string) error {
	runtime := m.Get(accountID)
	if runtime == nil {
		return nil
	}
	runtime.Mu.Lock()
	defer runtime.Mu.Unlock()
	if err := runtime.Client.Close(); err != nil {
		return err
	}
	return m.Discard(runtime)
}

func (m *RuntimeManager) Close() {
	m.mu.Lock()
	var active, quarantined []*Runtime
	for _, runtime := range m.runtimes {
		active = append(active, runtime)
	}
	for _, runtime := range m.quarantined {
		quarantined = append(quarantined, runtime)
	}
	m.runtimes = map[string]*Runtime{}
	m.mu.Unlock()
	for _, runtime := range active {
		_ = runtime.Client.Close()
		_ = os.RemoveAll(runtime.Root)
	}
	for _, runtime := range quarantined {
		_ = runtime.Client.Close()
	}
}

func errorsNew(value string) error { return fmt.Errorf("%s", value) }
