package responsesproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHeadersAndRedirects(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("followed redirect with credentials") }))
	defer destination.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer leased-token" || r.Header.Get("ChatGPT-Account-ID") != "leased-account" {
			t.Error("wrong lease headers")
		}
		http.Redirect(w, r, destination.URL, 307)
	}))
	defer upstream.Close()
	client := NewClient()
	defer client.CloseIdleConnections()
	source := httptest.NewRequest("POST", "/v1/responses", nil)
	source.Header.Set("Authorization", "Bearer cbk_secret")
	response, err := Forward(client, source, upstream.URL, []byte("opaque"), "leased-token", "leased-account")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 307 {
		t.Fatalf("status=%d", response.StatusCode)
	}
	source.Header = http.Header{"Connection": {"X-Remove, Authorization"}, "X-Remove": {"secret"}, "Cookie": {"secret"}, "Set-Cookie": {"secret"}, "Proxy-Authorization": {"secret"}, "X-Api-Key": {"secret"}, "Authorization": {"secret"}, "Chatgpt-Account-Id": {"secret"}, "Content-Type": {"text/event-stream"}, "X-Request-Id": {"safe"}}
	filtered := http.Header{}
	CopyHeaders(filtered, source.Header)
	if len(filtered) != 2 || filtered.Get("Content-Type") != "text/event-stream" || filtered.Get("X-Request-ID") != "safe" {
		t.Fatalf("headers=%v", filtered)
	}
}

func TestCompressionIsOpaque(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "" {
			t.Error("transport added compression")
		}
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = io.WriteString(w, "opaque gzip bytes")
	}))
	defer upstream.Close()
	client := NewClient()
	defer client.CloseIdleConnections()
	response, err := Forward(client, httptest.NewRequest("POST", "/v1/responses", nil), upstream.URL, nil, "token", "account")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "opaque gzip bytes" || response.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("response encoding changed")
	}
}

func TestForwardPreservesMethod(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method=%s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	response, err := Forward(NewClient(), httptest.NewRequest(http.MethodGet, "/v1/models", nil), upstream.URL, nil, "token", "account")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

func TestCopyResponseFlushesOpaqueSSE(t *testing.T) {
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"unchanged\"}\n\n"
	w := httptest.NewRecorder()
	CopyResponse(w, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "Connection": {"X-Remove"}, "X-Remove": {"secret"}}, Body: io.NopCloser(strings.NewReader(body))})
	if !w.Flushed || w.Body.String() != body || w.Header().Get("X-Remove") != "" {
		t.Fatalf("response=%v", w)
	}
}

func TestForwardIdleReadTimeout(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		for _, prefix := range []string{"", "data: active\n\n"} {
			t.Run(fmt.Sprintf("http2=%t/streaming=%t", http2, prefix != ""), func(t *testing.T) {
				upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if (r.ProtoMajor == 2) != http2 {
						t.Errorf("HTTP version=%s", r.Proto)
					}
					w.WriteHeader(200)
					_, _ = io.WriteString(w, prefix)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}))
				upstream.EnableHTTP2 = http2
				upstream.StartTLS()
				defer upstream.Close()
				response, err := forward(upstream.Client(), httptest.NewRequest("POST", "/v1/responses", nil), upstream.URL, nil, "token", "account", 50*time.Millisecond)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if !errors.Is(err, ErrIdleReadTimeout) || string(body) != prefix {
					t.Fatalf("body=%q read error=%v", body, err)
				}
			})
		}
	}
}

func TestForwardIdleReadAllowsLongActiveStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 20 {
			_, _ = io.WriteString(w, "data\n")
			w.(http.Flusher).Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer upstream.Close()
	response, err := forward(NewClient(), httptest.NewRequest("POST", "/v1/responses", nil), upstream.URL, nil, "token", "account", 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
}

func TestForwardClientCancellationIsNotIdleTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response, err := Forward(NewClient(), httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx), upstream.URL, nil, "token", "account")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	cancel()
	if _, err := io.ReadAll(response.Body); !errors.Is(err, context.Canceled) || errors.Is(err, ErrIdleReadTimeout) {
		t.Fatalf("read error=%v", err)
	}
}
