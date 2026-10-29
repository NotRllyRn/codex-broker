package gateway

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/responsesproxy"
)

func TestTimeoutReadinessAndRecovery(t *testing.T) {
	var logs []string
	e := &Engine{Log: func(level, event, message string) { logs = append(logs, event+" "+message) }}
	if !e.Ready() {
		t.Fatal("initially unready")
	}
	e.failure(httptest.NewRequest("POST", "/v1/responses", nil), "request", "account", "client", "upstream_body", time.Now().UnixMilli(), responsesproxy.ErrIdleReadTimeout)
	if e.Ready() {
		t.Fatal("timeout did not remove readiness")
	}
	for _, field := range []string{"proxy.failure", "request_id=request", "account_id=account", "client_id=client", "stage=upstream_body", "elapsed_ms=", "readiness_cooldown=30s"} {
		if !strings.Contains(logs[0], field) {
			t.Fatalf("log missing %s: %v", field, logs)
		}
	}
	e.unreadyUntil.Store(time.Now().Add(-time.Second).UnixNano())
	if !e.Ready() || !strings.Contains(logs[1], "proxy.ready") {
		t.Fatalf("did not recover: %v", logs)
	}
}

func TestClientCancellationAndOtherFailuresKeepReadiness(t *testing.T) {
	logs := 0
	e := &Engine{Log: func(string, string, string) { logs++ }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.failure(httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx), "r", "a", "c", "upstream_body", 0, responsesproxy.ErrIdleReadTimeout)
	if logs != 0 || !e.Ready() {
		t.Fatal("client cancellation marked unready")
	}
	e.failure(httptest.NewRequest("POST", "/v1/responses", nil), "r", "a", "c", "route", 0, errors.New("route unavailable"))
	if logs != 1 || !e.Ready() {
		t.Fatal("non-timeout marked unready or was not logged")
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, responsesproxy.ErrIdleReadTimeout }

func TestBodyTimeoutIsObservedOnce(t *testing.T) {
	calls := 0
	body := &observedBody{ReadCloser: io.NopCloser(failedReader{}), onError: func(error) { calls++ }}
	for range 2 {
		_, _ = body.Read(make([]byte, 1))
	}
	if calls != 1 {
		t.Fatalf("logged %d times", calls)
	}
}
