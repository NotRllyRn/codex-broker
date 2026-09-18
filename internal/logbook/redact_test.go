package logbook

import (
	"strings"
	"testing"
)

func TestRedactRemovesSecretsAndURLQueries(t *testing.T) {
	value := Redact(map[string]any{
		"access_token": "secret",
		"message":      "Bearer sk-abcdefghijklmnopqrstuvwxyz https://example.com/callback?code=secret&state=secret#fragment",
	}).(map[string]any)
	if value["access_token"] != "[REDACTED]" {
		t.Fatalf("access token = %q", value["access_token"])
	}
	message := value["message"].(string)
	for _, secret := range []string{"sk-abcdefghijklmnopqrstuvwxyz", "code=secret", "state=secret", "fragment"} {
		if strings.Contains(message, secret) {
			t.Fatalf("redacted message contains %q: %s", secret, message)
		}
	}
}

func TestRedactBoundsAndFlattensStrings(t *testing.T) {
	value := Redact(strings.Repeat("a", 9000) + "\nsecond line").(string)
	if len(value) != 8192 || strings.ContainsAny(value, "\r\n") {
		t.Fatalf("redacted string has length %d", len(value))
	}
}
