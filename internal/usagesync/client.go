// Package usagesync installs and runs the native ccusage report uploader.
package usagesync

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/usagereport"
	"golang.org/x/term"
)

// ErrSyncRunning lets installation coexist with its startup scheduler trigger.
var ErrSyncRunning = errors.New("another usage sync is already running")

type Config struct {
	URL          string   `json:"url"`
	Name         string   `json:"name"`
	Code         string   `json:"enrollment_code,omitempty"`
	Installation string   `json:"installation_id"`
	Token        string   `json:"token"`
	Device       string   `json:"device_id,omitempty"`
	CA           string   `json:"ca,omitempty"`
	Environment  []string `json:"environment,omitempty"`
}
type Entry struct {
	Revision      int64           `json:"revision"`
	Report        json.RawMessage `json:"report"`
	AllowDecrease bool            `json:"allow_decrease,omitempty"`
	Acked         bool            `json:"acked"`
}
type Journal map[string]Entry

type Client struct {
	Directory string
	Config    Config
	HTTP      *http.Client
	Output    io.Writer
	Collect   func(context.Context) ([]byte, error)
}

func executable(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
func random(n int) []byte {
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		panic(err)
	}
	return data
}

func atomicJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".save-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return replaceFile(name, path)
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}

func transport(config Config) (*http.Client, error) {
	parsed, err := url.Parse(config.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("use a direct HTTPS Broker URL, optionally with its root path")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if config.CA != "" {
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		data, err := os.ReadFile(config.CA)
		if err != nil {
			return nil, err
		}
		if !roots.AppendCertsFromPEM(data) {
			return nil, errors.New("private CA has no certificates")
		}
		tlsConfig.RootCAs = roots
	}
	return &http.Client{Timeout: time.Minute, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: tlsConfig}, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("redirect rejected; configure the direct Broker URL")
	}}, nil
}

func (c *Client) post(ctx context.Context, path string, body any, token string, result any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err = writer.Write(raw); err != nil {
		return err
	}
	if err = writer.Close(); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, "POST", c.Config.URL+path, &compressed)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "gzip")
	// Enrollment uses an ordinary small JSON body for compatibility with Broker.
	if token == "" {
		request.Body = io.NopCloser(bytes.NewReader(raw))
		request.ContentLength = int64(len(raw))
		request.Header.Del("Content-Encoding")
	} else {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != 200 {
		var detail struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(data, &detail)
		return fmt.Errorf("Broker HTTP %d: %s", response.StatusCode, detail.Detail)
	}
	return json.Unmarshal(data, result)
}

func (c *Client) collect(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, filepath.Join(c.Directory, executable("ccusage")), "daily", "--by-agent", "--json", "--timezone", "UTC", "--offline", "--config", filepath.Join(c.Directory, "ccusage.json"))
	command.Env = append(os.Environ(), c.Config.Environment...)
	// Bound output so a broken collector cannot exhaust memory or corrupt the journal.
	output := &limitedBuffer{limit: 64 << 20}
	diagnostic := &limitedBuffer{limit: 16 << 10}
	command.Stdout = output
	command.Stderr = diagnostic
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("ccusage failed: %w (%s)", err, strings.TrimSpace(diagnostic.String()))
	}
	return output.Bytes(), nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if len(data) > b.limit-b.Len() {
		return 0, errors.New("collector output exceeds limit")
	}
	return b.Buffer.Write(data)
}

func (c *Client) Sync(ctx context.Context, allowDecrease bool) error {
	unlock, err := lock(c.Directory)
	if err != nil {
		return err
	}
	defer unlock()
	journal := Journal{}
	path := filepath.Join(c.Directory, "reports.json")
	if err := readJSON(path, &journal); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read usage journal: %w", err)
	}
	if journal == nil {
		return errors.New("invalid empty journal")
	}
	for day, entry := range journal {
		_, row, err := usagereport.Canonical(entry.Report)
		if err != nil || entry.Revision <= 0 || row.Period != day {
			return fmt.Errorf("invalid journal entry for %s; restore the intact journal", day)
		}
	}
	// Always send durable pending reports before taking a new snapshot.
	if err := c.enroll(ctx); err != nil {
		// Even a failed first connection must preserve locally readable history.
		collectErr := c.capture(ctx, journal, allowDecrease)
		return errors.Join(err, collectErr)
	}
	if err := c.upload(ctx, journal); err != nil {
		collectErr := c.capture(ctx, journal, allowDecrease)
		return errors.Join(err, collectErr)
	}
	collectErr := c.capture(ctx, journal, allowDecrease)
	if collectErr != nil {
		var warning *historyWarning
		if !errors.As(collectErr, &warning) {
			return collectErr
		}
	}
	if err := c.upload(ctx, journal); err != nil {
		return errors.Join(err, collectErr)
	}
	if collectErr != nil {
		return collectErr
	}
	fmt.Fprintln(c.Output, "Usage sync complete.")
	return nil
}

func (c *Client) capture(ctx context.Context, journal Journal, allowDecrease bool) error {
	collect := c.Collect
	if collect == nil {
		collect = c.collect
	}
	data, err := collect(ctx)
	if err != nil {
		return err
	}
	reports, err := usagereport.Daily(data)
	if err != nil {
		return err
	}
	var warnings []error
	for _, raw := range reports {
		_, row, err := usagereport.Canonical(raw)
		if err != nil {
			return err
		}
		previous := journal[row.Period]
		if bytes.Equal(previous.Report, raw) {
			continue
		}
		if previous.Revision > 0 {
			_, old, err := usagereport.Canonical(previous.Report)
			if err != nil {
				return err
			}
			if !allowDecrease && usagereport.Decreases(old, row) {
				warnings = append(warnings, fmt.Errorf("%s: tokens decreased; kept previous report (restore logs or use sync --allow-decrease for an intentional correction)", row.Period))
				continue
			}
			// Never supersede an unacknowledged snapshot with a smaller one.
			if !previous.Acked && usagereport.Decreases(old, row) {
				warnings = append(warnings, fmt.Errorf("%s: sync the pending report before correcting it", row.Period))
				continue
			}
		}
		if previous.Revision == int64(^uint64(0)>>1) {
			return errors.New("report revision exhausted")
		}
		journal[row.Period] = Entry{Revision: previous.Revision + 1, Report: raw, AllowDecrease: allowDecrease}
	}
	if err := atomicJSON(filepath.Join(c.Directory, "reports.json"), journal); err != nil {
		return err
	}
	if len(warnings) > 0 {
		return &historyWarning{errors.Join(warnings...)}
	}
	return nil
}

type historyWarning struct{ error }

func (c *Client) enroll(ctx context.Context) error {
	if c.Config.Code == "" {
		return nil
	}
	var response struct {
		ID string `json:"id"`
	}
	body := map[string]string{"code": c.Config.Code, "installation_id": c.Config.Installation, "name": c.Config.Name, "token": c.Config.Token}
	if err := c.post(ctx, "/api/v1/usage/devices", body, "", &response); err != nil {
		return err
	}
	if response.ID == "" {
		return errors.New("Broker did not acknowledge enrollment")
	}
	c.Config.Device = response.ID
	c.Config.Code = ""
	return atomicJSON(filepath.Join(c.Directory, "config.json"), c.Config)
}

func (c *Client) upload(ctx context.Context, journal Journal) error {
	days := []string{}
	for day, entry := range journal {
		if !entry.Acked {
			days = append(days, day)
		}
	}
	sort.Strings(days)
	// Empty uploads are a last-successful-sync heartbeat, not additional usage.
	for first := 0; first < len(days) || first == 0; {
		last := first
		size := 0
		batch := []Entry{}
		for last < len(days) && last-first < 100 {
			entry := journal[days[last]]
			if len(batch) > 0 && size+len(entry.Report) > 1500000 {
				break
			}
			size += len(entry.Report)
			batch = append(batch, entry)
			last++
		}
		body := struct {
			Version        int     `json:"version"`
			CCUsageVersion string  `json:"ccusage_version"`
			Snapshots      []Entry `json:"snapshots"`
		}{2, usagereport.Version, batch}
		var reply struct {
			Accepted int `json:"accepted"`
		}
		if err := c.post(ctx, "/api/v1/usage/batches", body, c.Config.Token, &reply); err != nil {
			return err
		}
		if reply.Accepted != len(batch) {
			return errors.New("Broker did not acknowledge the full batch; retained for retry")
		}
		for _, day := range days[first:last] {
			entry := journal[day]
			entry.Acked = true
			journal[day] = entry
		}
		if err := atomicJSON(filepath.Join(c.Directory, "reports.json"), journal); err != nil {
			return err
		}
		if last == len(days) {
			break
		}
		first = last
	}
	return nil
}

func Run(ctx context.Context, args []string, input *os.File, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("use install, sync, status, uninstall, or version")
	}
	if args[0] == "version" {
		fmt.Fprintln(output, "Codex Broker usage sync; ccusage", usagereport.Version)
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(output)
	directory := flags.String("state-dir", filepath.Join(home, ".codex-broker-usage-native"), "Private per-installation state directory")
	address := flags.String("broker-url", "", "Broker HTTPS URL (include any root path)")
	name := flags.String("name", "", "Device name (defaults to hostname)")
	ca := flags.String("ca", "", "Trusted private CA PEM")
	configPath := flags.String("ccusage-config", "", "Optional ccusage JSON config to copy into this installation")
	noSchedule := flags.Bool("no-schedule", false, "Manage scheduling yourself")
	codeStdin := flags.Bool("code-stdin", false, "Read enrollment code from private stdin")
	allowDecrease := flags.Bool("allow-decrease", false, "Accept an intentional decrease in cumulative report tokens")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	*directory, err = filepath.Abs(*directory)
	if err != nil {
		return err
	}
	if err := privateDir(*directory); err != nil {
		return err
	}
	if args[0] == "uninstall" {
		return schedule(*directory, true)
	}
	if args[0] == "status" {
		journal := Journal{}
		if err := readJSON(filepath.Join(*directory, "reports.json"), &journal); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		pending := 0
		for _, entry := range journal {
			if !entry.Acked {
				pending++
			}
		}
		fmt.Fprintf(output, "ccusage %s; %d daily reports retained; %d pending.\n", usagereport.Version, len(journal), pending)
		return nil
	}
	if args[0] != "install" && args[0] != "sync" {
		return errors.New("unknown command")
	}
	var config Config
	err = readJSON(filepath.Join(*directory, "config.json"), &config)
	if args[0] == "install" {
		unlock, lockErr := lock(*directory)
		if lockErr != nil {
			return lockErr
		}
		err = readJSON(filepath.Join(*directory, "config.json"), &config)
		err = install(ctx, *directory, &config, err, *address, *name, *ca, *configPath, *codeStdin, input, output)
		unlock()
		if err != nil {
			return err
		}
		if !*noSchedule {
			if err := schedule(*directory, false); err != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	client, err := transport(config)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	err = (&Client{Directory: *directory, Config: config, HTTP: client, Output: output}).Sync(ctx, *allowDecrease)
	if args[0] == "install" && errors.Is(err, ErrSyncRunning) {
		fmt.Fprintln(output, "Installed; the background sync is already running.")
		return nil
	}
	if args[0] == "install" && err != nil {
		return fmt.Errorf("installed, but initial sync failed: %w; check the Broker URL and CA, then rerun install --broker-url https://YOUR-BROKER with the same --state-dir to correct the saved connection", err)
	}
	return err
}

func install(ctx context.Context, directory string, config *Config, readErr error, address, name, ca, configPath string, codeStdin bool, input *os.File, output io.Writer) error {
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if readErr != nil {
		if _, err := transport(Config{URL: address}); err != nil {
			return err
		}
		var code []byte
		var err error
		if codeStdin {
			code, err = io.ReadAll(io.LimitReader(input, 256))
		} else {
			fmt.Fprint(output, "Identity code (hidden): ")
			code, err = term.ReadPassword(int(input.Fd()))
			fmt.Fprintln(output)
		}
		if err != nil {
			return fmt.Errorf("read identity code (automation can use --code-stdin): %w", err)
		}
		secret := strings.TrimSpace(string(code))
		if len(secret) != 47 || !strings.HasPrefix(secret, "cbi_") {
			return errors.New("enter the generated identity code, not its name")
		}
		if name == "" {
			name, _ = os.Hostname()
		}
		*config = Config{URL: strings.TrimRight(address, "/"), Name: name, Code: secret, Installation: hex.EncodeToString(random(16)), Token: "cbu_" + base64.RawURLEncoding.EncodeToString(random(32)), Environment: sourceEnvironment()}
		if ca != "" {
			data, err := os.ReadFile(ca)
			if err != nil {
				return err
			}
			config.CA = filepath.Join(directory, "broker-ca.pem")
			if err := os.WriteFile(config.CA, data, 0600); err != nil {
				return err
			}
		}
		if _, err := transport(*config); err != nil {
			return err
		}
		if err := atomicJSON(filepath.Join(directory, "config.json"), config); err != nil {
			return err
		}
	}
	if readErr == nil && (address != "" || ca != "") {
		if err := updateConnection(directory, config, address, ca); err != nil {
			return err
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := ensureCollector(ctx, directory, filepath.Dir(self), Native); err != nil {
		return err
	}
	if err := copyExecutable(self, filepath.Join(directory, executable("codex-broker-usage"))); err != nil {
		return err
	}
	options := filepath.Join(directory, "ccusage.json")
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			return err
		}
		if !json.Valid(data) {
			return errors.New("invalid ccusage config JSON")
		}
		if err := os.WriteFile(options, data, 0600); err != nil {
			return err
		}
	} else if _, err := os.Stat(options); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(options, []byte("{}"), 0600); err != nil {
			return err
		}
	}
	fmt.Fprintf(output, "Installed in %s. All usage sources are included; ccusage %s.\n", directory, usagereport.Version)
	return nil
}

// Explicit connection overrides repair a failed installation without reenrollment.
func updateConnection(directory string, config *Config, address, ca string) error {
	next := *config
	if address != "" {
		next.URL = strings.TrimRight(address, "/")
	}
	if ca != "" {
		next.CA = ca
	}
	client, err := transport(next)
	if err != nil {
		return err
	}
	client.CloseIdleConnections()
	if ca != "" {
		data, err := os.ReadFile(ca)
		if err != nil {
			return err
		}
		// Keep the previous CA usable if saving the new configuration fails.
		next.CA = filepath.Join(directory, fmt.Sprintf("broker-ca-%x.pem", sha256.Sum256(data)))
		if err := os.WriteFile(next.CA, data, 0600); err != nil {
			return err
		}
	}
	if err := atomicJSON(filepath.Join(directory, "config.json"), next); err != nil {
		return err
	}
	*config = next
	return nil
}

// Validate the replacement before touching the installed collector. npm ships only
// the helper; a changed collector pin must replace an existing older collector.
func ensureCollector(ctx context.Context, directory, source string, fetch func(context.Context, string, string) ([]byte, error)) error {
	target := filepath.Join(directory, executable("ccusage"))
	valid := func(path string) bool {
		version, err := exec.CommandContext(ctx, path, "--version").Output()
		return err == nil && strings.TrimSpace(string(version)) == "ccusage "+usagereport.Version
	}
	if valid(target) {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(source, executable("ccusage")))
	if errors.Is(err, os.ErrNotExist) || source == directory {
		data, err = fetch(ctx, runtime.GOOS, runtime.GOARCH)
	}
	if err != nil {
		return err
	}
	staging, err := os.MkdirTemp(directory, ".collector-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	candidate := filepath.Join(staging, executable("ccusage"))
	if err := writeExecutable(candidate, data); err != nil {
		return err
	}
	if !valid(candidate) {
		return fmt.Errorf("expected a working ccusage %s executable", usagereport.Version)
	}
	return replaceFile(candidate, target)
}

func writeExecutable(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".executable-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0700); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return replaceFile(name, path)
}
func copyExecutable(from, to string) error {
	source, err := os.Stat(from)
	if err != nil {
		return err
	}
	if target, err := os.Stat(to); err == nil && os.SameFile(source, target) {
		return nil
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return writeExecutable(to, data)
}

func sourceEnvironment() []string {
	result := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !(strings.HasSuffix(key, "_PATH") || strings.HasSuffix(key, "_HOME") || strings.HasSuffix(key, "_DIR") || strings.HasSuffix(key, "_DIRS") || strings.HasSuffix(key, "_ROOT")) {
			continue
		}
		for _, prefix := range []string{"CLAUDE_", "CODEX_", "PI_", "AMP_", "DROID_", "HERMES_", "XDG_", "OPENCODE_", "CCUSAGE_", "GEMINI_", "GROK_", "ZCODE_", "KIMI_", "QWEN_", "KILO_", "COPILOT_", "ANTIGRAVITY_", "GOOSE_", "OPENCLAW_", "CODEBUFF_"} {
			if strings.HasPrefix(key, prefix) {
				result = append(result, entry)
				break
			}
		}
	}
	return result
}
