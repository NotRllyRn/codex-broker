package httpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/responsesproxy"
)

func TestProxyScrubsOnceBeforeFailover(t *testing.T) {
	s, key := proxyFixture(t, 2)
	body := scrubRequestBody(t, true)
	s.proxyScrubPolicy = responsesproxy.ScrubPolicy{TriggerBytes: len(body) - 1, TargetBytes: len(body) - 500}
	var forwarded [][]byte
	s.proxyClient.Transport = proxyRoundTrip(func(r *http.Request) (*http.Response, error) {
		value, _ := io.ReadAll(r.Body)
		forwarded = append(forwarded, value)
		status := http.StatusOK
		if len(forwarded) == 1 {
			status = http.StatusTooManyRequests
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})

	got := proxyRequest(s, key.Token, "/v1/responses", string(body))
	snapshot := s.proxyStats.snapshot()
	if got.Code != http.StatusOK || len(forwarded) != 2 || !bytes.Equal(forwarded[0], forwarded[1]) {
		t.Fatalf("status=%d calls=%d", got.Code, len(forwarded))
	}
	if snapshot.ScrubbedRequests != 1 || snapshot.ScrubbedInputBytes != uint64(len(body)) || snapshot.ScrubbedOutputBytes >= snapshot.ScrubbedInputBytes || snapshot.ScrubbedRemovedBytes != snapshot.ScrubbedInputBytes-snapshot.ScrubbedOutputBytes {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestProxyRejectsOversizedProtectedTurnWithoutRouting(t *testing.T) {
	s, key := proxyFixture(t, 1)
	body := scrubRequestBody(t, false)
	s.proxyScrubPolicy = responsesproxy.ScrubPolicy{TriggerBytes: len(body) - 1, TargetBytes: len(body) - 500}
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected upstream request")
		return nil, nil
	})

	got := proxyRequest(s, key.Token, "/v1/responses", string(body))
	if got.Code != http.StatusRequestEntityTooLarge || !strings.Contains(got.Body.String(), "request_too_large_after_scrub") || s.proxyStats.snapshot().ScrubRejectedRequests != 1 {
		t.Fatalf("status=%d body=%s snapshot=%+v", got.Code, got.Body.String(), s.proxyStats.snapshot())
	}
}

func TestProxyRejectsMalformedScrubCandidate(t *testing.T) {
	s, key := proxyFixture(t, 1)
	s.proxyScrubPolicy = responsesproxy.ScrubPolicy{TriggerBytes: 2, TargetBytes: 1}
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected upstream request")
		return nil, nil
	})
	got := proxyRequest(s, key.Token, "/v1/responses", "not-json")
	if got.Code != http.StatusBadRequest || s.proxyStats.snapshot().ScrubRejectedRequests != 1 {
		t.Fatalf("status=%d snapshot=%+v", got.Code, s.proxyStats.snapshot())
	}
}

func TestProxyLargeRequestGateReleasesBeforeUpstream(t *testing.T) {
	s, key := proxyFixture(t, 1)
	body := scrubRequestBody(t, true)
	s.proxyScrubPolicy = responsesproxy.ScrubPolicy{TriggerBytes: len(body) - 1, TargetBytes: len(body) - 500}
	started, release := make(chan struct{}, 2), make(chan struct{})
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-release
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	results := make(chan int, 2)
	request := func() { results <- proxyRequest(s, key.Token, "/v1/responses", string(body)).Code }
	go request()
	<-started
	go request()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("large-request gate remained held during upstream inference")
	}
	close(release)
	for range 2 {
		if status := <-results; status != http.StatusOK {
			t.Fatalf("status=%d", status)
		}
	}
}

func scrubRequestBody(t *testing.T, historical bool) []byte {
	t.Helper()
	oldRole := "user"
	if !historical {
		oldRole = "assistant"
	}
	items := []any{
		map[string]any{"type": "message", "role": oldRole, "content": "old"},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + strings.Repeat("a", 1200)}}},
	}
	if historical {
		items[0], items[1] = items[1], map[string]any{"type": "message", "role": "user", "content": "current"}
	}
	body, err := json.Marshal(map[string]any{"model": "test", "store": false, "input": items})
	if err != nil {
		t.Fatal(err)
	}
	return body
}
