// Package responsesproxy contains the stateless transport shared by the broker
// and the ChatGPT adapter. Routing and body transformations belong to callers.
package responsesproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const BackendURL = "https://chatgpt.com/backend-api"
const UpstreamURL = BackendURL + "/codex"
const MaxRequest = 64 << 20
const ThreadIDHeader = "X-Codex-Broker-Thread-ID"
const TurnIDHeader = "X-Codex-Broker-Turn-ID"

func NewClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
			DisableCompression:    true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

const IdleReadTimeout = 2 * time.Minute

var ErrIdleReadTimeout = errors.New("upstream response idle read timeout")

func Forward(client *http.Client, source *http.Request, target string, body []byte, token, accountID string) (*http.Response, error) {
	return forward(client, source, target, body, token, accountID, IdleReadTimeout)
}

func forward(client *http.Client, source *http.Request, target string, body []byte, token, accountID string, idleTimeout time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(source.Context())
	request, err := http.NewRequestWithContext(ctx, source.Method, target, bytes.NewReader(body))
	if err != nil {
		cancel(nil)
		return nil, err
	}
	CopyHeaders(request.Header, source.Header)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("ChatGPT-Account-ID", accountID)
	if request.Header.Get("Originator") == "" {
		request.Header.Set("Originator", "codex_cli_rs")
	}
	// Never let net/http replay inference after a connection failure.
	request.GetBody = nil
	response, err := client.Do(request)
	if err != nil {
		cancel(nil)
		return nil, err
	}
	response.Body = &idleBody{ReadCloser: response.Body, ctx: ctx, cancel: cancel, timeout: idleTimeout}
	return response, nil
}

// Time only blocked upstream reads, not downstream writes or total stream duration.
type idleBody struct {
	io.ReadCloser
	ctx     context.Context
	cancel  context.CancelCauseFunc
	timeout time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	timer := time.AfterFunc(b.timeout, func() { b.cancel(ErrIdleReadTimeout) })
	n, err := b.ReadCloser.Read(p)
	timer.Stop()
	if errors.Is(context.Cause(b.ctx), ErrIdleReadTimeout) {
		return n, ErrIdleReadTimeout
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.cancel(nil)
	return b.ReadCloser.Close()
}

// CopyHeaders strips credentials, cookies and hop-by-hop headers in both directions.
func CopyHeaders(destination, source http.Header) {
	blocked := map[string]bool{}
	for _, value := range source.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			blocked[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	for name, values := range source {
		lower := strings.ToLower(name)
		if blocked[lower] {
			continue
		}
		switch lower {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "authorization", "chatgpt-account-id", "cookie", "set-cookie", "x-api-key", "api-key", "forwarded", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto", strings.ToLower(ThreadIDHeader), strings.ToLower(TurnIDHeader):
			continue
		}
		destination[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
}

func FailureKind(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "auth"
	case http.StatusTooManyRequests:
		return "quota"
	default:
		return ""
	}
}

// Inspect only the error envelope, never assistant text or request content.
// Restore the body so unrelated errors still pass through unchanged.
func ResponseFailureKind(response *http.Response) string {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return ""
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return "quota"
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	response.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(body), response.Body), Closer: response.Body}
	var envelope struct {
		Error  any `json:"error"`
		Detail any `json:"detail"`
	}
	if err == nil && json.Unmarshal(body, &envelope) == nil && (terminalFailureKind(envelope.Error) == "quota" || terminalFailureKind(envelope.Detail) == "quota") {
		return "quota"
	}
	return FailureKind(response.StatusCode)
}

type replayBody struct {
	io.Reader
	io.Closer
}

func CopyResponse(w http.ResponseWriter, response *http.Response) {
	defer response.Body.Close()
	CopyHeaders(w.Header(), response.Header)
	// Inference must not be cached, even when upstream supplies cache headers.
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.WriteHeader(response.StatusCode)
	controller := http.NewResponseController(w)
	_ = controller.Flush()
	if _, err := io.Copy(flushWriter{w, controller}, response.Body); err != nil {
		// Headers have been delivered; abort the stream without replay or an error body.
		panic(http.ErrAbortHandler)
	}
}

type flushWriter struct {
	io.Writer
	controller *http.ResponseController
}

func (w flushWriter) Write(value []byte) (int, error) {
	n, err := w.Writer.Write(value)
	if err == nil {
		_ = w.controller.Flush()
	}
	return n, err
}
