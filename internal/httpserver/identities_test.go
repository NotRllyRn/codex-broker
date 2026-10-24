package httpserver

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/identity"
	"github.com/NotRllyRn/codex-broker/internal/usagereport"
	"github.com/NotRllyRn/codex-broker/internal/webhook"
)

func dailyReport(day string, output int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"period":%q,"agent":"all","inputTokens":10,"outputTokens":%d,"cacheReadTokens":20,"cacheCreationTokens":3,"totalTokens":%d,"totalCost":0.5,"agents":[{"agent":"some-new-agent","inputTokens":10,"outputTokens":%d}]}`, day, output, 33+output, output))
}
func TestIdentitySettingsAndUploadBoundaries(t *testing.T) {
	s, key := proxyFixture(t, 1)
	var err error
	s.renderer, err = newRenderer("templates")
	if err != nil {
		t.Fatal(err)
	}
	s.App.Webhooks = webhook.New(s.App.Store, nil)
	if err := s.App.Admin.SetPassword(t.Context(), "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	session, err := s.App.Admin.Login(t.Context(), "correct horse battery staple", "test")
	if err != nil {
		t.Fatal(err)
	}
	s.App.Config.RootPath = "/broker"
	handler := s.rootPath(s.middleware(s.mux))
	form := func(path string, values url.Values, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/broker"+path, strings.NewReader(values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if auth {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := form("/settings/identities", url.Values{"name": {"Tim"}}, false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := form("/settings/identities", url.Values{"name": {"Tim"}}, true); w.Code != 403 {
		t.Fatal(w.Code)
	}
	w := form("/settings/identities", url.Values{"name": {"Tim"}, "csrf_token": {session.CSRF}}, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	start := strings.Index(w.Body.String(), "cbi_")
	if start < 0 {
		t.Fatal("code missing")
	}
	code := w.Body.String()[start : start+47]
	if !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("secret cached")
	}
	call := func(token, encoding string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/broker/api/v1/usage/batches", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Encoding", encoding)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	token := "cbu_" + core.RandomToken(32)
	device, err := identity.Enroll(t.Context(), s.App.Store, code, "0123456789abcdef0123456789abcdef", "Laptop", token)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(identity.ReportBatch{Version: 2, CCUsageVersion: usagereport.Version, Snapshots: []identity.Snapshot{{Revision: 1, Report: dailyReport("2026-09-01", 25)}}})
	for _, bad := range []string{"", key.Token, code} {
		if got := call(bad, "", body); got.Code != 401 {
			t.Fatal(got.Code)
		}
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, _ = gz.Write(body)
	_ = gz.Close()
	for _, want := range []string{`"updated":1`, `"updated":0`} {
		w = call(token, "gzip", compressed.Bytes())
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if got := call(token, "", []byte(`{"version":1,"events":[]}`)); got.Code != 422 || !strings.Contains(got.Body.String(), "Upgrade") {
		t.Fatal("old client not told to upgrade", got.Code, got.Body.String())
	}
	if got := call(token, "gzip", []byte("bad")); got.Code != 400 {
		t.Fatal(got.Code)
	}
	var bomb bytes.Buffer
	gz = gzip.NewWriter(&bomb)
	_, _ = gz.Write(bytes.Repeat([]byte(" "), (2<<20)+1))
	_ = gz.Close()
	if got := call(token, "gzip", bomb.Bytes()); got.Code != 413 {
		t.Fatal(got.Code)
	}
	if got := call(token, "br", body); got.Code != 415 {
		t.Fatal(got.Code)
	}
	if got := call(token, "", append(body, []byte(`{}`)...)); got.Code != 422 {
		t.Fatal(got.Code)
	}
	if got := form("/settings/usage-devices/"+device.ID+"/revoke", url.Values{"csrf_token": {session.CSRF}}, true); got.Code != 303 {
		t.Fatal(got.Code)
	}
	if got := call(token, "", body); got.Code != 401 {
		t.Fatal("revocation failed")
	}
	r := httptest.NewRequest("GET", "/broker/settings", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, r)
	if page.Code != 200 || strings.Contains(page.Body.String(), code) || !strings.Contains(page.Body.String(), "Laptop") {
		t.Fatal("settings", page.Code)
	}
}
