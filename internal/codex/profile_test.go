package codex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchProfileAuthenticatesAndDecodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("ChatGPT-Account-Id") != "account" {
			t.Fatalf("unexpected auth headers: %#v", r.Header)
		}
		_, _ = w.Write([]byte(`{"profile":{"display_name":"Test"},"stats":{"lifetime_tokens":42,"fast_mode_usage_percentage":12.5,"daily_usage_buckets":[{"start_date":"2026-09-19","tokens":7}]}}`))
	}))
	defer server.Close()

	profile, err := FetchProfile(context.Background(), server.URL, "token", "account")
	if err != nil {
		t.Fatal(err)
	}
	if profile.Stats.LifetimeTokens == nil || *profile.Stats.LifetimeTokens != 42 || len(profile.Stats.DailyUsageBuckets) != 1 {
		t.Fatalf("profile = %#v", profile)
	}
}

func TestFetchProfileRejectsRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, "https://example.com", http.StatusFound)
	}))
	defer server.Close()
	if _, err := FetchProfile(context.Background(), server.URL, "token", "account"); err == nil {
		t.Fatal("redirect was accepted")
	}
}
