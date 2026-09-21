package loopback

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConfigurationRequiresLoopbackAndBrokerTLS(t *testing.T) {
	for _, address := range []string{"localhost:8789", "0.0.0.0:8789", "192.168.1.2:8789", "missing-port"} {
		if ValidateListen(address) == nil {
			t.Errorf("accepted listen address %q", address)
		}
	}
	if ValidateListen("127.0.0.1:8789") != nil || ValidateListen("[::1]:8789") != nil {
		t.Fatal("loopback address was rejected")
	}
	if _, err := New(Config{Listen: DefaultListen, BrokerURL: "http://broker.example"}); err == nil {
		t.Fatal("HTTP broker URL was accepted")
	}
}

func TestForwardsResponsesWithLeasedIdentity(t *testing.T) {
	broker, ca := testBroker(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cbk_test" {
			t.Errorf("broker authorization = %q", r.Header.Get("Authorization"))
		}
		var route routeRequest
		if json.NewDecoder(r.Body).Decode(&route) != nil || route.SessionID == "" || route.TurnID == "" {
			t.Error("invalid route request")
		}
		writeJSON(w, http.StatusOK, lease{Status: "ok", AccountID: "public-a", AccessToken: "access-a", ChatGPTAccountID: "upstream-a"})
	})
	defer broker.Close()

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/responses" || r.URL.RawQuery != "include=test" || string(body) != `{"model":"test"}` {
			t.Errorf("upstream request = %s?%s %s", r.URL.Path, r.URL.RawQuery, body)
		}
		if r.Header.Get("Authorization") != "Bearer access-a" || r.Header.Get("ChatGPT-Account-ID") != "upstream-a" {
			t.Errorf("upstream identity = %#v", r.Header)
		}
		if r.Header.Get("Originator") != "codex_cli_rs" {
			t.Errorf("originator = %q", r.Header.Get("Originator"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Add("Set-Cookie", "secret=value")
		_, _ = io.WriteString(w, "data: one\n\ndata: two\n\n")
	}))
	defer upstream.Close()

	adapter := newTestAdapter(t, broker.URL, ca, upstream)
	response := adapterRequest(t, adapter, `{"model":"test"}`, "include=test")
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(body) != "data: one\n\ndata: two\n\n" {
		t.Fatalf("response = %d %q", response.StatusCode, body)
	}
	if response.Header.Get("Set-Cookie") != "" {
		t.Fatal("upstream cookie was forwarded")
	}
}

func TestForwardsRemoteCompactionPath(t *testing.T) {
	broker, ca := testBroker(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, lease{Status: "ok", AccountID: "a", AccessToken: "access-a", ChatGPTAccountID: "upstream-a"})
	})
	defer broker.Close()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses/compact" {
			t.Errorf("compaction path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"output":[]}`)
	}))
	defer upstream.Close()
	server := httptest.NewServer(newTestAdapter(t, broker.URL, ca, upstream).Handler())
	defer server.Close()
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses/compact", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer cbk_test")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("compaction status = %d", response.StatusCode)
	}
}

func TestQuotaFailureSelectsAnotherAccount(t *testing.T) {
	var routes atomic.Int32
	broker, ca := testBroker(t, func(w http.ResponseWriter, r *http.Request) {
		var route routeRequest
		_ = json.NewDecoder(r.Body).Decode(&route)
		if routes.Add(1) == 1 {
			writeJSON(w, 200, lease{Status: "ok", AccountID: "a", AccessToken: "access-a", ChatGPTAccountID: "upstream-a"})
			return
		}
		if route.FailedAccountID != "a" || route.FailureKind != "quota" || route.PreferredAccountID != "a" {
			t.Errorf("replacement request = %#v", route)
		}
		writeJSON(w, 200, lease{Status: "ok", AccountID: "b", AccessToken: "access-b", ChatGPTAccountID: "upstream-b"})
	})
	defer broker.Close()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer access-a" {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "quota"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer access-b" {
			t.Errorf("replacement authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, "selected b")
	}))
	defer upstream.Close()

	response := adapterRequest(t, newTestAdapter(t, broker.URL, ca, upstream), `{}`, "")
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || string(body) != "selected b" || routes.Load() != 2 {
		t.Fatalf("response = %d %q, routes = %d", response.StatusCode, body, routes.Load())
	}
}

func TestAuthenticationRefreshRetriesSameAccountOnce(t *testing.T) {
	var routes atomic.Int32
	broker, ca := testBroker(t, func(w http.ResponseWriter, r *http.Request) {
		var route routeRequest
		_ = json.NewDecoder(r.Body).Decode(&route)
		count := routes.Add(1)
		if count == 2 && (route.FailedAccountID != "a" || route.FailureKind != "auth") {
			t.Errorf("refresh request = %#v", route)
		}
		token := "expired"
		if count > 1 {
			token = "refreshed"
		}
		writeJSON(w, 200, lease{Status: "ok", AccountID: "a", AccessToken: token, ChatGPTAccountID: "upstream-a"})
	})
	defer broker.Close()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer expired" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "expired"})
			return
		}
		_, _ = io.WriteString(w, "refreshed")
	}))
	defer upstream.Close()

	response := adapterRequest(t, newTestAdapter(t, broker.URL, ca, upstream), `{}`, "")
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || string(body) != "refreshed" || routes.Load() != 2 {
		t.Fatalf("response = %d %q, routes = %d", response.StatusCode, body, routes.Load())
	}
}

func TestBrokerFailureNeverReachesUpstream(t *testing.T) {
	broker, ca := testBroker(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "CLIENT_KEY_INVALID"})
	})
	defer broker.Close()
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer upstream.Close()

	response := adapterRequest(t, newTestAdapter(t, broker.URL, ca, upstream), `{}`, "")
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatalf("status = %d, upstream calls = %d", response.StatusCode, calls.Load())
	}
}

func TestPoolWaitStopsWhenRequestIsCancelled(t *testing.T) {
	broker, ca := testBroker(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusTooManyRequests, wait{Status: "wait", Code: "POOL_EXHAUSTED", RetryAfterSeconds: 60})
	})
	defer broker.Close()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()
	server := httptest.NewServer(newTestAdapter(t, broker.URL, ca, upstream).Handler())
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer cbk_test")
	done := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled pool wait did not stop")
	}
}

func testBroker(t *testing.T, route http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			writeJSON(w, 200, map[string]string{"status": "ok"})
			return
		}
		if r.URL.Path != "/api/v1/route" {
			t.Errorf("broker path = %q", r.URL.Path)
		}
		route(w, r)
	}))
	certificate := server.Certificate()
	path := t.TempDir() + "/broker-ca.pem"
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := x509.ParseCertificate(certificate.Raw); err != nil {
		t.Fatal(err)
	}
	return server, path
}

func newTestAdapter(t *testing.T, brokerURL, ca string, upstream *httptest.Server) *Adapter {
	t.Helper()
	adapter, err := New(Config{Listen: DefaultListen, BrokerURL: brokerURL, BrokerCA: ca, UpstreamURL: upstream.URL, UpstreamClient: upstream.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func adapterRequest(t *testing.T, adapter *Adapter, body, query string) *http.Response {
	t.Helper()
	server := httptest.NewServer(adapter.Handler())
	t.Cleanup(server.Close)
	target := server.URL + "/v1/responses"
	if query != "" {
		target += "?" + query
	}
	request, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer cbk_test")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
