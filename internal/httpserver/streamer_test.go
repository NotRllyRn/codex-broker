package httpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/relayprotocol"
	"github.com/NotRllyRn/codex-broker/internal/webhook"
	"github.com/flosch/pongo2/v6"
)

func TestClientKeyStreamerMode(t *testing.T) {
	s, key := proxyFixture(t, 1)
	ctx := t.Context()
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(proxyUsageStream("response", "completed", 150)))}, nil
	})
	for _, enabled := range []bool{false, true, false} {
		if err := s.App.ClientKeys.SetStreamerMode(ctx, key.ID, enabled); err != nil {
			t.Fatal(err)
		}
		authenticated, err := s.App.ClientKeys.Authenticate(ctx, key.Token)
		if err != nil || authenticated.StreamerMode != enabled {
			t.Fatalf("authenticate=%+v err=%v", authenticated, err)
		}
		keys, err := s.App.ClientKeys.List(ctx)
		if err != nil || len(keys) != 1 || keys[0].StreamerMode != enabled {
			t.Fatalf("keys=%+v err=%v", keys, err)
		}
		snapshot, err := s.App.RelaySnapshot(ctx, "edge")
		if err != nil || len(snapshot.Clients) != 1 || snapshot.Clients[0].StreamerMode != enabled {
			t.Fatalf("snapshot=%+v err=%v", snapshot, err)
		}
		if snapshot.ProtocolVersion != relayprotocol.Version {
			t.Fatal("wrong protocol")
		}
		response := proxyRequest(s, key.Token, "/v1/responses", `{"model":"test","input":[]}`)
		body := response.Body.String()
		if response.Code != 200 || !strings.Contains(body, "5h 100%") || !strings.Contains(body, "Hello") {
			t.Fatalf("response=%d %s", response.Code, body)
		}
		if enabled && (strings.Contains(body, "account0") || !strings.Contains(body, "Usage stats")) {
			t.Fatalf("private notice=%s", body)
		}
		if !enabled && !strings.Contains(body, "🔨 account0") {
			t.Fatalf("normal notice=%s", body)
		}
		lease := proxyRequest(s, key.Token, "/api/v1/route", `{"session_id":"session","turn_id":"turn"}`)
		var result map[string]any
		if err := json.Unmarshal(lease.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		expected := "account0"
		if enabled {
			expected = "Usage stats"
		}
		if lease.Code != 200 || result["account_label"] != expected {
			t.Fatalf("lease=%d %s", lease.Code, lease.Body.String())
		}
	}
	// Privacy changes attribution only; internal metering still uses the real account.
	var total int
	if err := s.App.Store.DB.QueryRow("SELECT count(*) FROM proxy_responses WHERE account_id='account0' AND total_tokens=150").Scan(&total); err != nil || total != 3 {
		t.Fatalf("telemetry=%d err=%v", total, err)
	}
	other, err := s.App.ClientKeys.Create(ctx, "Work laptop")
	if err != nil {
		t.Fatal(err)
	}
	authenticated, err := s.App.ClientKeys.Authenticate(ctx, other.Token)
	if err != nil || authenticated.StreamerMode {
		t.Fatalf("new key default=%+v err=%v", authenticated, err)
	}
	if _, err := s.App.ClientKeys.Revoke(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{key.ID, "missing"} {
		if err := s.App.ClientKeys.SetStreamerMode(ctx, id, true); err == nil {
			t.Fatal("updated missing or revoked key")
		}
	}
}

func TestStreamerSettingsAuthorizationAndBrowserPreference(t *testing.T) {
	s, key := proxyFixture(t, 1)
	ctx := t.Context()
	var err error
	s.renderer, err = newRenderer("templates")
	if err != nil {
		t.Fatal(err)
	}
	s.App.Webhooks = webhook.New(s.App.Store, nil)
	if err := s.App.Admin.SetPassword(ctx, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	session, err := s.App.Admin.Login(ctx, "correct horse battery staple", "test")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, form url.Values, login, private bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if login {
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
		}
		if private {
			req.AddCookie(&http.Cookie{Name: "codex-broker.streamer", Value: "on"})
		}
		response := httptest.NewRecorder()
		s.mux.ServeHTTP(response, req)
		return response
	}
	path := "/settings/client-keys/" + key.ID + "/streamer-mode"
	form := url.Values{"enabled": {"true"}, "csrf_token": {session.CSRF}}
	if got := request("POST", path, form, false, false); got.Code != 401 {
		t.Fatalf("session=%d", got.Code)
	}
	if got := request("POST", path, url.Values{"enabled": {"true"}}, true, false); got.Code != 403 {
		t.Fatalf("csrf=%d", got.Code)
	}
	// Dashboard privacy has no effect on the saved key flag.
	page := request("GET", "/settings", nil, true, true)
	if page.Code != 200 || !strings.Contains(page.Body.String(), key.Name) || !strings.Contains(page.Body.String(), key.Prefix) || !strings.Contains(page.Body.String(), "Streamer mode: off</button>") {
		t.Fatalf("unmasked settings=%d %s", page.Code, page.Body.String())
	}
	if got := request("POST", path, form, true, true); got.Code != 303 {
		t.Fatalf("update=%d %s", got.Code, got.Body.String())
	}
	page = request("GET", "/settings", nil, true, false)
	if page.Code != 200 || !strings.Contains(page.Body.String(), key.Name) || !strings.Contains(page.Body.String(), "Streamer mode: on</button>") {
		t.Fatalf("settings=%d %s", page.Code, page.Body.String())
	}
	if got := request("POST", "/settings/client-keys/missing/streamer-mode", form, true, false); got.Code != 404 {
		t.Fatalf("missing=%d", got.Code)
	}
}

func TestStreamerRenderingIsDashboardOnly(t *testing.T) {
	renderer, err := newRenderer("templates")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{renderer: renderer}
	const sensitive = "private-example@example.test"
	cases := map[string]pongo2.Context{
		"dashboard.html":      {"accounts": []map[string]any{{"display_name": sensitive, "labels": []string{sensitive}, "evidence": sensitive}}, "profiles": []map[string]any{{"ID": "all", "Label": sensitive}}, "identities": []map[string]any{{"ID": "identity", "Name": sensitive}}, "usage_devices": []map[string]any{{"Name": sensitive, "ID": "device"}}},
		"account_detail.html": {"detail": map[string]any{"account": map[string]any{"display_name": sensitive, "upstream_email": sensitive, "labels": []string{sensitive}}}},
		"settings.html":       {"client_keys": []map[string]any{{"name": sensitive, "key_prefix": sensitive}}, "identities": []map[string]any{{"ID": "identity", "Name": sensitive}}, "issued_key": map[string]any{"token": sensitive}},
	}
	for name, context := range cases {
		t.Run(name, func(t *testing.T) {
			for _, enabled := range []bool{true, false} {
				request := httptest.NewRequest("GET", "/", nil)
				if enabled {
					request.AddCookie(&http.Cookie{Name: "codex-broker.streamer", Value: "on"})
				}
				response := httptest.NewRecorder()
				if err := s.render(response, request, 200, name, context); err != nil {
					t.Fatal(err)
				}
				body := response.Body.String()
				masked := enabled && name == "dashboard.html"
				if strings.Contains(body, sensitive) == masked {
					t.Fatalf("page=%s privacy=%v body=%s", name, enabled, body)
				}
				if strings.Contains(body, "data-streamer-toggle") != (name == "dashboard.html") {
					t.Fatal("dashboard toggle shown on wrong page")
				}
				if name == "settings.html" && (!strings.Contains(body, ">Create key</button>") || !strings.Contains(body, ">Create identity and code</button>") || !strings.Contains(body, "/rename\"")) {
					t.Fatal("settings controls hidden")
				}
			}
		})
	}
}
