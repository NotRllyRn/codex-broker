package publicsite

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/flosch/pongo2/v6"
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

func TestEnrollmentThrottleUsesBrowserAndEnrollmentIdentity(t *testing.T) {
	server := &Server{}
	first := httptest.NewRequest(http.MethodPost, "/start", nil)
	first.RemoteAddr = "192.0.2.1:1000"
	second := httptest.NewRequest(http.MethodPost, "/start", nil)
	second.RemoteAddr = "192.0.2.1:2000"
	first.AddCookie(&http.Cookie{Name: clientCookieName, Value: base64.RawURLEncoding.EncodeToString(make([]byte, 24))})
	second.AddCookie(&http.Cookie{Name: clientCookieName, Value: base64.RawURLEncoding.EncodeToString([]byte("abcdefghijklmnopqrstuvwx"))})
	if server.throttleKey(first) == server.throttleKey(second) {
		t.Fatal("browsers behind one address share a throttle key")
	}
	handleValue := strings.Repeat("a", 32)
	first.AddCookie(&http.Cookie{Name: cookieName, Value: (Handle{handleValue, handleValue, handleValue, handleValue}).Encode()})
	if got := server.throttleKey(first); got != "enrollment:"+handleValue {
		t.Fatalf("status throttle key = %q", got)
	}
}

func TestEnrollmentPageSetsClientCookie(t *testing.T) {
	server := &Server{template: pongo2.Must(pongo2.FromString("ok"))}
	response := httptest.NewRecorder()
	server.page(response, httptest.NewRequest(http.MethodGet, "/", nil))
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != clientCookieName || clientCookie(httptestRequestWithCookie(cookies[0])) == "" {
		t.Fatalf("client cookie = %#v", cookies)
	}
}

func httptestRequestWithCookie(cookie *http.Cookie) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookie)
	return request
}

func TestEnrollmentPageExplainsPrivacyAndUsage(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"enabled":true}`))
	}))
	defer broker.Close()
	handler, err := New(config.Config{PublicEnrollmentBrokerURL: broker.URL, PublicEnrollmentKey: strings.Repeat("k", 32)})
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

func TestEnrollmentLinksForwardLabels(t *testing.T) {
	var received struct {
		Session string   `json:"session_token"`
		Labels  []string `json:"labels"`
	}
	enabled := true
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/availability") {
			_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": enabled})
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		value := strings.Repeat("a", 32)
		_ = json.NewEncoder(w).Encode(map[string]string{"enrollment_id": value, "login_attempt_id": value, "interaction_nonce": value})
	}))
	defer broker.Close()
	handler, err := New(config.Config{PublicEnrollmentBrokerURL: broker.URL, PublicEnrollmentKey: strings.Repeat("k", 32)})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/label/", "/label", "/unknown-label/", "/?labels=label,another-label"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "data-enrollment") {
			t.Fatalf("%s = %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/start?labels=label,another-label&labels=unknown-label", nil))
	if w.Code != 200 || len(received.Session) < 32 || strings.Join(received.Labels, ",") != "label,another-label,unknown-label" {
		t.Fatalf("start=%d body=%+v", w.Code, received)
	}
	// Repeated starts from one browser/address must exceed the former hourly limit.
	for i := 0; i < 25; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", "/start", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("start %d rejected: %d %s", i, w.Code, w.Body.String())
		}
	}
	enabled = false
	for _, path := range []string{"/label/", "/?labels=label"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 || w.Body.Len() != 0 {
			t.Fatalf("disabled %s = %d %s", path, w.Code, w.Body.String())
		}
	}
}

func TestEnrollmentStatusExposesOnlyDuplicateError(t *testing.T) {
	for _, code := range []string{"PUBLIC_ACCOUNT_EXISTS", "LOGIN_FAILED"} {
		t.Run(code, func(t *testing.T) {
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"state": "FAILED", "error_code": code, "error_summary": "internal details"})
			}))
			defer broker.Close()
			server := &Server{config: config.Config{PublicEnrollmentBrokerURL: broker.URL}, client: broker.Client(), status: newThrottle(10, time.Minute)}
			value := strings.Repeat("a", 32)
			request := httptestRequestWithCookie(&http.Cookie{Name: cookieName, Value: (Handle{value, value, value, value}).Encode()})
			response := httptest.NewRecorder()
			server.poll(response, request)
			var status map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if response.Code != 200 || status["state"] != "FAILED" || (status["error_code"] == "PUBLIC_ACCOUNT_EXISTS") != (code == "PUBLIC_ACCOUNT_EXISTS") || status["error_summary"] != "" {
				t.Fatalf("status = %d %s", response.Code, response.Body.String())
			}
		})
	}
}
