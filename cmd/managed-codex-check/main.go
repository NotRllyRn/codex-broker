// managed-codex-check runs an opt-in authenticated compatibility check. It never
// connects to a broker database or imports existing account credentials.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/codex"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

func main() {
	executable := flag.String("codex", "codex", "candidate Codex executable")
	expected := flag.String("version", "", "required numeric candidate version, e.g. 0.165.0")
	timeout := flag.Duration("timeout", 20*time.Minute, "total time allowed, including device sign-in")
	flag.Parse()
	if *expected == "" || flag.NArg() != 0 || *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "Usage: managed-codex-check --codex /path/to/codex --version VERSION [--timeout 20m]")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, deadline := context.WithTimeout(ctx, *timeout)
	defer deadline()
	if err := run(ctx, *executable, *expected, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

type check struct {
	root, executable, version string
	vault                     *vault.Vault
	out                       io.Writer
}

func run(ctx context.Context, executable, version string, out io.Writer) error {
	path, err := exec.LookPath(executable)
	if err != nil {
		return errors.New("candidate executable not found")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return errors.New("candidate executable path is invalid")
	}
	root, err := os.MkdirTemp("", "managed-codex-check-")
	if err != nil {
		return errors.New("cannot create isolated temporary directory")
	}
	defer os.RemoveAll(root)
	key, err := vault.DecodeKey(vault.GenerateKey())
	if err != nil {
		return errors.New("cannot generate isolated vault key")
	}
	v, err := vault.New(key, "example-upgrade-check")
	if err != nil {
		return errors.New("cannot create isolated vault")
	}
	c := check{root: root, executable: path, version: "codex-cli " + version, vault: v, out: out}
	// Even --version runs without access to the operator's normal Codex home.
	versionHome := filepath.Join(root, "version")
	if err := os.Mkdir(versionHome, 0700); err != nil {
		return errors.New("cannot create version-check home")
	}
	versionCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	cmd := exec.CommandContext(versionCtx, path, "--version")
	cmd.Env = c.environment(versionHome)
	observed, err := cmd.Output()
	cancel()
	if err != nil || strings.TrimSpace(string(observed)) != c.version {
		return errors.New("candidate did not report the exact requested version")
	}
	fmt.Fprintln(out, "PASS: exact candidate version", c.version)

	client, home, err := c.start(ctx, "login", nil)
	if err != nil {
		return err
	}
	defer client.Close()
	interaction, err := (codex.Adapter{Client: client}).StartLogin(ctx, "CHATGPT_DEVICE_CODE")
	if err != nil {
		return errors.New("device-code login could not start")
	}
	if interaction.LoginID == "" || interaction.UserCode == "" || interaction.VerificationURL == "" {
		return errors.New("device-code login response is incomplete")
	}
	fmt.Fprintln(out, "Sign in with the account authorized for this isolated test:")
	fmt.Fprintln(out, "Verification URL:", interaction.VerificationURL)
	fmt.Fprintln(out, "Device code:", interaction.UserCode)
	fmt.Fprintln(out, "WAITING: complete device sign-in; checks continue automatically.")
	if err := waitLogin(ctx, client, interaction.LoginID); err != nil {
		return err
	}
	identity, err := authenticated(ctx, client)
	if err != nil {
		return err
	}
	payload, err := c.capture(client, home)
	if err != nil {
		return err
	}
	before, err := tokenState(c.vault, payload)
	if err != nil {
		return err
	}
	if err := c.save(payload); err != nil {
		return err
	}
	fmt.Fprintln(out, "PASS: fresh device-code login, verified identity, encrypted checkpoint")

	// Restore into a different home so renewal cannot accidentally use the login
	// process's in-memory credential or original auth.json.
	payload, err = c.load()
	if err != nil {
		return err
	}
	client, home, err = c.start(ctx, "renewal", &payload)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := sameIdentity(ctx, client, identity); err != nil {
		return err
	}
	if _, err := (codex.Adapter{Client: client}).Account(ctx, true); err != nil {
		return errors.New("forced token renewal request failed")
	}
	if err := sameIdentity(ctx, client, identity); err != nil {
		return err
	}
	payload, err = c.capture(client, home)
	if err != nil {
		return err
	}
	after, err := tokenState(c.vault, payload)
	if err != nil {
		return err
	}
	// Some CLI versions return an account even when refresh fails. Require
	// observable credential renewal, rather than treating a successful RPC as proof.
	if before.access == after.access && before.refresh == after.refresh {
		return errors.New("renewal is inconclusive: neither token changed; do not mark renewal passed")
	}
	if err := c.save(payload); err != nil {
		return err
	}
	fmt.Fprintf(out, "PASS: forced renewal; access token changed=%t, refresh token changed=%t\n", before.access != after.access, before.refresh != after.refresh)

	payload, err = c.load()
	if err != nil {
		return err
	}
	client, home, err = c.start(ctx, "restored", &payload)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := sameIdentity(ctx, client, identity); err != nil {
		return err
	}
	adapter := codex.Adapter{Client: client}
	limits, err := adapter.RateLimits(ctx)
	if err != nil || quotaAvailable(limits) != nil {
		return errors.New("quota read failed or test account has no available quota")
	}
	fmt.Fprintln(out, "PASS: renewed credential restored into a fresh process; identity and quota reads")
	pulse, err := adapter.PulseWindows(ctx)
	if err != nil || pulse.Status != "completed" || pulse.Response == "" || quotaAvailable(pulse.Usage) != nil {
		return errors.New("window pulse or its quota reread failed")
	}
	fmt.Fprintf(out, "PASS: window pulse completed; model=%s effort=%s duration_ms=%d\n", pulse.Model, pulse.ReasoningEffort, pulse.DurationMS)
	payload, err = c.capture(client, home)
	if err != nil {
		return err
	}
	if err := c.save(payload); err != nil {
		return err
	}
	fmt.Fprintln(out, "PASS: final credential checkpoint; all authenticated checks completed")
	return nil
}

func (c check) environment(home string) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + filepath.Join(home, "codex"), "TMPDIR=" + home, "LANG=C.UTF-8", "TZ=UTC", "NO_COLOR=1"}
}

func (c check) start(ctx context.Context, name string, payload *vault.Payload) (*codex.Client, string, error) {
	home := filepath.Join(c.root, name)
	codexHome := filepath.Join(home, "codex")
	if err := os.MkdirAll(codexHome, 0700); err != nil {
		return nil, "", errors.New("cannot create isolated runtime")
	}
	if payload != nil {
		if err := c.vault.Materialize(*payload, codexHome); err != nil {
			return nil, "", errors.New("cannot restore encrypted credential")
		}
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("cli_auth_credentials_store = \"file\"\nweb_search = \"disabled\"\n"), 0600); err != nil {
		return nil, "", errors.New("cannot write isolated configuration")
	}
	client, err := codex.Spawn(ctx, c.executable, home, c.environment(home))
	if err != nil {
		return nil, "", errors.New("candidate app-server initialization failed")
	}
	return client, home, nil
}

func waitLogin(ctx context.Context, client *codex.Client, loginID string) error {
	for {
		select {
		case <-ctx.Done():
			return errors.New("device sign-in timed out or was cancelled; start a new check for a fresh code")
		case <-client.Done():
			return errors.New("app-server stopped while waiting for device sign-in")
		case message := <-client.Notifications():
			if message["method"] != "account/login/completed" {
				continue
			}
			params, _ := message["params"].(map[string]any)
			if params["loginId"] != loginID || params["success"] != true {
				return errors.New("device sign-in was rejected or returned an unexpected login ID")
			}
			return nil
		}
	}
}

func authenticated(ctx context.Context, client *codex.Client) (string, error) {
	result, err := (codex.Adapter{Client: client}).Account(ctx, false)
	if err != nil {
		return "", errors.New("authenticated account read failed")
	}
	account, _ := result["account"].(map[string]any)
	if account["type"] != "chatgpt" {
		return "", errors.New("candidate did not return an authenticated ChatGPT account")
	}
	// Keep the identity in memory for comparison, never print or persist it.
	identity, _ := json.Marshal(account)
	return string(identity), nil
}

func sameIdentity(ctx context.Context, client *codex.Client, expected string) error {
	identity, err := authenticated(ctx, client)
	if err != nil {
		return err
	}
	if identity != expected {
		return errors.New("account identity changed across credential renewal or restart")
	}
	return nil
}

type tokens struct{ access, refresh [32]byte }

func tokenState(v *vault.Vault, payload vault.Payload) (tokens, error) {
	raw, err := v.AuthJSON(payload)
	if err != nil {
		return tokens{}, errors.New("credential checkpoint has no readable auth.json")
	}
	var auth struct {
		Tokens struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(raw, &auth) != nil || auth.Tokens.Access == "" || auth.Tokens.Refresh == "" {
		return tokens{}, errors.New("sign-in did not produce both access and refresh tokens")
	}
	return tokens{sha256.Sum256([]byte(auth.Tokens.Access)), sha256.Sum256([]byte(auth.Tokens.Refresh))}, nil
}

func (c check) capture(client *codex.Client, home string) (vault.Payload, error) {
	if client.Close() != nil {
		return vault.Payload{}, errors.New("app-server could not be quiesced before checkpointing")
	}
	payload, err := c.vault.Capture(filepath.Join(home, "codex"), c.version, nil)
	if err != nil {
		return vault.Payload{}, errors.New("credential capture failed")
	}
	return payload, nil
}

func (c check) save(payload vault.Payload) error {
	envelope, err := c.vault.Encrypt("example-test-account", payload)
	if err != nil {
		return errors.New("credential encryption failed")
	}
	raw, err := json.Marshal(envelope)
	if err != nil || os.WriteFile(filepath.Join(c.root, "checkpoint.json"), raw, 0600) != nil {
		return errors.New("encrypted checkpoint could not be saved")
	}
	return nil
}

func (c check) load() (vault.Payload, error) {
	raw, err := os.ReadFile(filepath.Join(c.root, "checkpoint.json"))
	if err != nil {
		return vault.Payload{}, errors.New("encrypted checkpoint could not be read")
	}
	var envelope vault.Envelope
	if json.Unmarshal(raw, &envelope) != nil {
		return vault.Payload{}, errors.New("encrypted checkpoint has an invalid format")
	}
	payload, err := c.vault.Decrypt(envelope)
	if err != nil {
		return vault.Payload{}, errors.New("encrypted checkpoint could not be decrypted")
	}
	return payload, nil
}

func quotaAvailable(result map[string]any) error {
	limits, _ := result["rateLimits"].(map[string]any)
	for _, key := range []string{"primary", "secondary"} {
		window, _ := limits[key].(map[string]any)
		used, ok := window["usedPercent"].(float64)
		reset, resetOK := window["resetsAt"].(float64)
		if !ok || used < 0 || used >= 100 || !resetOK || reset <= float64(time.Now().Unix()) {
			return errors.New("quota window missing, invalid, or exhausted")
		}
	}
	return nil
}
