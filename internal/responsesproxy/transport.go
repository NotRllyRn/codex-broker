// Package responsesproxy contains the stateless transport shared by the broker
// and the ChatGPT adapter. Routing and body transformations belong to callers.
package responsesproxy

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const UpstreamURL = "https://chatgpt.com/backend-api/codex"
const MaxRequest = 32 << 20

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

func Forward(client *http.Client, source *http.Request, target string, body []byte, token, accountID string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(source.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
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
	return client.Do(request)
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
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "authorization", "chatgpt-account-id", "cookie", "set-cookie", "x-api-key", "api-key", "forwarded", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto":
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
