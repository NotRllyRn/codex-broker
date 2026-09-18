package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	DataDir                         string
	RuntimeDir                      string
	LogDir                          string
	Host                            string
	Port                            int
	RootPath                        string
	TrustedProxies                  []netip.Prefix
	TLSCertFile                     string
	TLSKeyFile                      string
	PublicEnrollmentKey             string
	PublicEnrollmentBrokerURL       string
	PublicEnrollmentCACert          string
	PublicEnrollmentHost            string
	PublicEnrollmentPort            int
	PublicEnrollmentTLSCertFile     string
	PublicEnrollmentTLSKeyFile      string
	PublicEnrollmentMaxActive       int
	PublicEnrollmentAttemptsPerHour int
	VaultKeyFile                    string
	VaultKey                        string
	AdminPasswordFile               string
	AdminPassword                   string
	CookieSecure                    string
	SessionIdleMinutes              int
	SessionAbsoluteHours            int
	UsagePollSeconds                int
	UsageRefreshConcurrency         int
	WindowPulseEnabled              bool
	WindowPulsePollSeconds          int
	WindowPulseRetrySeconds         int
	WindowPulseConcurrency          int
	AuthConcurrency                 int
	ProcessStartConcurrency         int
	ResetPaddingSeconds             int
	BrowserOAuthMode                string
	BrowserOAuthCallbackPorts       []int
	LoginTimeoutSeconds             int
	BrowserCallbackMaxBytes         int
	CodexExecutable                 string
	CodexVersion                    string
	LogLevel                        string
}

func Load() (Config, error) {
	_ = godotenv.Load()
	c := Config{
		DataDir:                     env("DATA_DIR", ".windowkeeper/data"),
		RuntimeDir:                  env("RUNTIME_DIR", ".windowkeeper/run"),
		LogDir:                      env("LOG_DIR", ""),
		Host:                        env("HOST", "127.0.0.1"),
		RootPath:                    env("ROOT_PATH", ""),
		TLSCertFile:                 env("TLS_CERT_FILE", ""),
		TLSKeyFile:                  env("TLS_KEY_FILE", ""),
		PublicEnrollmentKey:         env("PUBLIC_ENROLLMENT_KEY", ""),
		PublicEnrollmentBrokerURL:   env("PUBLIC_ENROLLMENT_BROKER_URL", "https://codex-broker:8787"),
		PublicEnrollmentCACert:      env("PUBLIC_ENROLLMENT_CA_CERT", ""),
		PublicEnrollmentHost:        env("PUBLIC_ENROLLMENT_HOST", "127.0.0.1"),
		PublicEnrollmentTLSCertFile: env("PUBLIC_ENROLLMENT_TLS_CERT_FILE", ""),
		PublicEnrollmentTLSKeyFile:  env("PUBLIC_ENROLLMENT_TLS_KEY_FILE", ""),
		VaultKeyFile:                env("VAULT_KEY_FILE", ""),
		VaultKey:                    env("VAULT_KEY", ""),
		AdminPasswordFile:           env("ADMIN_PASSWORD_FILE", ""),
		AdminPassword:               env("ADMIN_PASSWORD", ""),
		CookieSecure:                env("COOKIE_SECURE", "auto"),
		BrowserOAuthMode:            env("BROWSER_OAUTH_MODE", "manual"),
		CodexExecutable:             env("CODEX_EXECUTABLE", "codex"),
		CodexVersion:                env("CODEX_VERSION", "unknown"),
		LogLevel:                    env("LOG_LEVEL", "INFO"),
	}
	var err error
	for target, spec := range map[*int]struct {
		name       string
		value, min int
		max        int
	}{
		&c.Port:                            {"PORT", 8787, 1, 65535},
		&c.PublicEnrollmentPort:            {"PUBLIC_ENROLLMENT_PORT", 8788, 1, 65535},
		&c.PublicEnrollmentMaxActive:       {"PUBLIC_ENROLLMENT_MAX_ACTIVE", 4, 1, 32},
		&c.PublicEnrollmentAttemptsPerHour: {"PUBLIC_ENROLLMENT_ATTEMPTS_PER_HOUR", 3, 1, 20},
		&c.SessionIdleMinutes:              {"SESSION_IDLE_MINUTES", 44640, 1, 0},
		&c.SessionAbsoluteHours:            {"SESSION_ABSOLUTE_HOURS", 2160, 1, 0},
		&c.UsagePollSeconds:                {"USAGE_POLL_SECONDS", 300, 60, 0},
		&c.UsageRefreshConcurrency:         {"USAGE_REFRESH_CONCURRENCY", 4, 1, 16},
		&c.WindowPulsePollSeconds:          {"WINDOW_PULSE_POLL_SECONDS", 60, 10, 0},
		&c.WindowPulseRetrySeconds:         {"WINDOW_PULSE_RETRY_SECONDS", 900, 60, 0},
		&c.WindowPulseConcurrency:          {"WINDOW_PULSE_CONCURRENCY", 2, 1, 8},
		&c.AuthConcurrency:                 {"AUTH_CONCURRENCY", 2, 1, 8},
		&c.ProcessStartConcurrency:         {"PROCESS_START_CONCURRENCY", 2, 1, 8},
		&c.ResetPaddingSeconds:             {"RESET_PADDING_SECONDS", 10, 0, 300},
		&c.LoginTimeoutSeconds:             {"LOGIN_TIMEOUT_SECONDS", 900, 60, 3600},
		&c.BrowserCallbackMaxBytes:         {"BROWSER_CALLBACK_MAX_BYTES", 16384, 1024, 65536},
	} {
		*target, err = envInt(spec.name, spec.value, spec.min, spec.max)
		if err != nil {
			return Config{}, err
		}
	}
	c.WindowPulseEnabled, err = envBool("WINDOW_PULSE_ENABLED", true)
	if err != nil {
		return Config{}, err
	}
	if c.LogDir == "" {
		c.LogDir = filepath.Join(c.DataDir, "logs")
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func env(name, fallback string) string {
	if value, ok := os.LookupEnv("WINDOWKEEPER_" + name); ok {
		return value
	}
	return fallback
}

func envInt(name string, fallback, min, max int) (int, error) {
	value, err := strconv.Atoi(env(name, strconv.Itoa(fallback)))
	if err != nil || value < min || max > 0 && value > max {
		return 0, fmt.Errorf("WINDOWKEEPER_%s is invalid", name)
	}
	return value, nil
}

func envBool(name string, fallback bool) (bool, error) {
	value, err := strconv.ParseBool(env(name, strconv.FormatBool(fallback)))
	if err != nil {
		return false, fmt.Errorf("WINDOWKEEPER_%s is invalid", name)
	}
	return value, nil
}

func (c *Config) validate() error {
	data, _ := filepath.Abs(c.DataDir)
	runtime, _ := filepath.Abs(c.RuntimeDir)
	if data == runtime {
		return errors.New("persistent and runtime directories must differ")
	}
	if c.VaultKeyFile != "" {
		key, _ := filepath.Abs(c.VaultKeyFile)
		relative, err := filepath.Rel(data, key)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("vault key file cannot be under the data directory")
		}
	}
	if c.RootPath == "/" {
		c.RootPath = ""
	}
	if c.RootPath != "" && (!strings.HasPrefix(c.RootPath, "/") || strings.HasSuffix(c.RootPath, "/")) {
		return errors.New("root_path must start with / and cannot end with /")
	}
	if !oneOf(c.CookieSecure, "auto", "true", "false") {
		return errors.New("cookie_secure must be auto, true, or false")
	}
	if !oneOf(c.BrowserOAuthMode, "disabled", "manual", "host-loopback") {
		return errors.New("browser_oauth_mode is invalid")
	}
	if c.PublicEnrollmentKey != "" && len(c.PublicEnrollmentKey) < 32 {
		return errors.New("public enrollment key must contain at least 32 characters")
	}
	parsed, err := url.Parse(c.PublicEnrollmentBrokerURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("public enrollment broker URL must be an HTTPS origin")
	}
	c.PublicEnrollmentBrokerURL = strings.TrimSuffix(c.PublicEnrollmentBrokerURL, "/")
	for _, part := range strings.Split(env("TRUSTED_PROXIES", ""), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "*" {
			return errors.New("trusted proxy wildcard is forbidden")
		}
		prefix, err := parsePrefix(part)
		if err != nil {
			return errors.New("trusted proxies must be IP addresses or CIDR ranges")
		}
		c.TrustedProxies = append(c.TrustedProxies, prefix)
	}
	ports := map[int]bool{}
	for _, part := range strings.Split(env("BROWSER_OAUTH_CALLBACK_PORTS", "1455,1457"), ",") {
		port, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || port != 1455 && port != 1457 {
			return errors.New("callback ports must match the pinned compatibility profile")
		}
		ports[port] = true
	}
	for _, port := range []int{1455, 1457} {
		if ports[port] {
			c.BrowserOAuthCallbackPorts = append(c.BrowserOAuthCallbackPorts, port)
		}
	}
	return nil
}

func parsePrefix(value string) (netip.Prefix, error) {
	if strings.Contains(value, "/") {
		return netip.ParsePrefix(value)
	}
	address, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(address, address.BitLen()), nil
}

func oneOf(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

func IsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	return net.ParseIP(host).IsLoopback()
}
