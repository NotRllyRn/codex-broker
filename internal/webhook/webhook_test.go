package webhook

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

func TestWebhookURLRejectsPrivateDestinations(t *testing.T) {
	for _, target := range []string{"http://example.com/hook", "https://localhost/hook", "https://127.0.0.1/hook", "https://10.0.0.1/hook", "https://[::1]/hook"} {
		if err := validateURL(target); err == nil {
			t.Errorf("accepted %s", target)
		}
	}
	if err := validateURL("https://hooks.example.com/path"); err != nil {
		t.Fatal(err)
	}
}

func TestProviderBodiesPreserveNotificationContract(t *testing.T) {
	event := map[string]any{
		"event_id": "event", "subject": "account:primary", "occurred_at": "2026-01-01T00:00:00.000000+00:00",
		"notification": map[string]any{"code": "CB-101", "title": "INCIDENT OPENED"},
		"data":         map[string]any{"account_name": "Primary", "severity": "ERROR", "incident_status": "OPEN", "summary": "Authentication failed", "recommended_action": "Sign in again"},
	}
	for _, kind := range []string{"slack", "discord"} {
		var body map[string]any
		if err := json.Unmarshal(providerBody(kind, event), &body); err != nil {
			t.Fatal(err)
		}
		if body["allowed_mentions"] == nil && body["blocks"] == nil {
			t.Fatalf("%s body lost provider structure: %#v", kind, body)
		}
		encoded, _ := json.Marshal(body)
		if !strings.Contains(string(encoded), "Authentication failed") || !strings.Contains(string(encoded), "CB-101") {
			t.Fatalf("%s body lost incident detail: %s", kind, encoded)
		}
	}
}

func TestPublicAddress(t *testing.T) {
	for _, address := range []string{"192.168.1.1", "192.0.2.1", "198.51.100.2", "203.0.113.3", "100.64.0.1", "2001:db8::1"} {
		if publicAddress(netip.MustParseAddr(address)) {
			t.Errorf("special-use address %s was public", address)
		}
	}
	if !publicAddress(netip.MustParseAddr("1.1.1.1")) || !publicAddress(netip.MustParseAddr("2606:4700:4700::1111")) {
		t.Fatal("public address was blocked")
	}
}
