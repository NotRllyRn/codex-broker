package httpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/responsesproxy"
)

func TestProxyScrubsOnceBeforeFailover(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		t.Run(path, func(t *testing.T) {
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

			got := proxyRequest(s, key.Token, path, string(body))
			snapshot := s.proxyStats.snapshot()
			if got.Code != http.StatusOK || len(forwarded) != 2 || !bytes.Equal(forwarded[0], forwarded[1]) {
				t.Fatalf("status=%d calls=%d", got.Code, len(forwarded))
			}
			if snapshot.ScrubbedRequests != 1 || snapshot.ScrubbedInputBytes != uint64(len(body)) || snapshot.ScrubbedOutputBytes >= snapshot.ScrubbedInputBytes || snapshot.ScrubbedRemovedBytes != snapshot.ScrubbedInputBytes-snapshot.ScrubbedOutputBytes {
				t.Fatalf("snapshot=%+v", snapshot)
			}
		})
	}
}

func TestProxyScrubArmsNativeCompaction(t *testing.T) {
	for _, test := range []struct {
		name, path        string
		compactionTrigger bool
		wantInput         int64
		wantArmed         uint64
	}{
		{name: "normal", path: "/v1/responses", wantInput: responsesproxy.NativeCompactionInputTokens, wantArmed: 1},
		{name: "remote v2 compaction", path: "/v1/responses", compactionTrigger: true, wantInput: 120},
		{name: "legacy compaction", path: "/v1/responses/compact", wantInput: 120},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, key := proxyFixture(t, 1)
			body := scrubRequestBody(t, true)
			if test.compactionTrigger {
				var request map[string]any
				if err := json.Unmarshal(body, &request); err != nil {
					t.Fatal(err)
				}
				request["input"] = append(request["input"].([]any), map[string]any{"type": "compaction_trigger"})
				body, _ = json.Marshal(request)
			}
			s.proxyScrubPolicy = responsesproxy.ScrubPolicy{TriggerBytes: len(body) - 1, TargetBytes: len(body) - 500}
			s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(proxyUsageStream("resp", "completed", 150)))}, nil
			})

			got := proxyRequest(s, key.Token, test.path, string(body))
			if got.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
			}
			usage := terminalEvent(t, got.Body.String())["response"].(map[string]any)["usage"].(map[string]any)
			if input := int64(usage["input_tokens"].(float64)); input != test.wantInput {
				t.Fatalf("input_tokens=%d want=%d", input, test.wantInput)
			}
			if snapshot := s.proxyStats.snapshot(); snapshot.NativeCompactionArmed != test.wantArmed {
				t.Fatalf("snapshot=%+v", snapshot)
			}
			if test.path == "/v1/responses" {
				var storedInput, storedOutput, storedTotal int64
				if err := s.App.Store.DB.QueryRow("SELECT input_tokens,output_tokens,total_tokens FROM proxy_responses").Scan(&storedInput, &storedOutput, &storedTotal); err != nil {
					t.Fatal(err)
				}
				if storedInput != 120 || storedOutput != 30 || storedTotal != 150 {
					t.Fatalf("stored usage=%d/%d/%d", storedInput, storedOutput, storedTotal)
				}
			}
		})
	}
}

func TestProxyDoesNotArmBelowScrubThreshold(t *testing.T) {
	s, key := proxyFixture(t, 1)
	s.proxyScrubPolicy = responsesproxy.ScrubPolicy{TriggerBytes: 1 << 20, TargetBytes: 1 << 19}
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(proxyUsageStream("resp", "completed", 150)))}, nil
	})
	got := proxyRequest(s, key.Token, "/v1/responses", `{"model":"test","input":[]}`)
	usage := terminalEvent(t, got.Body.String())["response"].(map[string]any)["usage"].(map[string]any)
	if got.Code != http.StatusOK || usage["input_tokens"] != float64(120) || usage["total_tokens"] != float64(150) || usage["future_counter"] != float64(7) || s.proxyStats.snapshot().NativeCompactionArmed != 0 {
		t.Fatalf("status=%d usage=%v snapshot=%+v", got.Code, usage, s.proxyStats.snapshot())
	}
}

func TestProxyScrubArmingSurvivesRetryAndFailover(t *testing.T) {
	for _, test := range []struct {
		name             string
		status, accounts int
	}{
		{name: "auth retry", status: http.StatusUnauthorized, accounts: 1},
		{name: "account failover", status: http.StatusTooManyRequests, accounts: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, key := proxyFixture(t, test.accounts)
			body := scrubRequestBody(t, true)
			s.proxyScrubPolicy = responsesproxy.ScrubPolicy{TriggerBytes: len(body) - 1, TargetBytes: len(body) - 500}
			calls := 0
			s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return &http.Response{StatusCode: test.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("retry"))}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(proxyUsageStream("resp", "completed", 150)))}, nil
			})

			got := proxyRequest(s, key.Token, "/v1/responses", string(body))
			usage := terminalEvent(t, got.Body.String())["response"].(map[string]any)["usage"].(map[string]any)
			if got.Code != http.StatusOK || calls != 2 || strings.Count(got.Body.String(), `"type":"response.completed"`) != 1 || usage["input_tokens"] != float64(responsesproxy.NativeCompactionInputTokens) || s.proxyStats.snapshot().NativeCompactionArmed != 1 {
				t.Fatalf("status=%d calls=%d body=%s snapshot=%+v", got.Code, calls, got.Body.String(), s.proxyStats.snapshot())
			}
		})
	}
}

func TestProxyNativeCompactionHandoffSequence(t *testing.T) {
	s, key := proxyFixture(t, 1)
	body := scrubRequestBody(t, true)
	s.proxyScrubPolicy = responsesproxy.ScrubPolicy{TriggerBytes: len(body) - 1, TargetBytes: len(body) - 500}
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(proxyUsageStream("resp", "completed", 150)))}, nil
	})

	normal := proxyRequest(s, key.Token, "/v1/responses", string(body))
	normalUsage := terminalEvent(t, normal.Body.String())["response"].(map[string]any)["usage"].(map[string]any)

	var compaction map[string]any
	if err := json.Unmarshal(body, &compaction); err != nil {
		t.Fatal(err)
	}
	compaction["input"] = append(compaction["input"].([]any), map[string]any{"type": "compaction_trigger"})
	compactionBody, _ := json.Marshal(compaction)
	compacted := proxyRequest(s, key.Token, "/v1/responses", string(compactionBody))
	compactionUsage := terminalEvent(t, compacted.Body.String())["response"].(map[string]any)["usage"].(map[string]any)

	if normal.Code != http.StatusOK || compacted.Code != http.StatusOK || normalUsage["input_tokens"] != float64(responsesproxy.NativeCompactionInputTokens) || compactionUsage["input_tokens"] != float64(120) || s.proxyStats.snapshot().NativeCompactionArmed != 1 {
		t.Fatalf("normal=%v compaction=%v snapshot=%+v", normalUsage, compactionUsage, s.proxyStats.snapshot())
	}
}

func TestProxyAdmitsRequestAboveOldRescueLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates an 81 MiB request")
	}
	s, key := proxyFixture(t, 1)
	body, err := json.Marshal(map[string]any{
		"model": "test",
		"input": []any{
			map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + strings.Repeat("a", 81<<20)}},
			},
			map[string]any{"type": "message", "role": "user", "content": "current"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= 80<<20 || len(body) >= responsesproxy.MaxInboundRequest {
		t.Fatalf("unexpected fixture size %d", len(body))
	}
	var forwarded int
	s.proxyClient.Transport = proxyRoundTrip(func(r *http.Request) (*http.Response, error) {
		value, _ := io.ReadAll(r.Body)
		forwarded = len(value)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(proxyUsageStream("resp", "completed", 150)))}, nil
	})
	got := proxyRequest(s, key.Token, "/v1/responses", string(body))
	if got.Code != http.StatusOK || forwarded > responsesproxy.ScrubTarget || s.proxyStats.snapshot().ScrubbedRequests != 1 {
		t.Fatalf("status=%d forwarded=%d snapshot=%+v", got.Code, forwarded, s.proxyStats.snapshot())
	}
}

func TestProxyRejectsOversizedProtectedTurnWithoutRouting(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		t.Run(path, func(t *testing.T) {
			s, key := proxyFixture(t, 1)
			body := scrubRequestBody(t, false)
			s.proxyScrubPolicy = responsesproxy.ScrubPolicy{TriggerBytes: len(body) - 1, TargetBytes: len(body) - 500}
			s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
				t.Fatal("unexpected upstream request")
				return nil, nil
			})

			got := proxyRequest(s, key.Token, path, string(body))
			if got.Code != http.StatusRequestEntityTooLarge || !strings.Contains(got.Body.String(), "request_too_large_after_scrub") || s.proxyStats.snapshot().ScrubRejectedRequests != 1 {
				t.Fatalf("status=%d body=%s snapshot=%+v", got.Code, got.Body.String(), s.proxyStats.snapshot())
			}
		})
	}
}

func TestProxyRejectsMalformedScrubCandidate(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		t.Run(path, func(t *testing.T) {
			s, key := proxyFixture(t, 1)
			s.proxyScrubPolicy = responsesproxy.ScrubPolicy{TriggerBytes: 2, TargetBytes: 1}
			s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
				t.Fatal("unexpected upstream request")
				return nil, nil
			})
			got := proxyRequest(s, key.Token, path, "not-json")
			if got.Code != http.StatusBadRequest || s.proxyStats.snapshot().ScrubRejectedRequests != 1 {
				t.Fatalf("status=%d snapshot=%+v", got.Code, s.proxyStats.snapshot())
			}
		})
	}
}

func TestProxyScrubRoutesRejectAboveRescueLimit(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		t.Run(path, func(t *testing.T) {
			s, key := proxyFixture(t, 1)
			s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
				t.Fatal("unexpected upstream request")
				return nil, nil
			})
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
			request.Header.Set("Authorization", "Bearer "+key.Token)
			request.ContentLength = responsesproxy.MaxInboundRequest + 1
			response := httptest.NewRecorder()
			s.mux.ServeHTTP(response, request)
			if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "request_too_large") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
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

func terminalEvent(t *testing.T, stream string) map[string]any {
	t.Helper()
	start := strings.LastIndex(stream, "event: response.")
	if start < 0 {
		start = strings.LastIndex(stream, `data: {"type":"response.`)
	}
	if start < 0 {
		t.Fatalf("terminal event missing from %q", stream)
	}
	for _, line := range strings.Split(stream[start:], "\n") {
		if strings.HasPrefix(line, "data: ") {
			var event map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			return event
		}
	}
	t.Fatal("terminal data missing")
	return nil
}
