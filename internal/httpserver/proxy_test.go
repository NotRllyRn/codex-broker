package httpserver

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/broker"
	"github.com/NotRllyRn/codex-broker/internal/codex"
	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/NotRllyRn/codex-broker/internal/events"
	"github.com/NotRllyRn/codex-broker/internal/logbook"
	"github.com/NotRllyRn/codex-broker/internal/relayprotocol"
	"github.com/NotRllyRn/codex-broker/internal/responsesproxy"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

type proxyRoundTrip func(*http.Request) (*http.Response, error)

func (f proxyRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// HTTP tests need isolated data, not a repeat of every historical migration.
// Cache only the closed, empty database; each fixture writes its own copy.
var proxyDatabaseSeed = sync.OnceValues(func() ([]byte, error) {
	root, err := os.MkdirTemp("", "codex-broker-proxy-seed-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	path := filepath.Join(root, "broker.db")
	database, err := store.Open(context.Background(), path)
	if err != nil {
		return nil, err
	}
	if err := database.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
})

func proxyFixture(t *testing.T, accounts int) (*Server, auth.IssuedClientKey) {
	t.Helper()
	ctx, root := context.Background(), t.TempDir()
	seed, err := proxyDatabaseSeed()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "broker.db"), seed, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(root, "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	instance, err := db.InstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := vault.New(make([]byte, 32), instance)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "codex")
	command := fmt.Sprintf("#!/bin/sh\nexec %s -test.run=^TestProxyFakeAppServer$ -- \"$@\"\n", strconv.Quote(os.Args[0]))
	if err := os.WriteFile(script, []byte(command), 0700); err != nil {
		t.Fatal(err)
	}
	settings := config.Config{RuntimeDir: filepath.Join(root, "run"), CodexExecutable: script, CodexVersion: "test", ProcessStartConcurrency: 2, UsageRefreshConcurrency: 2, WindowPulseConcurrency: 2, AuthConcurrency: 2}
	service := broker.NewService(db, settings, secrets, codex.NewRuntimeManager(settings, secrets), events.New(100, 10))
	t.Cleanup(service.Close)
	logs, err := logbook.Open(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logs.Close() })
	app := &broker.Application{Config: settings, Store: db, Service: service, ClientKeys: auth.NewClientKeys(db), Admin: auth.NewAdmin(db, 30, 24), Router: broker.Router{Store: db, Service: service, PaddingSeconds: 3}, Logs: logs}
	server := &Server{App: app, mux: http.NewServeMux(), clientAuth: newThrottle(5, time.Minute), proxyClient: responsesproxy.NewClient()}
	server.routes()
	for i := 0; i < accounts; i++ {
		id := fmt.Sprintf("account%d", i)
		_, err := db.DB.Exec(`INSERT INTO accounts(account_id,public_token,display_name,enabled,lifecycle_state,created_at_ms,updated_at_ms) VALUES(?,?,?,1,'ACTIVE',?,1); INSERT INTO account_state(account_id,auth_state,worker_state,overall_state,usage_state,state_version,updated_at_ms) VALUES(?,'VERIFIED','STOPPED','HEALTHY','FRESH',1,1); INSERT INTO usage_current(account_id,short_used_percent_raw,weekly_used_percent_raw) VALUES(?,0,0)`, id, id, id, i+1, id, id)
		if err != nil {
			t.Fatal(err)
		}
		home := filepath.Join(root, id)
		if err := os.Mkdir(home, 0700); err != nil {
			t.Fatal(err)
		}
		writeProxyAuth(t, home, id)
		payload, err := secrets.Capture(home, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := secrets.Encrypt(id, payload)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.DB.Exec("INSERT INTO credential_bundles VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", envelope.BundleID, id, "ACTIVE", envelope.EnvelopeVersion, envelope.PayloadSchemaVersion, envelope.KeyID, envelope.Nonce, envelope.Ciphertext, envelope.AAD, "test", 1, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	key, err := app.ClientKeys.Create(ctx, "Generic app")
	if err != nil {
		t.Fatal(err)
	}
	return server, key
}

func writeProxyAuth(t *testing.T, home, id string) {
	t.Helper()
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": id}})
	token := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	body, _ := json.Marshal(map[string]any{"tokens": map[string]string{"access_token": token, "refresh_token": "secret-refresh"}})
	if err := os.WriteFile(filepath.Join(home, "auth.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
}

// Launched by RuntimeManager; no network or real account credentials are used.
func TestProxyFakeAppServer(t *testing.T) {
	home := os.Getenv("CODEX_HOME")
	if home == "" || !strings.Contains(home, "/run/accounts/") {
		return
	}
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	pulsed := false
	usageFixturePath := filepath.Join(filepath.Dir(filepath.Dir(home)), "usage-fixture.json")
	var usageFixture struct {
		ResetS int64 `json:"reset_s"`
		Used   int64 `json:"used"`
	}
	usageFixtureJSON, _ := os.ReadFile(usageFixturePath)
	_ = json.Unmarshal(usageFixtureJSON, &usageFixture)
	for {
		var request map[string]any
		if decoder.Decode(&request) != nil {
			os.Exit(0)
		}
		id, ok := request["id"]
		if !ok {
			continue
		}
		result := map[string]any{}
		content, _ := os.ReadFile(filepath.Join(home, "auth.json"))
		var fixture map[string]any
		_ = json.Unmarshal(content, &fixture)
		failure, _ := fixture["test_failure"].(string)
		if failure != "" && (request["method"] == "account/read" || request["method"] == "account/rateLimits/read") {
			_ = encoder.Encode(map[string]any{"id": id, "error": map[string]any{"message": failure}})
			continue
		}
		if request["method"] == "account/read" {
			writeProxyAuth(t, home, "refreshed")
			result["account"] = map[string]string{"type": "chatgpt", "email": "test@example.com"}
		}
		switch request["method"] {
		case "model/list":
			result["data"] = []any{map[string]any{"model": "test-mini", "inputModalities": []string{"text"}, "supportedReasoningEfforts": []any{map[string]string{"reasoningEffort": "low"}}}}
		case "thread/start":
			result["thread"] = map[string]string{"id": "thread"}
		case "turn/start":
			pulsed = true
			if len(usageFixtureJSON) > 0 {
				_ = os.WriteFile(usageFixturePath+".pulsed", []byte("1"), 0600)
			}
			result["turn"] = map[string]string{"id": "turn"}
		case "account/rateLimits/read":
			if len(usageFixtureJSON) > 0 {
				used := usageFixture.Used
				reset := usageFixture.ResetS
				if reset == 0 {
					reset = time.Now().Add(7 * 24 * time.Hour).Unix()
				}
				if pulsed {
					used = 1
				}
				result["rateLimitsByLimitId"] = map[string]any{"codex": map[string]any{"primary": map[string]any{"usedPercent": used, "windowDurationMins": 300, "resetsAt": time.Now().Add(5 * time.Hour).Unix()}, "secondary": map[string]any{"usedPercent": used, "windowDurationMins": 10080, "resetsAt": reset}}}
				break
			}
			result["rateLimitsByLimitId"] = map[string]any{"codex": map[string]any{"primary": map[string]any{"usedPercent": 1, "windowDurationMins": 300, "resetsAt": time.Now().Add(5 * time.Hour).Unix()}, "secondary": map[string]any{"usedPercent": 1, "windowDurationMins": 10080, "resetsAt": time.Now().Add(7 * 24 * time.Hour).Unix()}}}
		}
		_ = encoder.Encode(map[string]any{"id": id, "result": result})
		if request["method"] == "turn/start" {
			_ = encoder.Encode(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread", "turnId": "turn", "item": map[string]string{"type": "agentMessage", "text": "OK"}}})
			_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]string{"id": "turn", "status": "completed"}}})
		}
	}
}

func proxyRequest(server *Server, key, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	server.middleware(server.mux).ServeHTTP(w, r)
	return w
}

func proxyMethodRequest(server *Server, key, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	server.middleware(server.mux).ServeHTTP(w, r)
	return w
}

func proxyCorrelatedRequest(server *Server, key, threadID, turnID, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set(responsesproxy.ThreadIDHeader, threadID)
	r.Header.Set(responsesproxy.TurnIDHeader, turnID)
	w := httptest.NewRecorder()
	server.middleware(server.mux).ServeHTTP(w, r)
	return w
}

func TestProxyCodexRoutes(t *testing.T) {
	tests := []struct {
		method, path, upstream string
	}{
		{http.MethodGet, "/v1/models?client_version=1.2.3", responsesproxy.UpstreamURL + "/models?client_version=1.2.3"},
		{http.MethodGet, "/models", responsesproxy.UpstreamURL + "/models"},
		{http.MethodPost, "/v1/images/generations", responsesproxy.UpstreamURL + "/images/generations"},
		{http.MethodPost, "/images/edits", responsesproxy.UpstreamURL + "/images/edits"},
		{http.MethodPost, "/transcribe", responsesproxy.BackendURL + "/transcribe"},
		{http.MethodGet, "/thread/goal/get?thread_id=test", responsesproxy.UpstreamURL + "/thread/goal/get?thread_id=test"},
		{http.MethodPost, "/codex/thread/goal/set", responsesproxy.UpstreamURL + "/thread/goal/set"},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			s, key := proxyFixture(t, 1)
			s.proxyClient.Transport = proxyRoundTrip(func(r *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(r.Body)
				if r.Method != test.method || r.URL.String() != test.upstream || string(body) != map[bool]string{true: "payload"}[test.method == http.MethodPost] {
					t.Errorf("request=%s %s body=%q", r.Method, r.URL, body)
				}
				if test.path == "/transcribe" && (r.Header.Get("Content-Type") != "multipart/form-data; boundary=fixture" || r.Header.Get("Content-Encoding") != "gzip") {
					t.Errorf("transcription headers=%v", r.Header)
				}
				if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer header.") || r.Header.Get("ChatGPT-Account-ID") != "account0" {
					t.Error("managed credentials missing")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("opaque"))}, nil
			})
			body := ""
			if test.method == http.MethodPost {
				body = "payload"
			}
			r := httptest.NewRequest(test.method, test.path, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+key.Token)
			if test.path == "/transcribe" {
				r.Header.Set("Content-Type", "multipart/form-data; boundary=fixture")
				r.Header.Set("Content-Encoding", "gzip")
			}
			got := httptest.NewRecorder()
			s.middleware(s.mux).ServeHTTP(got, r)
			if got.Code != 200 || got.Body.String() != "opaque" {
				t.Fatalf("response=%d %q", got.Code, got.Body.String())
			}
		})
	}
}

func TestProxyResponsesStatusNotice(t *testing.T) {
	s, key := proxyFixture(t, 1)
	now := time.Now()
	_, err := s.App.Store.DB.Exec(`UPDATE accounts SET display_name='Account' WHERE account_id='account0'; UPDATE usage_current SET short_used_percent_raw=30,short_resets_at_s=?,weekly_used_percent_raw=5,weekly_resets_at_s=? WHERE account_id='account0'`, now.Add(4*time.Hour+36*time.Minute).Unix(), now.Add(89*time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	s.proxyClient.Transport = proxyRoundTrip(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "🔨 Previous") {
			t.Error("replayed broker notice reached upstream")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader(proxyStatusStream))}, nil
	})
	request := `{"model":"test","input":[{"role":"assistant","content":"🔨 Previous · 5h 1% 1m · 7d 2% 2m\n\nOld answer"},{"role":"user","content":"next"}]}`
	got := proxyRequest(s, key.Token, "/v1/responses", request)
	if got.Code != 200 || !strings.Contains(got.Body.String(), "🔨 Account · 5h 70% 4h36m · 7d 95% 3d17h") || !strings.Contains(got.Body.String(), "Hello") {
		t.Fatalf("response=%d %s", got.Code, got.Body.String())
	}
	if got.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content-type=%s", got.Header().Get("Content-Type"))
	}
}

const proxyStatusStream = "data: {\"type\":\"response.created\",\"response\":{\"output\":[]}}\n\n" +
	"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_answer\",\"type\":\"message\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[]}}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"item_id\":\"msg_answer\",\"content_index\":0,\"delta\":\"Hello\"}\n\n" +
	"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg_answer\",\"type\":\"message\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello\"}]}}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n"

func proxyUsageStream(id, status string, total int) string {
	terminal := fmt.Sprintf(`data: {"type":"response.%s","response":{"id":%q,"model":"gpt-test-result","service_tier":"priority","output":[],"usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":80},"output_tokens":30,"output_tokens_details":{"reasoning_tokens":12},"total_tokens":%d,"future_counter":7}}}`+"\n\n", status, id, total)
	return strings.Replace(proxyStatusStream, `data: {"type":"response.completed","response":{"output":[]}}`+"\n\n", terminal, 1)
}

func TestProxyStoresResponseUsageAndCorrelation(t *testing.T) {
	s, key := proxyFixture(t, 1)
	s.proxyClient.Transport = proxyRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get(responsesproxy.ThreadIDHeader) != "" || r.Header.Get(responsesproxy.TurnIDHeader) != "" {
			t.Error("broker correlation headers reached upstream")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(proxyUsageStream("resp_123", "completed", 150)))}, nil
	})
	got := proxyCorrelatedRequest(s, key.Token, "thread-1", "turn-1", `{"model":"gpt-test-request","service_tier":"default","previous_response_id":"resp_122","reasoning":{"effort":"high"},"input":[]}`)
	if got.Code != http.StatusOK {
		t.Fatalf("response=%d %s", got.Code, got.Body.String())
	}
	var accountID, clientKeyID, threadID, turnID, endpoint, requestModel, effort, requestTier string
	var previousID, responseID, responseModel, responseTier, status, usageJSON string
	var attempts, httpStatus, inputTokens, cachedTokens, outputTokens, reasoningTokens, totalTokens int64
	var requestStarted, upstreamStarted, headersAt, firstEventAt, terminalAt int64
	err := s.App.Store.DB.QueryRow(`SELECT
		account_id,client_key_id,thread_id,turn_id,endpoint,request_model,reasoning_effort,request_service_tier,
		previous_response_id,response_id,response_model,response_service_tier,terminal_status,upstream_attempts,upstream_http_status,
		request_started_at_ms,upstream_started_at_ms,response_headers_at_ms,first_response_event_at_ms,terminal_event_at_ms,
		input_tokens,cached_input_tokens,output_tokens,reasoning_output_tokens,total_tokens,usage_json
		FROM proxy_responses`).Scan(
		&accountID, &clientKeyID, &threadID, &turnID, &endpoint, &requestModel, &effort, &requestTier,
		&previousID, &responseID, &responseModel, &responseTier, &status, &attempts, &httpStatus,
		&requestStarted, &upstreamStarted, &headersAt, &firstEventAt, &terminalAt,
		&inputTokens, &cachedTokens, &outputTokens, &reasoningTokens, &totalTokens, &usageJSON)
	if err != nil {
		t.Fatal(err)
	}
	if accountID != "account0" || clientKeyID != key.ID || threadID != "thread-1" || turnID != "turn-1" || endpoint != "/v1/responses" ||
		requestModel != "gpt-test-request" || effort != "high" || requestTier != "default" || previousID != "resp_122" ||
		responseID != "resp_123" || responseModel != "gpt-test-result" || responseTier != "priority" || status != "completed" ||
		attempts != 1 || httpStatus != 200 || inputTokens != 120 || cachedTokens != 80 || outputTokens != 30 ||
		reasoningTokens != 12 || totalTokens != 150 || !strings.Contains(usageJSON, `"future_counter":7`) {
		t.Fatalf("stored response telemetry does not match the proxied response")
	}
	if requestStarted > upstreamStarted || upstreamStarted > headersAt || headersAt > firstEventAt || firstEventAt > terminalAt {
		t.Fatalf("timestamps out of order: %d %d %d %d %d", requestStarted, upstreamStarted, headersAt, firstEventAt, terminalAt)
	}
	var aggregateRows, refreshes int
	if err := s.App.Store.DB.QueryRow("SELECT count(*) FROM proxy_usage_totals WHERE model='gpt-test-result' AND response_count=1 AND total_tokens=150").Scan(&aggregateRows); err != nil || aggregateRows != 2 {
		t.Fatalf("aggregate rows=%d err=%v", aggregateRows, err)
	}
	if err := s.App.Store.DB.QueryRow("SELECT count(*) FROM operations WHERE kind='usage.refresh'").Scan(&refreshes); err != nil || refreshes != 0 {
		t.Fatalf("response triggered usage refreshes=%d err=%v", refreshes, err)
	}
	usage, err := s.proxyUsage(context.Background())
	if err != nil || len(usage) != 2 || usage[0].Model != "all" || usage[1].Model != "gpt-test-result" || usage[1].Lifetime.TotalTokens != 150 {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
}

func TestProxyTelemetryMapsPublicAccountID(t *testing.T) {
	s, key := proxyFixture(t, 1)
	if _, err := s.App.Store.DB.Exec("UPDATE accounts SET public_token='public-account' WHERE account_id='account0'"); err != nil {
		t.Fatal(err)
	}
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(proxyUsageStream("mapped", "completed", 1)))}, nil
	})
	if got := proxyRequest(s, key.Token, "/v1/responses", `{"input":[]}`); got.Code != 200 {
		t.Fatalf("response=%d %s", got.Code, got.Body.String())
	}
	var accountID string
	if err := s.App.Store.DB.QueryRow("SELECT account_id FROM proxy_responses WHERE proxy_request_id IS NOT NULL").Scan(&accountID); err != nil || accountID != "account0" {
		t.Fatalf("account=%q err=%v", accountID, err)
	}
}

func TestProxyGroupsResponsesByThreadAndTurn(t *testing.T) {
	s, key := proxyFixture(t, 1)
	calls := 0
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(proxyUsageStream(fmt.Sprintf("resp_%d", calls), "completed", calls)))}, nil
	})
	for _, value := range []struct{ thread, turn string }{{"thread-a", "turn-1"}, {"thread-a", "turn-1"}, {"thread-a", "turn-2"}, {"thread-b", "turn-1"}} {
		if got := proxyCorrelatedRequest(s, key.Token, value.thread, value.turn, `{"input":[]}`); got.Code != http.StatusOK {
			t.Fatalf("response=%d %s", got.Code, got.Body.String())
		}
	}
	var responses, turns, tokens int
	if err := s.App.Store.DB.QueryRow("SELECT count(*),count(DISTINCT turn_id),sum(total_tokens) FROM proxy_responses WHERE thread_id='thread-a'").Scan(&responses, &turns, &tokens); err != nil {
		t.Fatal(err)
	}
	if responses != 3 || turns != 2 || tokens != 6 {
		t.Fatalf("thread-a responses=%d turns=%d tokens=%d", responses, turns, tokens)
	}
}

func TestProxyUsageHandlesMissingCorrelationUsageAndFailover(t *testing.T) {
	s, key := proxyFixture(t, 2)
	calls := 0
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("quota"))}, nil
		}
		stream := strings.Replace(proxyStatusStream, "response.completed", "response.failed", 1)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	})
	if got := proxyRequest(s, key.Token, "/v1/responses", `{"input":[]}`); got.Code != http.StatusOK {
		t.Fatalf("response=%d %s", got.Code, got.Body.String())
	}
	var accountID, status string
	var threadID, turnID, usage sql.NullString
	var attempts int
	if err := s.App.Store.DB.QueryRow("SELECT account_id,thread_id,turn_id,terminal_status,upstream_attempts,usage_json FROM proxy_responses").Scan(&accountID, &threadID, &turnID, &status, &attempts, &usage); err != nil {
		t.Fatal(err)
	}
	if accountID != "account1" || threadID.Valid || turnID.Valid || status != "failed" || attempts != 2 || usage.Valid {
		t.Fatalf("account=%q thread=%#v turn=%#v status=%q attempts=%d usage=%#v", accountID, threadID, turnID, status, attempts, usage)
	}
}

func TestProxyRecognizesQuotaInSuccessfulFailedStream(t *testing.T) {
	for _, test := range []struct {
		name, code string
		want       int
	}{{"quota", "usage_limit_reached", 1}, {"server", "server_error", 0}} {
		t.Run(test.name, func(t *testing.T) {
			s, key := proxyFixture(t, 1)
			terminal := fmt.Sprintf(`data: {"type":"response.failed","response":{"error":{"code":%q},"output":[]}}`+"\n\n", test.code)
			stream := strings.Replace(proxyStatusStream, `data: {"type":"response.completed","response":{"output":[]}}`+"\n\n", terminal, 1)
			s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
			})
			if got := proxyRequest(s, key.Token, "/v1/responses", `{"input":[]}`); got.Code != 200 {
				t.Fatalf("response=%d %s", got.Code, got.Body.String())
			}
			var blocks int
			if err := s.App.Store.DB.QueryRow("SELECT count(*) FROM account_quota_blocks").Scan(&blocks); err != nil || blocks != test.want {
				t.Fatalf("blocks=%d want=%d err=%v", blocks, test.want, err)
			}
		})
	}
}

func TestProxyQuotaErrorsOverrideCachedCapacityForEveryClient(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"http429", 429, `{"error":{"code":"usage_limit_reached"}}`},
		{"message400", 400, `{"error":{"message":"You're out of usage"}}`},
		{"message403", 403, `{"error":{"message":"Usage maxed out"}}`},
		{"detail403", 403, `{"detail":"You've hit your usage limit"}`},
		{"streamFailed", 200, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"You're out of usage\"}}}\n\n"},
		{"streamError", 200, "data: {\"type\":\"error\",\"error\":{\"message\":\"Usage maxed out\"}}\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, key := proxyFixture(t, 2)
			now := time.Now()
			_, err := s.App.Store.DB.Exec("UPDATE usage_current SET short_used_percent_raw=80,weekly_used_percent_raw=40,short_resets_at_s=?,weekly_resets_at_s=?", now.Add(2*time.Hour).Unix(), now.Add(3*24*time.Hour).Unix())
			if err != nil {
				t.Fatal(err)
			}
			failedCalls := 0
			s.proxyClient.Transport = proxyRoundTrip(func(r *http.Request) (*http.Response, error) {
				status, body := 200, proxyUsageStream("ok", "completed", 150)
				if r.Header.Get("ChatGPT-Account-ID") == "account0" {
					failedCalls++
					status, body = test.status, test.body
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			if got := proxyRequest(s, key.Token, "/v1/responses", `{"input":[]}`); got.Code != 200 {
				t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
			}
			other, err := s.App.ClientKeys.Create(context.Background(), "other")
			if err != nil {
				t.Fatal(err)
			}
			for _, token := range []string{key.Token, other.Token} {
				if got := proxyRequest(s, token, "/v1/responses", `{"input":[]}`); got.Code != 200 {
					t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
				}
			}
			if failedCalls != 1 {
				t.Fatalf("quota-rejected account retried %d times", failedCalls)
			}
			var reads, blocks int
			if err := s.App.Store.DB.QueryRow("SELECT count(*) FROM operations WHERE kind='usage.refresh'").Scan(&reads); err != nil || reads != 0 {
				t.Fatalf("reads=%d err=%v", reads, err)
			}
			if err := s.App.Store.DB.QueryRow("SELECT count(*) FROM account_quota_blocks WHERE account_id='account0'").Scan(&blocks); err != nil || blocks != 1 {
				t.Fatalf("blocks=%d err=%v", blocks, err)
			}
		})
	}
}

func TestProxyRejectsLongCorrelationIdentifiers(t *testing.T) {
	s, key := proxyFixture(t, 1)
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected upstream request")
		return nil, nil
	})
	got := proxyCorrelatedRequest(s, key.Token, strings.Repeat("x", 201), "turn", `{"input":[]}`)
	if got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "invalid_correlation_id") {
		t.Fatalf("response=%d %s", got.Code, got.Body.String())
	}
}

func TestProxyResponsesStatusShowsRetryAndFailover(t *testing.T) {
	for _, test := range []struct {
		status, accounts int
		want             string
	}{{http.StatusUnauthorized, 1, "↻ retry"}, {http.StatusTooManyRequests, 2, "↻ failover"}} {
		t.Run(test.want, func(t *testing.T) {
			s, key := proxyFixture(t, test.accounts)
			calls := 0
			s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return &http.Response{StatusCode: test.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("failure"))}, nil
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(proxyStatusStream))}, nil
			})
			got := proxyRequest(s, key.Token, "/v1/responses", `{"model":"test","input":[]}`)
			if got.Code != 200 || calls != 2 || !strings.Contains(got.Body.String(), " · "+test.want+"\\n\\n") {
				t.Fatalf("response=%d calls=%d body=%s", got.Code, calls, got.Body.String())
			}
		})
	}
}

func TestProxyResponsesWebSocketFallbackRequiresAuth(t *testing.T) {
	s, key := proxyFixture(t, 1)
	if got := proxyMethodRequest(s, "", http.MethodGet, "/v1/responses", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", got.Code)
	}
	if got := proxyMethodRequest(s, key.Token, http.MethodGet, "/v1/responses", ""); got.Code != http.StatusUpgradeRequired {
		t.Fatalf("fallback status=%d", got.Code)
	}
}

func TestProxyOpaqueBodiesAndCredentials(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		t.Run(path, func(t *testing.T) {
			s, key := proxyFixture(t, 1)
			body := ` {"input":[],"store":false,"tools":[{"type":"function"}],"reasoning":{"effort":"high"},"text":{"format":{"type":"json_schema"}}} `
			output := `{"output":[{"content":[{"text":"unchanged"}]}]}`
			calls := 0
			s.proxyClient.Transport = proxyRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				got, _ := io.ReadAll(r.Body)
				if string(got) != body || r.URL.String() != responsesproxy.UpstreamURL+strings.TrimPrefix(path, "/v1")+"?test=1" {
					t.Errorf("body or target changed")
				}
				if r.Header.Get("Authorization") == "Bearer "+key.Token || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer header.") || r.Header.Get("ChatGPT-Account-ID") != "account0" {
					t.Error("wrong upstream credentials")
				}
				for _, name := range []string{"Cookie", "X-Api-Key", "Proxy-Authorization", "X-Remove"} {
					if r.Header.Get(name) != "" {
						t.Errorf("forwarded %s", name)
					}
				}
				if r.GetBody != nil {
					t.Error("transport may replay inference")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"secret"}, "Authorization": {"secret"}, "Chatgpt-Account-Id": {"secret"}}, Body: io.NopCloser(strings.NewReader(output))}, nil
			})
			r := httptest.NewRequest("POST", path+"?test=1", strings.NewReader(body))
			r.Header = http.Header{"Authorization": {"Bearer " + key.Token}, "Cookie": {"secret"}, "X-Api-Key": {"secret"}, "Proxy-Authorization": {"secret"}, "Connection": {"X-Remove"}, "X-Remove": {"secret"}}
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, r)
			if w.Code != 200 || w.Body.String() != output || calls != 1 {
				t.Fatalf("response %d %q calls=%d", w.Code, w.Body.String(), calls)
			}
			for _, name := range []string{"Set-Cookie", "Authorization", "ChatGPT-Account-ID"} {
				if w.Header().Get(name) != "" {
					t.Errorf("leaked %s", name)
				}
			}
		})
	}
}

func TestProxyAuthenticationAndBodyLimit(t *testing.T) {
	s, key := proxyFixture(t, 1)
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected upstream call"); return nil, nil })
	for _, token := range []string{"", "cbk_invalid"} {
		if got := proxyRequest(s, token, "/v1/responses", "{}"); got.Code != 401 {
			t.Fatalf("status=%d", got.Code)
		}
	}
	if got := proxyRequest(s, key.Token, "/v1/responses", strings.Repeat("x", responsesproxy.MaxRequest+1)); got.Code != 413 {
		t.Fatalf("limit status=%d", got.Code)
	}
	if _, err := s.App.ClientKeys.Revoke(context.Background(), key.ID); err != nil {
		t.Fatal(err)
	}
	if got := proxyRequest(s, key.Token, "/v1/responses", "{}"); got.Code != 401 {
		t.Fatalf("revoked status=%d", got.Code)
	}
}

func TestProxyFailover(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			s, key := proxyFixture(t, 2)
			calls := 0
			s.proxyClient.Transport = proxyRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				code := 200
				body := "success"
				if calls == 1 {
					code = status
					body = "private failure"
				} else if status == 429 && r.Header.Get("ChatGPT-Account-ID") != "account1" {
					t.Error("quota did not fail over")
				} else if status != 429 && r.Header.Get("ChatGPT-Account-ID") != "refreshed" {
					t.Error("auth did not refresh")
				}
				return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			got := proxyRequest(s, key.Token, "/v1/responses", "{}")
			if got.Code != 200 || got.Body.String() != "success" || calls != 2 {
				t.Fatalf("response %d %q calls=%d", got.Code, got.Body.String(), calls)
			}
			want := uint64(0)
			if status == 429 {
				want = 1
			}
			if s.proxyStats.snapshot().Failovers != want {
				t.Error("wrong failover counter")
			}
		})
	}
}

func TestProxyExhaustionAndUnknownReset(t *testing.T) {
	s, key := proxyFixture(t, 1)
	reset := time.Now().Add(time.Hour).Unix()
	if _, err := s.App.Store.DB.Exec("UPDATE usage_current SET short_used_percent_raw=100,short_resets_at_s=?", reset); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got := proxyRequest(s, key.Token, "/v1/responses", "{}")
	retry, _ := strconv.Atoi(got.Header().Get("Retry-After"))
	if got.Code != 429 || retry < 3601 || retry > 3603 || !strings.Contains(got.Body.String(), `"code":"pool_exhausted"`) || time.Since(start) > time.Second {
		t.Fatalf("exhaustion %d retry=%d body=%s", got.Code, retry, got.Body.String())
	}
	if _, err := s.App.Store.DB.Exec("UPDATE usage_current SET short_resets_at_s=NULL"); err != nil {
		t.Fatal(err)
	}
	if got := proxyRequest(s, key.Token, "/v1/responses", "{}"); got.Code != 503 {
		t.Fatalf("unknown reset status=%d", got.Code)
	}
}

func TestProxyNoRetryForOtherFailures(t *testing.T) {
	for _, status := range []int{400, 500, 503, 307, 0} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			s, key := proxyFixture(t, 1)
			calls := 0
			s.proxyClient.Transport = proxyRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if status == 0 {
					return nil, errors.New("secret transport error")
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("opaque upstream error"))}, nil
			})
			got := proxyRequest(s, key.Token, "/v1/responses", "{}")
			want := status
			if status == 0 || status == 307 {
				want = 502
			}
			if got.Code != want || calls != 1 || strings.Contains(got.Body.String(), "secret transport") {
				t.Fatalf("response %d calls=%d", got.Code, calls)
			}
		})
	}
}

func TestProxyStreamingAndCancellation(t *testing.T) {
	s, key := proxyFixture(t, 1)
	upstreamStarted, cancelled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: opaque\n\n")
		w.(http.Flusher).Flush()
		close(upstreamStarted)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	s.proxyClient.Transport = proxyRoundTrip(func(r *http.Request) (*http.Response, error) {
		r.URL.Scheme = "http"
		r.URL.Host = strings.TrimPrefix(upstream.URL, "http://")
		return transport.RoundTrip(r)
	})
	brokerServer := httptest.NewServer(s.middleware(s.mux))
	defer brokerServer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", brokerServer.URL+"/v1/responses", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+key.Token)
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	<-upstreamStarted
	value := make([]byte, len("data: opaque\n\n"))
	if _, err := io.ReadFull(response.Body, value); err != nil || string(value) != "data: opaque\n\n" {
		t.Fatalf("stream=%q err=%v", value, err)
	}
	snapshot := s.proxyStats.snapshot()
	if snapshot.Active != 1 || snapshot.RecentClients[0].Active != 1 {
		t.Fatalf("active snapshot=%+v", snapshot)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not cancelled")
	}
}

type brokenProxyBody struct{ sent bool }

func (b *brokenProxyBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "partial"), nil
	}
	return 0, io.ErrUnexpectedEOF
}
func (*brokenProxyBody) Close() error { return nil }
func TestProxyAbortsBrokenStreamWithoutReplay(t *testing.T) {
	s, key := proxyFixture(t, 1)
	calls := 0
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: &brokenProxyBody{}}, nil
	})
	got := proxyRequest(s, key.Token, "/v1/responses", "{}")
	if got.Code != http.StatusBadGateway || calls != 1 || s.proxyStats.snapshot().Active != 0 {
		t.Fatalf("response=%d calls=%d active=%d", got.Code, calls, s.proxyStats.snapshot().Active)
	}
}

func TestProxyTelemetryIdentityAndAdminSession(t *testing.T) {
	s, key := proxyFixture(t, 0)
	other, err := s.App.ClientKeys.Create(context.Background(), key.Name)
	if err != nil {
		t.Fatal(err)
	}
	done := s.proxyStats.begin(auth.ClientKey{ID: key.ID, Name: key.Name, Prefix: key.Prefix})
	end := s.proxyStats.begin(auth.ClientKey{ID: other.ID, Name: other.Name, Prefix: other.Prefix})
	end()
	snapshot := s.proxyStats.snapshot()
	if snapshot.Active != 1 || snapshot.Requests != 2 || len(snapshot.RecentClients) != 2 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	done()
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/internal/v1/proxy", nil))
	if w.Code != 401 {
		t.Fatalf("admin status=%d", w.Code)
	}
	now := time.Now().UnixMilli()
	token := "test-session"
	_, err = s.App.Store.DB.Exec("INSERT INTO admin_sessions VALUES(?,?,?,?,?,?,?,?,?)", auth.Digest(token), auth.Digest("csrf"), now, now, now+3600000, now+3600000, nil, nil, auth.Digest("test"))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/internal/v1/proxy", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), key.Prefix) || strings.Contains(w.Body.String(), key.Token) {
		t.Fatalf("snapshot response %d %s", w.Code, w.Body.String())
	}
}

func TestProxyStatusIncludesLiveRelayHealth(t *testing.T) {
	s, _ := proxyFixture(t, 0)
	now := time.Now()
	for _, relay := range []struct {
		id       string
		ready    int
		lastSeen time.Time
	}{{"ready-edge", 1, now}, {"connected-edge", 0, now}, {"offline-edge", 1, now.Add(-time.Hour)}} {
		if _, err := s.App.Store.DB.Exec("INSERT INTO relay_heartbeats VALUES(?,?,?,?,?,?)", relay.id, relayprotocol.Version, "snapshot", now.UnixMilli(), relay.ready, relay.lastSeen.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	token := "relay-status-session"
	_, err := s.App.Store.DB.Exec("INSERT INTO admin_sessions VALUES(?,?,?,?,?,?,?,?,?)", auth.Digest(token), auth.Digest("csrf"), now.UnixMilli(), now.UnixMilli(), now.Add(time.Hour).UnixMilli(), now.Add(time.Hour).UnixMilli(), nil, nil, auth.Digest("test"))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/internal/v1/proxy", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	response := httptest.NewRecorder()
	s.mux.ServeHTTP(response, request)
	var snapshot proxySnapshot
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	states := map[string]relayStatus{}
	for _, relay := range snapshot.Relays {
		states[relay.ID] = relay
	}
	if len(states) != 3 || states["ready-edge"].State != "ready" || !states["ready-edge"].Ready || states["connected-edge"].State != "connected" || !states["connected-edge"].Connected || states["offline-edge"].State != "offline" || states["offline-edge"].Connected || states["offline-edge"].Ready {
		t.Fatalf("relays=%+v", snapshot.Relays)
	}
}

func TestProxyAuthRefreshFailureSelectsAnotherAccount(t *testing.T) {
	s, key := proxyFixture(t, 2)
	settings := s.App.Config
	settings.CodexExecutable = "/bin/false"
	s.App.Service.Runtime = codex.NewRuntimeManager(settings, s.App.Service.Vault)
	calls := 0
	s.proxyClient.Transport = proxyRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		status := 200
		if calls == 1 {
			status = 403
		} else if r.Header.Get("ChatGPT-Account-ID") != "account1" {
			t.Error("auth failure did not select another account")
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("result"))}, nil
	})
	got := proxyRequest(s, key.Token, "/v1/responses", "{}")
	if got.Code != 200 || calls != 2 || s.proxyStats.snapshot().Failovers != 1 {
		t.Fatalf("status=%d calls=%d", got.Code, calls)
	}
}

func TestProxyRepeatedFailuresAreBounded(t *testing.T) {
	for _, status := range []int{401, 429} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			s, key := proxyFixture(t, 1)
			calls := 0
			s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("failure"))}, nil
			})
			got := proxyRequest(s, key.Token, "/v1/responses", "{}")
			wantCalls := 1
			if status == 401 {
				wantCalls = 2
			}
			if got.Code != status || calls != wantCalls {
				t.Fatalf("status=%d calls=%d", got.Code, calls)
			}
			if status == 429 && got.Header().Get("Retry-After") == "" {
				t.Error("missing pool retry time")
			}
		})
	}
}

func TestProxyDoesNotLogInferenceOrCredentials(t *testing.T) {
	s, key := proxyFixture(t, 1)
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("private-response"))}, nil
	})
	proxyRequest(s, key.Token, "/v1/responses", "private-prompt")
	var logs strings.Builder
	if err := s.App.Logs.Export("", "", &logs); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{key.Token, "private-prompt", "private-response", "secret-refresh", "header."} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs leaked %q", secret)
		}
	}
}

func TestProxyRootPath(t *testing.T) {
	s, key := proxyFixture(t, 1)
	s.App.Config.RootPath = "/broker"
	s.proxyClient.Transport = proxyRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != responsesproxy.UpstreamURL+"/responses/compact" {
			t.Fatalf("upstream=%s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("opaque"))}, nil
	})
	r := httptest.NewRequest("POST", "/broker/v1/responses/compact", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+key.Token)
	w := httptest.NewRecorder()
	s.rootPath(s.middleware(s.mux)).ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "opaque" {
		t.Fatalf("response=%d %q", w.Code, w.Body.String())
	}
}

func TestProxyRecentClientsIncludeLongStreams(t *testing.T) {
	stats := proxyStats{}
	done := stats.begin(auth.ClientKey{ID: "active", Name: "Active"})
	end := stats.begin(auth.ClientKey{ID: "idle", Name: "Idle"})
	end()
	stats.clients["active"].LastSeen = time.Now().Add(-10 * time.Minute)
	stats.clients["idle"].LastSeen = time.Now().Add(-10 * time.Minute)
	snapshot := stats.snapshot()
	if len(snapshot.RecentClients) != 1 || snapshot.RecentClients[0].Name != "Active" || snapshot.Requests != 2 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	done()
	if len(stats.snapshot().RecentClients) != 0 {
		t.Fatal("stale idle clients retained in snapshot")
	}
}

func TestProxyHeldAccountFallback(t *testing.T) {
	for _, scenario := range []string{"active", "short depleted", "weekly depleted", "excluded", "releasing", "all depleted"} {
		t.Run(scenario, func(t *testing.T) {
			s, key := proxyFixture(t, 2)
			s.App.Service.Config.WindowPulseEnabled = true
			now := time.Now().UnixMilli()
			_, err := s.App.Store.DB.Exec(`INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,cycle_state,weekly_started_at_ms) VALUES('account0',0,'IN_CYCLE',?),('account1',0,'HELD',NULL)`, now)
			if err != nil {
				t.Fatal(err)
			}
			statement := map[string]string{
				"short depleted":  `UPDATE usage_current SET short_used_percent_raw=100 WHERE account_id='account0'`,
				"weekly depleted": `UPDATE usage_current SET weekly_used_percent_raw=100 WHERE account_id='account0'`,
				"excluded":        `INSERT INTO account_exclusions VALUES(?,'account0','quota',?,?)`,
				"releasing":       `UPDATE window_pulse_state SET cycle_state='RELEASING' WHERE account_id='account0'; INSERT INTO operations(operation_id,account_id,kind,trigger,state,created_at_ms,state_version) VALUES('pulse','account0','window.pulse','USER','QUEUED',1,1)`,
				"all depleted":    `UPDATE usage_current SET weekly_used_percent_raw=100`,
			}[scenario]
			if statement != "" {
				var args []any
				if scenario == "excluded" {
					args = []any{key.ID, now + 60000, now}
				}
				if _, err := s.App.Store.DB.Exec(statement, args...); err != nil {
					t.Fatal(err)
				}
			}
			selected := ""
			s.proxyClient.Transport = proxyRoundTrip(func(r *http.Request) (*http.Response, error) {
				selected = r.Header.Get("ChatGPT-Account-ID")
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"output":[]}`))}, nil
			})
			w := proxyRequest(s, key.Token, "/v1/responses", `{"input":[]}`)
			if scenario == "all depleted" {
				if w.Code != 503 || selected != "" {
					t.Fatalf("depleted pool: status=%d selected=%s", w.Code, selected)
				}
				return
			}
			want := "account1"
			if scenario == "active" {
				want = "account0"
			}
			if w.Code != 200 || selected != want {
				t.Fatalf("status=%d selected=%s want=%s body=%s", w.Code, selected, want, w.Body.String())
			}
			var state string
			if err := s.App.Store.DB.QueryRow("SELECT cycle_state FROM window_pulse_state WHERE account_id='account1'").Scan(&state); err != nil {
				t.Fatal(err)
			}
			wantState := "IN_CYCLE"
			if scenario == "active" {
				wantState = "HELD"
			}
			if state != wantState {
				t.Fatalf("fallback state=%s want=%s", state, wantState)
			}
		})
	}
}

func TestManualPulseStartsRotation(t *testing.T) {
	s, _ := proxyFixture(t, 1)
	s.App.Service.Config.WindowPulseEnabled = true
	now := time.Now().UnixMilli()
	_, err := s.App.Store.DB.Exec("INSERT INTO admin_sessions VALUES(?,?,?,?,?,?,?,?,?)", auth.Digest("session"), auth.Digest("csrf"), now, now, now+3600000, now+3600000, nil, nil, auth.Digest("test"))
	if err != nil {
		t.Fatal(err)
	}
	// An account need not have a pulse state yet; manual release creates it.
	request := func(session, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/accounts/account0/pulse", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		r.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		return w
	}
	for _, credentials := range [][2]string{{"", ""}, {"session", "wrong"}} {
		if w := request(credentials[0], credentials[1]); w.Code != 401 && w.Code != 403 {
			t.Fatalf("unprotected pulse: %d %s", w.Code, w.Body.String())
		}
	}
	w := request("session", "csrf")
	if w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/operations/") {
		t.Fatalf("pulse response: %d %s", w.Code, w.Body.String())
	}
	operation := strings.TrimPrefix(w.Header().Get("Location"), "/operations/")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var state, trigger string
		if err := s.App.Store.DB.QueryRow("SELECT state,trigger FROM operations WHERE operation_id=?", operation).Scan(&state, &trigger); err != nil {
			t.Fatal(err)
		}
		if state == "FAILED" {
			value, _ := s.App.Service.Operation(context.Background(), operation)
			t.Fatalf("pulse failed: %#v", value)
		}
		if state == "SUCCEEDED" {
			var cycle string
			var started, next, released int64
			if err := s.App.Store.DB.QueryRow("SELECT cycle_state,weekly_started_at_ms,next_pulse_at_ms,cadence_anchor_at_ms FROM window_pulse_state CROSS JOIN weekly_cycle_state WHERE account_id='account0'").Scan(&cycle, &started, &next, &released); err != nil {
				t.Fatal(err)
			}
			if cycle != "IN_CYCLE" || trigger != "USER" || started < now-1000 || next <= now || released != started {
				t.Fatalf("cycle=%s trigger=%s start=%d next=%d released=%d", cycle, trigger, started, next, released)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pulse did not finish")
}

func TestManualPulseEligibilityAndDeduplication(t *testing.T) {
	for _, scenario := range []string{"worker busy", "quarantined", "missing credential", "depleted", "operation busy", "already pulsing"} {
		t.Run(scenario, func(t *testing.T) {
			s, _ := proxyFixture(t, 1)
			statement := map[string]string{
				"quarantined":        `UPDATE account_state SET auth_state='AUTH_REQUIRED',worker_state='CREDENTIAL_QUARANTINED'`,
				"missing credential": `DELETE FROM credential_bundles`,
				"worker busy":        `UPDATE account_state SET worker_state='CREDENTIAL_IN_USE'`,
				"depleted":           `UPDATE usage_current SET short_used_percent_raw=100`,
				"operation busy":     `INSERT INTO operations(operation_id,account_id,kind,trigger,state,created_at_ms,state_version) VALUES('existing','account0','usage.refresh','USER','QUEUED',1,1)`,
				"already pulsing":    `INSERT INTO operations(operation_id,account_id,kind,trigger,state,created_at_ms,state_version) VALUES('existing','account0','window.pulse','USER','RUNNING',1,1)`,
			}[scenario]
			if _, err := s.App.Store.DB.Exec(statement); err != nil {
				t.Fatal(err)
			}
			operation, err := s.App.Service.Pulse(context.Background(), "account0")
			if scenario == "already pulsing" {
				if err != nil || operation != "existing" {
					t.Fatalf("duplicate pulse: operation=%s err=%v", operation, err)
				}
			} else if err == nil || operation != "" {
				t.Fatalf("ineligible pulse: operation=%s err=%v", operation, err)
			}
		})
	}
}

// Exercise the HTTP readiness endpoint after the proxy receives a read timeout.
func TestProxyIdleTimeoutReadiness(t *testing.T) {
	s, key := proxyFixture(t, 1)
	s.App.Ready = true
	ready := func() int {
		w := httptest.NewRecorder()
		if err := s.ready(w, httptest.NewRequest("GET", "/health/ready", nil)); err != nil {
			t.Fatal(err)
		}
		return w.Code
	}
	if ready() != 200 {
		t.Fatal("initially unready")
	}
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(proxyIdleReader{})}, nil
	})
	got := proxyRequest(s, key.Token, "/v1/responses", `{"input":[]}`)
	if got.Code != 504 || ready() != 503 {
		t.Fatalf("response=%d readiness=%d", got.Code, ready())
	}
	logs := s.App.Logs.Recent("ERROR", "proxy.failure", 10)
	if len(logs) != 1 || !strings.Contains(logs[0]["message"].(string), "stage=upstream_body") {
		t.Fatalf("logs=%v", logs)
	}
}

type proxyIdleReader struct{}

func (proxyIdleReader) Read([]byte) (int, error) { return 0, responsesproxy.ErrIdleReadTimeout }
