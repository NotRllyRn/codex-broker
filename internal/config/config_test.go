package config

import (
	"net/netip"
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
