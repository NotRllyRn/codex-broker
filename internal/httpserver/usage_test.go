package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/identity"
	"github.com/NotRllyRn/codex-broker/internal/usage"
)

func TestUsageEndpointRequiresAdminAndValidatesPeriod(t *testing.T) {
	s, key := proxyFixture(t, 1)
	for _, token := range []string{"", key.Token} {
		r := httptest.NewRequest(http.MethodGet, "/api/internal/v1/usage", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.middleware(s.mux).ServeHTTP(w, r)
		if w.Code != 401 || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
			t.Fatalf("unauthorized usage: %d %s", w.Code, w.Body.String())
		}
	}
	now := time.Now()
	_, err := s.App.Store.DB.Exec("INSERT INTO admin_sessions VALUES(?,?,?,?,?,?,?,?,?)", auth.Digest("usage-test"), auth.Digest("csrf"), now.UnixMilli(), now.UnixMilli(), now.Add(time.Hour).UnixMilli(), now.Add(time.Hour).UnixMilli(), nil, nil, auth.Digest("test"))
	if err != nil {
		t.Fatal(err)
	}
	owner, code, err := identity.Create(t.Context(), s.App.Store, "Usage test")
	if err != nil {
		t.Fatal(err)
	}
	token := "cbu_" + core.RandomToken(32)
	device, err := identity.Enroll(t.Context(), s.App.Store, code, "0123456789abcdef0123456789abcdef", "Laptop", token)
	if err != nil {
		t.Fatal(err)
	}
	_, err = identity.Ingest(t.Context(), s.App.Store, token, []identity.Event{{ID: identity.EventID("old"), Source: "codex", Model: "gpt-test", TimestampMS: now.AddDate(0, 0, -100).UnixMilli(), OutputTokens: 42, UsageKnown: true}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.App.Store.DB.Exec(`INSERT INTO usage_rate_cache VALUES(1,?,?)`, now.UnixMilli(), `{"gpt-test":{"input_cost_per_token":0.000002,"output_cost_per_token":0.00001}}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.App.Store.DB.Exec(`INSERT INTO usage_daily VALUES(?,'account0','gpt-test','default',1,1,100,0,25)`, now.UTC().Format(time.DateOnly))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		query     string
		status    int
		responses int64
	}{{"?days=7&account=account0", 200, 1}, {"?days=30&account=missing", 200, 0}, {"?days=0", 400, 0}, {"?days=500000", 400, 0}, {"?days=invalid", 400, 0},
		{"?days=all&identity=" + owner.ID + "&device=" + device.ID, 200, 1},
		{"?days=90&identity=" + owner.ID, 200, 0},
		{"?days=all&identity=" + owner.ID + "&device=proxy", 200, 0},
		{"?days=all&identity=all", 200, 1},
		{"?days=all&identity=missing", 200, 0}} {
		r := httptest.NewRequest(http.MethodGet, "/api/internal/v1/usage"+test.query, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "usage-test"})
		w := httptest.NewRecorder()
		s.middleware(s.mux).ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("%s: %d %s", test.query, w.Code, w.Body.String())
		}
		if test.status == 200 {
			var summary usage.Summary
			if err := json.Unmarshal(w.Body.Bytes(), &summary); err != nil {
				t.Fatal(err)
			}
			if summary.Responses != test.responses || summary.Pricing.Status != "fresh" {
				t.Fatalf("%s: %+v", test.query, summary)
			}
			if strings.Contains(test.query, "days=all&identity=") && test.responses == 1 && (summary.Output != 42 || len(summary.Daily) != 101) {
				t.Fatalf("all-time history missing: %+v", summary)
			}
		}
	}
}
