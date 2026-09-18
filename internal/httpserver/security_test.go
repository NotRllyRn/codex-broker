package httpserver

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/broker"
	"github.com/NotRllyRn/codex-broker/internal/config"
)

func TestThrottleClearsSuccessfulIdentity(t *testing.T) {
	limiter := newThrottle(2, time.Minute)
	if !limiter.allow("client") || !limiter.allow("client") || limiter.allow("client") {
		t.Fatal("throttle did not enforce its limit")
	}
	limiter.clear("client")
	if !limiter.allow("client") {
		t.Fatal("throttle did not clear")
	}
}

func TestTrustedProxyControlsForwardedMetadata(t *testing.T) {
	server := &Server{App: &broker.Application{Config: config.Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}}}
	request := httptest.NewRequest(http.MethodGet, "http://broker/", nil)
	request.RemoteAddr = "10.1.2.3:1234"
	request.Header.Set("X-Forwarded-For", "203.0.113.9")
	request.Header.Set("X-Forwarded-Proto", "https")
	if server.clientAddress(request) != "203.0.113.9" || server.requestScheme(request) != "https" {
		t.Fatal("trusted forwarded metadata was ignored")
	}
	request.RemoteAddr = "192.0.2.5:1234"
	if server.clientAddress(request) != "192.0.2.5" || server.requestScheme(request) != "http" {
		t.Fatal("untrusted forwarded metadata was accepted")
	}
}

func TestRootPathStripsPrefixAndRejectsOtherPaths(t *testing.T) {
	server := &Server{App: &broker.Application{Config: config.Config{RootPath: "/broker"}}}
	handler := server.rootPath(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(r.URL.Path)) }))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://host/broker/health/live", nil))
	if response.Code != 200 || response.Body.String() != "/health/live" {
		t.Fatalf("rooted response = %d %q", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://host/health/live", nil))
	if response.Code != 404 {
		t.Fatalf("unrooted status = %d", response.Code)
	}
}

func TestDecodeJSONRejectsTrailingValues(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"value":"one"} {"value":"two"}`))
	var body struct {
		Value string `json:"value"`
	}
	if err := decodeJSON(request, &body); err == nil {
		t.Fatal("accepted a second JSON value")
	}
}
