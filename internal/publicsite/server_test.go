package publicsite

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/config"
)

func TestEnrollmentHandleRoundTripAndValidation(t *testing.T) {
	value := "abcdefghijklmnopqrstuvwxyz123456"
	handle := Handle{value, value, value, value}
	if decoded := decodeHandle(handle.Encode()); decoded == nil || *decoded != handle {
		t.Fatalf("handle did not round trip: %#v", decoded)
	}
	extra := base64.RawURLEncoding.EncodeToString([]byte(`{"enrollment_id":"abcdefghijklmnopqrstuvwxyz123456","login_attempt_id":"abcdefghijklmnopqrstuvwxyz123456","session_token":"abcdefghijklmnopqrstuvwxyz123456","interaction_nonce":"abcdefghijklmnopqrstuvwxyz123456","extra":true}`))
	if decodeHandle(extra) != nil {
		t.Fatal("handle with unknown fields was accepted")
	}
	trailing := base64.RawURLEncoding.EncodeToString([]byte(`{"enrollment_id":"abcdefghijklmnopqrstuvwxyz123456","login_attempt_id":"abcdefghijklmnopqrstuvwxyz123456","session_token":"abcdefghijklmnopqrstuvwxyz123456","interaction_nonce":"abcdefghijklmnopqrstuvwxyz123456"}{}`))
	if decodeHandle(trailing) != nil {
		t.Fatal("handle with trailing JSON was accepted")
	}
}

func TestClientIPUsesOnlyTrustedForwarding(t *testing.T) {
	server := &Server{config: config.Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.17.60.10/32")}}}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "10.17.60.10:1234"
	request.Header.Set("X-Forwarded-For", "203.0.113.4, 10.17.60.10")
	if got := server.clientIP(request); got != "203.0.113.4" {
		t.Fatalf("trusted client IP = %q", got)
	}
	request.RemoteAddr = "10.17.60.11:1234"
	if got := server.clientIP(request); got != "10.17.60.11" {
		t.Fatalf("untrusted client IP = %q", got)
	}
	request.RemoteAddr = "10.17.60.10:1234"
	request.Header.Set("X-Forwarded-For", "invalid")
	if got := server.clientIP(request); got != "10.17.60.10" {
		t.Fatalf("invalid forwarded client IP = %q", got)
	}
}

func TestEnrollmentPageExplainsPrivacyAndUsage(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"enabled":true}`))
	}))
	defer broker.Close()
	handler, err := New(config.Config{PublicEnrollmentBrokerURL: broker.URL, PublicEnrollmentKey: strings.Repeat("k", 32), PublicEnrollmentAttemptsPerHour: 3})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	body := w.Body.String()
	textBody := strings.Join(strings.Fields(body), " ")
	for _, text := range []string{"Your chats stay private.", "ChatGPT conversations, memories, or Codex history", "<details", "What does connecting my account do?", "included with ChatGPT Edu", "giving the operator more shared capacity"} {
		if !strings.Contains(textBody, text) {
			t.Fatalf("page missing %q", text)
		}
	}
	if strings.Contains(body, "Only continue if you trust its operator") {
		t.Fatal("old trust warning is still present")
	}
}
