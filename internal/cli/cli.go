package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/broker"
	"github.com/NotRllyRn/codex-broker/internal/codex"
	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/NotRllyRn/codex-broker/internal/httpserver"
	"github.com/NotRllyRn/codex-broker/internal/platform"
	"github.com/NotRllyRn/codex-broker/internal/publicsite"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/vault"
	"golang.org/x/term"
)

const Version = "0.1.0"

func Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usage()
	}
	if args[0] == "-c" {
		return composeHealthProbe(args)
	}
	switch args[0] {
	case "version", "--version":
		return version(args[1:])
	case "help", "-h", "--help":
		return usage()
	}
	if args[0] == "vault" && len(args) > 1 && args[1] == "generate-key" {
		return generateKey(args[2:])
	}
	settings, err := config.Load()
	if err != nil {
		return err
	}
	switch args[0] {
	case "serve":
		return serve(ctx, settings, args[1:])
	case "public-serve":
		return publicServe(ctx, settings, args[1:])
	case "init":
		return initialize(ctx, settings, args[1:])
	case "password-set":
		return passwordSet(ctx, settings, args[1:])
	case "health":
		return health(settings, args[1:])
	case "status":
		return status(ctx, settings, args[1:])
	case "client-key":
		return clientKey(ctx, settings, args[1:])
	case "doctor":
		return doctor(settings)
	case "backup":
		return backup(ctx, settings, args[1:])
	case "restore":
		return restore(ctx, settings, args[1:])
	case "vault":
		return vaultCommand(ctx, settings, args[1:])
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func composeHealthProbe(args []string) error {
	port := "8787"
	certificatePath := os.Getenv("WINDOWKEEPER_TLS_CERT_FILE")
	if len(args) > 1 && strings.Contains(args[1], "8788") {
		port = "8788"
		certificatePath = os.Getenv("WINDOWKEEPER_PUBLIC_ENROLLMENT_TLS_CERT_FILE")
	}
	address := net.JoinHostPort("127.0.0.1", port)
	if certificatePath != "" {
		certificatePEM, err := os.ReadFile(certificatePath)
		if err != nil {
			return err
		}
		block, _ := pem.Decode(certificatePEM)
		if block == nil {
			return errors.New("health certificate is invalid")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return err
		}
		serverName := ""
		if len(certificate.DNSNames) > 0 {
			serverName = certificate.DNSNames[0]
		} else if len(certificate.IPAddresses) > 0 {
			serverName = certificate.IPAddresses[0].String()
		}
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM(certificatePEM)
		connection, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", address, &tls.Config{RootCAs: roots, ServerName: serverName, MinVersion: tls.VersionTLS12})
		if err != nil {
			return err
		}
		return connection.Close()
	}
	connection, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		return err
	}
	return connection.Close()
}
func usage() error {
	fmt.Println("Codex Broker\n\nCommands: serve, public-serve, init, password-set, version, health, status, client-key, doctor, backup, restore, vault")
	return nil
}
func flags(name string) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	return set
}

func serve(ctx context.Context, settings config.Config, args []string) error {
	set := flags("serve")
	host := set.String("host", settings.Host, "bind host")
	port := set.Int("port", settings.Port, "bind port")
	if err := set.Parse(args); err != nil {
		return err
	}
	if !config.IsLoopback(*host) && (settings.TLSCertFile == "" || settings.TLSKeyFile == "") {
		return errors.New("non-loopback binds require TLS certificate and key files")
	}
	application, err := broker.OpenApplication(ctx, settings)
	if err != nil {
		return err
	}
	defer application.Close()
	handler, err := httpserver.New(application)
	if err != nil {
		return err
	}
	return runServer(ctx, &http.Server{Addr: net.JoinHostPort(*host, strconv.Itoa(*port)), Handler: handler, ReadHeaderTimeout: 10 * time.Second}, settings.TLSCertFile, settings.TLSKeyFile)
}
func publicServe(ctx context.Context, settings config.Config, args []string) error {
	set := flags("public-serve")
	host := set.String("host", settings.PublicEnrollmentHost, "bind host")
	port := set.Int("port", settings.PublicEnrollmentPort, "bind port")
	if err := set.Parse(args); err != nil {
		return err
	}
	if settings.PublicEnrollmentKey == "" {
		return errors.New("public enrollment internal key is not configured")
	}
	if settings.PublicEnrollmentCACert == "" {
		return errors.New("the broker CA certificate is required")
	}
	if !config.IsLoopback(*host) && (settings.PublicEnrollmentTLSCertFile == "" || settings.PublicEnrollmentTLSKeyFile == "") {
		return errors.New("non-loopback public binds require TLS certificate and key files")
	}
	handler, err := publicsite.New(settings)
	if err != nil {
		return err
	}
	return runServer(ctx, &http.Server{Addr: net.JoinHostPort(*host, strconv.Itoa(*port)), Handler: handler, ReadHeaderTimeout: 10 * time.Second}, settings.PublicEnrollmentTLSCertFile, settings.PublicEnrollmentTLSKeyFile)
}
func runServer(ctx context.Context, server *http.Server, cert, key string) error {
	result := make(chan error, 1)
	go func() {
		if cert != "" && key != "" {
			result <- server.ListenAndServeTLS(cert, key)
		} else {
			result <- server.ListenAndServe()
		}
	}()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	}
}

func openDatabase(ctx context.Context, settings config.Config) (*platform.Lock, *store.Store, error) {
	lock, err := platform.AcquireLock(filepath.Join(settings.DataDir, "windowkeeper.lock"))
	if err != nil {
		return nil, nil, err
	}
	database, err := store.Open(ctx, filepath.Join(settings.DataDir, "windowkeeper.db"))
	if err != nil {
		_ = lock.Close()
		return nil, nil, err
	}
	return lock, database, nil
}
func closeDatabase(lock *platform.Lock, database *store.Store) {
	_ = database.Close()
	_ = lock.Close()
}

func initialize(ctx context.Context, settings config.Config, args []string) error {
	set := flags("init")
	keyFile := set.String("key-file", "windowkeeper-vault.key", "vault key file")
	password := set.String("password", "", "administrator password")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *password == "" {
		value, err := promptPassword()
		if err != nil {
			return err
		}
		*password = value
	}
	encoded := vault.GenerateKey()
	if err := platform.WriteExclusive(*keyFile, []byte(encoded+"\n"), 0o600); err != nil {
		return fmt.Errorf("key file already exists: %s", *keyFile)
	}
	success := false
	defer func() {
		if !success {
			_ = os.Remove(*keyFile)
		}
	}()
	lock, database, err := openDatabase(ctx, settings)
	if err != nil {
		return err
	}
	defer closeDatabase(lock, database)
	instance, _ := database.InstanceID(ctx)
	root, _ := vault.DecodeKey(encoded)
	secrets, _ := vault.New(root, instance)
	sealed, err := secrets.SealText("vault-sentinel", "windowkeeper:"+instance)
	if err != nil {
		return err
	}
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	if _, err = database.DB.ExecContext(ctx, "INSERT INTO vault_state VALUES(1,?,?,?,?) ON CONFLICT(singleton_id) DO NOTHING", secrets.KeyID, nonce, sealed, time.Now().UnixMilli()); err != nil {
		return err
	}
	if err = auth.NewAdmin(database, settings.SessionIdleMinutes, settings.SessionAbsoluteHours).SetPassword(ctx, *password); err != nil {
		return err
	}
	absolute, _ := filepath.Abs(*keyFile)
	fmt.Println("Codex Broker initialized.")
	fmt.Printf("Set WINDOWKEEPER_VAULT_KEY_FILE=%s\n", absolute)
	success = true
	return nil
}
func promptPassword() (string, error) {
	reader := bufio.NewReader(os.Stdin)
	first, err := readPassword(reader, "Password: ")
	if err != nil {
		return "", errors.New("password is required")
	}
	confirmation, err := readPassword(reader, "Repeat for confirmation: ")
	if err != nil {
		return "", errors.New("password confirmation is required")
	}
	if first != confirmation {
		return "", errors.New("passwords do not match")
	}
	return first, nil
}

func readPassword(reader *bufio.Reader, prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		value, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(value), err
	}
	value, err := reader.ReadString('\n')
	if errors.Is(err, io.EOF) && value != "" {
		err = nil
	}
	return strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "\r"), err
}
func passwordSet(ctx context.Context, settings config.Config, args []string) error {
	set := flags("password-set")
	password := set.String("password", "", "administrator password")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *password == "" {
		value, err := promptPassword()
		if err != nil {
			return err
		}
		*password = value
	}
	lock, database, err := openDatabase(ctx, settings)
	if err != nil {
		return err
	}
	defer closeDatabase(lock, database)
	if err = auth.NewAdmin(database, settings.SessionIdleMinutes, settings.SessionAbsoluteHours).SetPassword(ctx, *password); err != nil {
		return err
	}
	fmt.Println("Administrator password updated; existing sessions were revoked.")
	return nil
}
func version(args []string) error {
	asJSON := len(args) > 0 && args[0] == "--json"
	if asJSON {
		return printJSON(map[string]any{"api_version": "codex-broker.dev/cli/v1", "kind": "Version", "data": map[string]any{"version": Version}})
	}
	fmt.Println(Version)
	return nil
}

func health(settings config.Config, args []string) error {
	asJSON := has(args, "--json")
	host := settings.Host
	if host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	scheme := "http"
	if settings.TLSCertFile != "" {
		scheme = "https"
	}
	requestURL := scheme + "://" + net.JoinHostPort(host, strconv.Itoa(settings.Port)) + "/health/ready"
	transport := &http.Transport{Proxy: nil}
	if settings.TLSCertFile != "" {
		certificate, readErr := os.ReadFile(settings.TLSCertFile)
		if readErr != nil {
			return readErr
		}
		roots, rootErr := x509.SystemCertPool()
		if rootErr != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(certificate) {
			return errors.New("TLS certificate could not be loaded for health check")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	client := http.Client{Timeout: 3 * time.Second, Transport: transport}
	response, err := client.Get(requestURL)
	if err != nil {
		if asJSON {
			_ = printJSON(map[string]any{"api_version": "codex-broker.dev/cli/v1", "kind": "Health", "data": map[string]any{"status": "unavailable", "error": fmt.Sprintf("%T", err)}})
		}
		return errors.New("Codex Broker service is unavailable")
	}
	defer response.Body.Close()
	var data map[string]any
	if json.NewDecoder(response.Body).Decode(&data) != nil {
		return errors.New("Codex Broker service is unavailable")
	}
	if asJSON {
		return printJSON(map[string]any{"api_version": "codex-broker.dev/cli/v1", "kind": "Health", "data": data})
	}
	fmt.Printf("Service status: %v\n", data["status"])
	if response.StatusCode != 200 {
		return errors.New("service is not ready")
	}
	return nil
}
func status(ctx context.Context, settings config.Config, args []string) error {
	lock, database, err := openDatabase(ctx, settings)
	if err != nil {
		return err
	}
	defer closeDatabase(lock, database)
	rows, err := database.DB.QueryContext(ctx, "SELECT a.public_token,a.display_name,a.enabled,s.overall_state,s.auth_state,s.usage_state FROM accounts a JOIN account_state s USING(account_id) WHERE a.deleted_at_ms IS NULL ORDER BY lower(a.display_name)")
	if err != nil {
		return err
	}
	defer rows.Close()
	accounts := []map[string]any{}
	for rows.Next() {
		var token, name, overall, authentication, usage string
		var enabled int
		if err := rows.Scan(&token, &name, &enabled, &overall, &authentication, &usage); err != nil {
			return err
		}
		accounts = append(accounts, map[string]any{"public_token": token, "display_name": name, "enabled": enabled == 1, "overall_state": overall, "auth_state": authentication, "usage_state": usage})
	}
	var incidents int
	_ = database.DB.QueryRowContext(ctx, "SELECT count(*) FROM incidents WHERE state='OPEN'").Scan(&incidents)
	value := map[string]any{"version": Version, "accounts": accounts, "open_incidents": incidents}
	if has(args, "--json") || has(args, "--json-output") {
		return printJSONIndent(value)
	}
	fmt.Printf("Codex Broker %s | %d accounts | %d open incidents\n", Version, len(accounts), incidents)
	for _, account := range accounts {
		fmt.Printf("  %-24s %-16s %s\n", account["display_name"], account["overall_state"], account["usage_state"])
	}
	return nil
}

func clientKey(ctx context.Context, settings config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("client-key subcommand is required")
	}
	lock, database, err := openDatabase(ctx, settings)
	if err != nil {
		return err
	}
	defer closeDatabase(lock, database)
	keys := auth.NewClientKeys(database)
	switch args[0] {
	case "create":
		if len(args) < 2 {
			return errors.New("client key name is required")
		}
		issued, err := keys.Create(ctx, strings.Join(args[1:], " "))
		if err != nil {
			return err
		}
		fmt.Println(issued.Token)
		return nil
	case "list":
		values, err := keys.List(ctx)
		if err != nil {
			return err
		}
		for _, key := range values {
			state := "active"
			if key.RevokedAtMS.Valid {
				state = "revoked"
			}
			fmt.Printf("%s  %s  %s  %s\n", key.ID, key.Prefix, state, key.Name)
		}
		return nil
	case "revoke":
		if len(args) != 2 {
			return errors.New("key id is required")
		}
		changed, err := keys.Revoke(ctx, args[1])
		if err != nil {
			return err
		}
		if !changed {
			return errors.New("active client key not found")
		}
		fmt.Println("Client key revoked.")
		return nil
	}
	return errors.New("unknown client-key subcommand")
}
func doctor(settings config.Config) error {
	failed := false
	data, _ := filepath.Abs(settings.DataDir)
	runtime, _ := filepath.Abs(settings.RuntimeDir)
	ok := data != runtime
	fmt.Printf("%s  directory separation: persistent and runtime roots differ\n", pass(ok))
	failed = !ok
	compatibility := codex.Inspect(settings.CodexExecutable)
	fmt.Printf("%s  managed codex: %s\n", pass(compatibility.Compatible), compatibility.Detail)
	failed = failed || !compatibility.Compatible
	if failed {
		return errors.New("doctor checks failed")
	}
	return nil
}
func pass(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}
func has(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
func printJSON(value any) error { return json.NewEncoder(os.Stdout).Encode(value) }
func printJSONIndent(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
