package loopback

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultListen = "127.0.0.1:8789"
	upstreamURL   = "https://chatgpt.com/backend-api/codex"
	maxRequest    = 32 << 20
	maxBrokerBody = 64 << 10
	maxErrorBody  = 1 << 20
)

type Config struct {
	Listen, BrokerURL, BrokerCA string
	UpstreamURL                 string
	UpstreamClient              *http.Client
}

type Adapter struct {
	broker, upstream *http.Client
	brokerURL        *url.URL
	upstreamURL      *url.URL
	sessionID        string
	mu               sync.Mutex
	preferred        string
}

type routeRequest struct {
	SessionID          string `json:"session_id"`
	TurnID             string `json:"turn_id"`
	PreferredAccountID string `json:"preferred_account_id,omitempty"`
	FailedAccountID    string `json:"failed_account_id,omitempty"`
	FailureKind        string `json:"failure_kind,omitempty"`
}

type lease struct {
	Status           string `json:"status"`
	AccountID        string `json:"account_id"`
	AccessToken      string `json:"access_token"`
	ChatGPTAccountID string `json:"chatgpt_account_id"`
}

type wait struct {
	Status            string `json:"status"`
	Code              string `json:"code"`
	RetryAfterSeconds int    `json:"retry_after_seconds"`
}

type brokerStatusError struct{ status int }

func (e brokerStatusError) Error() string { return fmt.Sprintf("broker returned HTTP %d", e.status) }

func New(config Config) (*Adapter, error) {
	if err := ValidateListen(config.Listen); err != nil {
		return nil, err
	}
	brokerURL, err := origin(config.BrokerURL)
	if err != nil || brokerURL.Scheme != "https" {
		return nil, errors.New("broker URL must be an HTTPS origin")
	}
	brokerTransport, err := transport(config.BrokerCA)
	if err != nil {
		return nil, err
	}
	target := config.UpstreamURL
	if target == "" {
		target = upstreamURL
	}
	parsedTarget, err := url.Parse(target)
	if err != nil || parsedTarget.Scheme != "https" || parsedTarget.Host == "" {
		return nil, errors.New("upstream URL is invalid")
	}
	upstreamClient := config.UpstreamClient
	if upstreamClient == nil {
		upstreamTransport, transportErr := transport("")
		if transportErr != nil {
			return nil, transportErr
		}
		upstreamClient = &http.Client{Transport: upstreamTransport, CheckRedirect: noRedirect}
	}
	return &Adapter{
		broker:      &http.Client{Transport: brokerTransport, Timeout: time.Minute, CheckRedirect: noRedirect},
		upstream:    upstreamClient,
		brokerURL:   brokerURL,
		upstreamURL: parsedTarget,
		sessionID:   "macos-" + randomID(),
	}, nil
}

func origin(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("URL must be an origin")
	}
	parsed.Path = ""
	return parsed, nil
}

func ValidateListen(address string) error {
	host, rawPort, err := net.SplitHostPort(address)
	port, portErr := strconv.Atoi(rawPort)
	if err != nil || portErr != nil || port < 1 || port > 65535 {
		return errors.New("listen address must include a port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("listen address must use a loopback IP")
	}
	return nil
}

func transport(caPath string) (*http.Transport, error) {
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if caPath != "" {
		certificate, readErr := os.ReadFile(caPath)
		if readErr != nil {
			return nil, fmt.Errorf("broker CA could not be read: %w", readErr)
		}
		if !roots.AppendCertsFromPEM(certificate) {
			return nil, errors.New("broker CA contains no certificates")
		}
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
	}, nil
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (a *Adapter) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.health)
	mux.HandleFunc("POST /v1/responses", a.responses)
	mux.HandleFunc("POST /v1/responses/compact", a.responses)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}

func (a *Adapter) health(w http.ResponseWriter, r *http.Request) {
	key, ok := clientKey(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	request, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, a.endpoint("/api/v1/health"), nil)
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := a.broker.Do(request)
	if err != nil {
		http.Error(w, "broker unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxBrokerBody))
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			http.Error(w, "broker client key rejected", http.StatusUnauthorized)
			return
		}
		http.Error(w, "broker rejected health check", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, "{\"status\":\"ok\"}\n")
}

func (a *Adapter) responses(w http.ResponseWriter, r *http.Request) {
	key, ok := clientKey(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	body, err := readBounded(r.Body, maxRequest)
	if err != nil {
		http.Error(w, "request body is too large", http.StatusRequestEntityTooLarge)
		return
	}
	turnID := randomID()
	request := routeRequest{SessionID: a.sessionID, TurnID: turnID, PreferredAccountID: a.preference()}
	attempts := map[string]int{}
	for {
		selected, waiting, routeErr := a.route(r.Context(), key, request)
		if routeErr != nil {
			var statusError brokerStatusError
			if errors.As(routeErr, &statusError) && (statusError.status == http.StatusUnauthorized || statusError.status == http.StatusForbidden) {
				http.Error(w, "broker client key rejected", http.StatusUnauthorized)
				return
			}
			http.Error(w, "broker routing failed", http.StatusBadGateway)
			return
		}
		if waiting != nil {
			if !sleep(r.Context(), time.Duration(waiting.RetryAfterSeconds)*time.Second) {
				return
			}
			request = routeRequest{SessionID: a.sessionID, TurnID: turnID, PreferredAccountID: a.preference()}
			attempts = map[string]int{}
			continue
		}
		if attempts[selected.AccountID] > 0 && !(request.FailureKind == "auth" && request.FailedAccountID == selected.AccountID && attempts[selected.AccountID] == 1) {
			http.Error(w, "broker returned a failed account", http.StatusBadGateway)
			return
		}
		attempts[selected.AccountID]++
		response, upstreamErr := a.forward(r, body, *selected)
		if upstreamErr != nil {
			http.Error(w, "upstream request failed", http.StatusBadGateway)
			return
		}
		kind := failureKind(response.StatusCode)
		if kind == "" {
			a.setPreference(selected.AccountID)
			copyResponse(w, response)
			return
		}
		errorBody, bodyErr := readBounded(response.Body, maxErrorBody)
		response.Body.Close()
		if bodyErr != nil {
			http.Error(w, "upstream error body is too large", http.StatusBadGateway)
			return
		}
		if kind == "auth" && attempts[selected.AccountID] >= 2 {
			writeResponse(w, response, errorBody)
			return
		}
		request = routeRequest{SessionID: a.sessionID, TurnID: turnID, PreferredAccountID: selected.AccountID, FailedAccountID: selected.AccountID, FailureKind: kind}
	}
}

func clientKey(r *http.Request) (string, bool) {
	scheme, value, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	return value, ok && strings.EqualFold(scheme, "bearer") && strings.HasPrefix(value, "cbk_")
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > limit {
		return nil, errors.New("body exceeds limit")
	}
	return value, nil
}

func (a *Adapter) route(ctx context.Context, key string, input routeRequest) (*lease, *wait, error) {
	body, _ := json.Marshal(input)
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint("/api/v1/route"), bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := a.broker.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	payload, err := readBounded(response.Body, maxBrokerBody)
	if err != nil {
		return nil, nil, err
	}
	if response.StatusCode == http.StatusOK {
		var result lease
		if json.Unmarshal(payload, &result) != nil || result.Status != "ok" || result.AccountID == "" || result.AccessToken == "" || result.ChatGPTAccountID == "" {
			return nil, nil, errors.New("invalid broker lease")
		}
		return &result, nil, nil
	}
	if response.StatusCode == http.StatusTooManyRequests {
		var result wait
		if json.Unmarshal(payload, &result) != nil || result.Status != "wait" || result.Code != "POOL_EXHAUSTED" || result.RetryAfterSeconds < 1 || result.RetryAfterSeconds > 8*24*60*60 {
			return nil, nil, errors.New("invalid broker wait")
		}
		return nil, &result, nil
	}
	return nil, nil, brokerStatusError{response.StatusCode}
}

func (a *Adapter) endpoint(path string) string {
	value := *a.brokerURL
	value.Path = path
	return value.String()
}

func (a *Adapter) forward(source *http.Request, body []byte, selected lease) (*http.Response, error) {
	target := *a.upstreamURL
	target.Path = strings.TrimSuffix(target.Path, "/") + strings.TrimPrefix(source.URL.Path, "/v1")
	target.RawQuery = source.URL.RawQuery
	request, err := http.NewRequestWithContext(source.Context(), http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(request.Header, source.Header)
	for _, name := range []string{"Authorization", "ChatGPT-Account-ID", "ChatGPT-Account-Id", "Cookie", "Proxy-Authorization", "X-Api-Key", "Api-Key"} {
		request.Header.Del(name)
	}
	request.Header.Set("Authorization", "Bearer "+selected.AccessToken)
	request.Header.Set("ChatGPT-Account-ID", selected.ChatGPTAccountID)
	if request.Header.Get("Originator") == "" {
		request.Header.Set("Originator", "codex_cli_rs")
	}
	return a.upstream.Do(request)
}

func copyHeaders(destination, source http.Header) {
	for name, values := range source {
		if hopHeader(name) {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func hopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

func failureKind(status int) string {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "auth"
	}
	if status == http.StatusTooManyRequests {
		return "quota"
	}
	return ""
}

func copyResponse(w http.ResponseWriter, response *http.Response) {
	defer response.Body.Close()
	copyHeaders(w.Header(), response.Header)
	w.Header().Del("Set-Cookie")
	w.WriteHeader(response.StatusCode)
	writer := io.Writer(w)
	if flusher, ok := w.(http.Flusher); ok {
		writer = flushWriter{w, flusher}
	}
	_, _ = io.Copy(writer, response.Body)
}

func writeResponse(w http.ResponseWriter, response *http.Response, body []byte) {
	copyHeaders(w.Header(), response.Header)
	w.Header().Del("Set-Cookie")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
}

type flushWriter struct {
	io.Writer
	http.Flusher
}

func (w flushWriter) Write(value []byte) (int, error) {
	written, err := w.Writer.Write(value)
	w.Flusher.Flush()
	return written, err
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (a *Adapter) preference() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.preferred
}

func (a *Adapter) setPreference(value string) {
	a.mu.Lock()
	a.preferred = value
	a.mu.Unlock()
}

func randomID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value)
}
