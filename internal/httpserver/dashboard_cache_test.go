package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/codex"
)

func dashboardSession(t *testing.T, s *Server, token string) {
	t.Helper()
	now := time.Now()
	_, err := s.App.Store.DB.Exec("INSERT INTO admin_sessions VALUES(?,?,?,?,?,?,?,?,?)", auth.Digest(token), auth.Digest("csrf"), now.UnixMilli(), now.UnixMilli(), now.Add(time.Hour).UnixMilli(), now.Add(time.Hour).UnixMilli(), nil, nil, auth.Digest("test"))
	if err != nil {
		t.Fatal(err)
	}
}

func dashboardRequest(path, token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	return r
}

func TestResponseCacheRevalidationAndIsolation(t *testing.T) {
	s, _ := proxyFixture(t, 0)
	dashboardSession(t, s, "first")
	dashboardSession(t, s, "second")
	builds := 0
	handler := s.cached(time.Minute, func(w http.ResponseWriter, r *http.Request) error {
		builds++
		return writeJSON(w, 200, map[string]any{"build": builds})
	})
	request := func(r *http.Request) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		if err := handler(w, r); err != nil {
			t.Fatal(err)
		}
		return w
	}
	first := request(dashboardRequest("/view", "first"))
	r := dashboardRequest("/view", "first")
	r.Header.Set("If-None-Match", first.Header().Get("ETag"))
	if got := request(r); got.Code != 304 || builds != 1 || got.Body.Len() != 0 || got.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("cache hit: status=%d builds=%d", got.Code, builds)
	}
	r.Header.Set("X-Dashboard-Refresh", "true")
	if got := request(r); got.Code != 200 || builds != 2 {
		t.Fatal("forced refresh reused stale data")
	}
	request(dashboardRequest("/view", "second"))
	r = dashboardRequest("/view", "first")
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: "other"})
	request(r)
	r = dashboardRequest("/view", "first")
	r.AddCookie(&http.Cookie{Name: "codex-broker.streamer", Value: "on"})
	request(r)
	if builds != 5 {
		t.Fatalf("cache crossed session/CSRF/privacy boundary: %d builds", builds)
	}
	if _, err := s.App.Store.DB.Exec("UPDATE admin_sessions SET revoked_at_ms=1"); err != nil {
		t.Fatal(err)
	}
	if err := handler(httptest.NewRecorder(), dashboardRequest("/view", "first")); err == nil {
		t.Fatal("cached response bypassed revoked session")
	}
}

func TestResponseCacheExpiryErrorsAndBound(t *testing.T) {
	s, _ := proxyFixture(t, 0)
	dashboardSession(t, s, "first")
	builds := 0
	next := func(w http.ResponseWriter, r *http.Request) error { builds++; return writeJSON(w, 200, builds) }
	expired := s.cached(0, next)
	for range 2 {
		if err := expired(httptest.NewRecorder(), dashboardRequest("/view", "first")); err != nil {
			t.Fatal(err)
		}
	}
	if builds != 2 {
		t.Fatal("expired entry was reused")
	}
	bounded := s.cached(time.Minute, next)
	for i := range 65 {
		if err := bounded(httptest.NewRecorder(), dashboardRequest(fmt.Sprintf("/view?i=%d", i), "first")); err != nil {
			t.Fatal(err)
		}
	}
	if err := bounded(httptest.NewRecorder(), dashboardRequest("/view?i=0", "first")); err != nil {
		t.Fatal(err)
	}
	if builds != 68 {
		t.Fatal("cache did not evict entries at its limit")
	}
	errors := 0
	failing := s.cached(time.Minute, func(w http.ResponseWriter, r *http.Request) error { errors++; return fmt.Errorf("unavailable") })
	for range 2 {
		_ = failing(httptest.NewRecorder(), dashboardRequest("/view", "first"))
	}
	if errors != 2 {
		t.Fatal("error was cached")
	}
}

func TestDashboardSnapshotsAtScale(t *testing.T) {
	s, _ := proxyFixture(t, 64)
	dashboardSession(t, s, "first")
	var days []codex.DailyUsage
	for i := range 90 {
		days = append(days, codex.DailyUsage{StartDate: time.Now().AddDate(0, 0, -i).Format(time.DateOnly), Tokens: 1000})
	}
	raw, _ := json.Marshal(days)
	if _, err := s.App.Store.DB.Exec(`INSERT INTO account_profiles(account_id,username,lifetime_tokens,daily_usage_buckets_json) SELECT account_id,'example-user',90000,? FROM accounts`, string(raw)); err != nil {
		t.Fatal(err)
	}
	var err error
	s.renderer, err = newRenderer("templates")
	if err != nil {
		t.Fatal(err)
	}
	shell := httptest.NewRecorder()
	s.mux.ServeHTTP(shell, dashboardRequest("/", "first"))
	if shell.Code != 200 || !strings.Contains(shell.Body.String(), "data-cache-scope") || strings.Contains(shell.Body.String(), "orbit-account") {
		t.Fatal("initial page did not use lightweight shell")
	}
	path := "/api/internal/v1/dashboard/view?state_filter=&mode=off"
	start := time.Now()
	first := httptest.NewRecorder()
	s.mux.ServeHTTP(first, dashboardRequest(path, "first"))
	cold := time.Since(start)
	if first.Code != 200 || strings.Count(first.Body.String(), `class="orbit-account state-`) != 64 {
		t.Fatalf("snapshot=%d %s", first.Code, first.Body.String())
	}
	start = time.Now()
	for range 20 {
		r := dashboardRequest(path, "first")
		r.Header.Set("If-None-Match", first.Header().Get("ETag"))
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		if w.Code != 304 || w.Body.Len() != 0 {
			t.Fatal("unchanged snapshot was downloaded again")
		}
	}
	t.Logf("64 accounts: cold snapshot %s; cached revalidation average %s", cold, time.Since(start)/20)
	private := httptest.NewRecorder()
	// Explicit mode wins over a cookie changed by another tab.
	s.mux.ServeHTTP(private, dashboardRequest("/api/internal/v1/dashboard/view?state_filter=&mode=on", "first"))
	if private.Code != 200 || strings.Contains(private.Body.String(), ">account0<") || strings.Contains(private.Body.String(), `"label":"account0"`) || strings.Contains(private.Body.String(), "example-user") {
		t.Fatal("streamer snapshot exposed account labels")
	}
}

func TestResponseCacheCoalescesConcurrentBuilds(t *testing.T) {
	cache := &responseCache{}
	builds := 0
	var group sync.WaitGroup
	for range 10 {
		group.Go(func() {
			_, _ = cache.get("view", false, func() (cachedResponse, error) {
				builds++
				return cachedResponse{status: 200, expires: time.Now().Add(time.Minute)}, nil
			})
		})
	}
	group.Wait()
	if builds != 1 {
		t.Fatalf("concurrent requests caused %d builds", builds)
	}
}
