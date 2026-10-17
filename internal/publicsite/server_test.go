package publicsite

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
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
