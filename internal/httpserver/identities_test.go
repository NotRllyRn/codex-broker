package httpserver

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/identity"
	"github.com/NotRllyRn/codex-broker/internal/proxytelemetry"
	"github.com/NotRllyRn/codex-broker/internal/usage"
	"github.com/NotRllyRn/codex-broker/internal/webhook"
)

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
		t.Fatalf("create=%d %s", w.Code, w.Body.String())
	}
	start := strings.Index(w.Body.String(), "cbi_")
	if start < 0 {
		t.Fatal("code not shown")
	}
	code := w.Body.String()[start : start+47]
	if !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("secret response cacheable")
	}
	items, _ := identity.List(t.Context(), s.App.Store.DB)
	owner := items[0].ID
	if w := form("/settings/client-keys/"+key.ID+"/identity", url.Values{"identity_id": {owner}, "csrf_token": {session.CSRF}}, true); w.Code != 303 || w.Header().Get("Location") != "/broker/settings" {
		t.Fatalf("assign=%d %s", w.Code, w.Body.String())
	}
	call := func(path, token, encoding string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/broker"+path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Encoding", encoding)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	deviceToken := "cbu_" + core.RandomToken(32)
	enrollment, _ := json.Marshal(map[string]string{"code": code, "installation_id": "0123456789abcdef0123456789abcdef", "name": "Laptop", "token": deviceToken})
	w = call("/api/v1/usage/devices", "", "", enrollment)
	if w.Code != 200 {
		t.Fatalf("enroll=%d %s", w.Code, w.Body.String())
	}
	var device identity.Device
	if err := json.Unmarshal(w.Body.Bytes(), &device); err != nil {
		t.Fatal(err)
	}
	if device.IdentityID != owner {
		t.Fatal("wrong enrollment owner")
	}
	e := identity.Event{ID: identity.EventID("direct"), Source: "pi", TimestampMS: time.Now().UnixMilli(), Model: "gpt-test", InputTokens: 100, CachedInputTokens: 80, OutputTokens: 25, UsageKnown: true}
	body, _ := json.Marshal(map[string]any{"version": 1, "events": []identity.Event{e}})
	for _, bad := range []string{"", key.Token, code} {
		if got := call("/api/v1/usage/batches", bad, "", body); got.Code != 401 {
			t.Fatalf("bad token accepted: %d", got.Code)
		}
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, _ = gz.Write(body)
	_ = gz.Close()
	for _, want := range []string{`"inserted":1`, `"inserted":0`} {
		w = call("/api/v1/usage/batches", deviceToken, "gzip", compressed.Bytes())
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("upload=%d %s", w.Code, w.Body.String())
		}
	}
	malformed := []byte(strings.TrimSuffix(string(body), "}") + `,"identity_id":"other"}`)
	if got := call("/api/v1/usage/batches", deviceToken, "", malformed); got.Code != 422 {
		t.Fatalf("identity injection=%d", got.Code)
	}
	if got := call("/api/v1/usage/batches", deviceToken, "gzip", []byte("bad")); got.Code != 400 {
		t.Fatal(got.Code)
	}
	var bomb bytes.Buffer
	gz = gzip.NewWriter(&bomb)
	_, _ = gz.Write(bytes.Repeat([]byte(" "), (2<<20)+1))
	_ = gz.Close()
	if got := call("/api/v1/usage/batches", deviceToken, "gzip", bomb.Bytes()); got.Code != 413 {
		t.Fatalf("decompression limit=%d", got.Code)
	}
	if got := call("/api/v1/usage/batches", deviceToken, "br", body); got.Code != 415 {
		t.Fatal(got.Code)
	}
	if got := form("/settings/usage-devices/"+device.ID+"/revoke", url.Values{"csrf_token": {session.CSRF}}, true); got.Code != 303 {
		t.Fatal(got.Code)
	}
	if got := call("/api/v1/usage/batches", deviceToken, "", body); got.Code != 401 {
		t.Fatal("revocation failed")
	}
	// Codes are shown only in the issuing response, never on subsequent visits.
	r := httptest.NewRequest("GET", "/broker/settings", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, r)
	if page.Code != 200 || strings.Contains(page.Body.String(), code) || !strings.Contains(page.Body.String(), "Laptop") {
		t.Fatalf("settings=%d", page.Code)
	}
	if w := form("/settings/identities/"+owner+"/rename", url.Values{"name": {"Tim renamed"}, "csrf_token": {session.CSRF}}, true); w.Code != 303 {
		t.Fatal(w.Code)
	}
}

// Exercise the actual downloadable client against Broker over verified HTTPS.
func TestUsageSyncClientHTTPS(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	s, _ := proxyFixture(t, 0)
	i, code, err := identity.Create(t.Context(), s.App.Store, "HTTPS test")
	if err != nil {
		t.Fatal(err)
	}
	s.App.Config.RootPath = "/broker"
	server := httptest.NewTLSServer(s.rootPath(s.middleware(s.mux)))
	defer server.Close()
	root := t.TempDir()
	logs := filepath.Join(root, "logs")
	if err := os.Mkdir(logs, 0700); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logs, "session.jsonl")
	data := `{"type":"session_meta","timestamp":"2026-09-01T00:00:00Z","payload":{"id":"https-session"}}
{"type":"turn_context","timestamp":"2026-09-01T00:00:00Z","payload":{"model":"gpt-test"}}
{"type":"token_usage_record","timestamp":"2026-09-01T00:00:01Z","payload":{"response_id":"https-response","usage":{"input_tokens":100,"cached_input_tokens":80,"output_tokens":7}}}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	// Download the embedded helper, so packaging is tested as well as its source.
	response, err := server.Client().Get(server.URL + "/broker/static/usage-sync.py")
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "usage-sync.py")
	content, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 {
		t.Fatal("helper download", err)
	}
	if err := os.WriteFile(helper, content, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	command := exec.CommandContext(t.Context(), python, helper, "install", "--broker-url", server.URL+"/broker", "--ca", ca, "--name", "TLS laptop", "--state-dir", state, "--code-stdin", "--codex-mode", "direct", "--pi-mode", "off", "--codex-path", logs, "--no-schedule")
	command.Stdin = strings.NewReader(code + "\n")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("install: %v %s", err, output)
	}
	for range 2 {
		command = exec.CommandContext(t.Context(), python, filepath.Join(state, "usage-sync.py"), "sync", "--state-dir", state)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("sync: %v %s", err, output)
		}
	}
	var output, events int64
	if err := s.App.Store.DB.QueryRow("SELECT sum(output_tokens),count(*) FROM identity_usage WHERE identity_id=?", i.ID).Scan(&output, &events); err != nil || output != 7 || events != 1 {
		t.Fatalf("output=%d events=%d err=%v", output, events, err)
	}
	config, err := os.ReadFile(filepath.Join(state, "config.json"))
	if err != nil || bytes.Contains(config, []byte(code)) {
		t.Fatal("enrollment code retained after acknowledgement")
	}
}

func TestIdentityProxyAndDirectUsageSurvivePruning(t *testing.T) {
	s, key := proxyFixture(t, 1)
	ctx := t.Context()
	i, code, err := identity.Create(ctx, s.App.Store, "Tim")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.App.Store.DB.Exec("INSERT INTO identity_proxy_keys VALUES(?,?)", key.ID, i.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.proxyClient = &http.Client{Transport: proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(`data: {"type":"response.completed","response":{"id":"unique-response","model":"gpt-test","usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":80},"output_tokens":30,"total_tokens":150}}}` + "\n\n"))}, nil
	})}
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true,"input":"hello"}`))
	r.Header.Set("Authorization", "Bearer "+key.Token)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("proxy=%d %s", w.Code, w.Body.String())
	}
	token := "cbu_" + core.RandomToken(32)
	d, err := identity.Enroll(ctx, s.App.Store, code, "0123456789abcdef0123456789abcdef", "Laptop", token)
	if err != nil {
		t.Fatal(err)
	}
	e := identity.Event{ID: identity.EventID("direct"), Source: "codex", Model: "gpt-test", TimestampMS: time.Now().UnixMilli(), InputTokens: 10, OutputTokens: 5, UsageKnown: true}
	if _, err := identity.Ingest(ctx, s.App.Store, token, []identity.Event{e}); err != nil {
		t.Fatal(err)
	}
	rates := usage.ParseRates([]byte(`{"gpt-test":{"input_cost_per_token":0.000001,"output_cost_per_token":0.00001}}`))
	for device, want := range map[string]int64{"all": 35, "proxy": 30, d.ID: 5} {
		summary, err := usage.ReadIdentitySummary(ctx, s.App.Store.DB, rates, i.ID, device, 7, time.Now())
		if err != nil || summary.Output != want {
			t.Fatalf("device=%s summary=%+v err=%v", device, summary, err)
		}
	}
	if err := proxytelemetry.Prune(ctx, s.App.Store, time.Now().Add(time.Hour).UnixMilli(), 100); err != nil {
		t.Fatal(err)
	}
	summary, err := usage.ReadIdentitySummary(ctx, s.App.Store.DB, rates, i.ID, "all", 90, time.Now())
	if err != nil || summary.Output != 35 || summary.Responses != 2 {
		t.Fatalf("pruned=%+v err=%v", summary, err)
	}
}
