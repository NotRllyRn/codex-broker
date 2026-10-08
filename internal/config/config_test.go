package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadValidatesDeploymentBoundaries(t *testing.T) {
	t.Setenv("WINDOWKEEPER_DATA_DIR", t.TempDir()+"/data")
	t.Setenv("WINDOWKEEPER_RUNTIME_DIR", t.TempDir()+"/run")
	t.Setenv("WINDOWKEEPER_ROOT_PATH", "/broker")
	t.Setenv("WINDOWKEEPER_TRUSTED_PROXIES", "127.0.0.1,10.0.0.0/8")
	t.Setenv("WINDOWKEEPER_PUBLIC_ENROLLMENT_BROKER_URL", "https://broker.example:8787")
	settings, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if settings.RootPath != "/broker" || len(settings.TrustedProxies) != 2 || !settings.TrustedProxies[1].Contains(netip.MustParseAddr("10.1.2.3")) {
		t.Fatalf("unexpected settings: %#v", settings)
	}
}

func TestLoadRejectsUnsafeProxyAndEnrollmentOrigin(t *testing.T) {
	t.Setenv("WINDOWKEEPER_DATA_DIR", t.TempDir()+"/data")
	t.Setenv("WINDOWKEEPER_RUNTIME_DIR", t.TempDir()+"/run")
	t.Setenv("WINDOWKEEPER_TRUSTED_PROXIES", "*")
	if _, err := Load(); err == nil {
		t.Fatal("trusted proxy wildcard was accepted")
	}
	t.Setenv("WINDOWKEEPER_TRUSTED_PROXIES", "")
	t.Setenv("WINDOWKEEPER_PUBLIC_ENROLLMENT_BROKER_URL", "http://broker.example")
	if _, err := Load(); err == nil {
		t.Fatal("HTTP enrollment origin was accepted")
	}
}

func TestPublicEnrollmentKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enrollment-key")
	key := strings.Repeat("k", 32)
	if err := os.WriteFile(path, []byte(key+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WINDOWKEEPER_PUBLIC_ENROLLMENT_KEY_FILE", path)
	t.Setenv("WINDOWKEEPER_PUBLIC_ENROLLMENT_KEY", "ignored")
	settings, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if settings.PublicEnrollmentKey != key {
		t.Fatal("credential file did not override the environment value")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("unprotected enrollment key was accepted")
	}
	t.Setenv("WINDOWKEEPER_PUBLIC_ENROLLMENT_KEY_FILE", path+"-missing")
	if _, err := Load(); err == nil {
		t.Fatal("missing enrollment key was accepted")
	}
}
