package httpserver

import (
	"context"
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
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/broker"
	"github.com/NotRllyRn/codex-broker/internal/codex"
	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/NotRllyRn/codex-broker/internal/events"
	"github.com/NotRllyRn/codex-broker/internal/logbook"
	"github.com/NotRllyRn/codex-broker/internal/responsesproxy"
	"github.com/NotRllyRn/codex-broker/internal/store"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

type proxyRoundTrip func(*http.Request) (*http.Response, error)

func (f proxyRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func proxyFixture(t *testing.T, accounts int) (*Server, auth.IssuedClientKey) {
	t.Helper()
	ctx, root := context.Background(), t.TempDir()
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
		if request["method"] == "account/read" {
			writeProxyAuth(t, home, "refreshed")
			result["account"] = map[string]string{"type": "chatgpt", "email": "test@example.com"}
		}
		_ = encoder.Encode(map[string]any{"id": id, "result": result})
	}
}

func proxyRequest(server *Server, key, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	server.middleware(server.mux).ServeHTTP(w, r)
	return w
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
	defer func() {
		if recover() != http.ErrAbortHandler || calls != 1 || s.proxyStats.snapshot().Active != 0 {
			t.Fatal("broken stream not aborted cleanly")
		}
	}()
	proxyRequest(s, key.Token, "/v1/responses", "{}")
	t.Fatal("expected aborted stream")
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
