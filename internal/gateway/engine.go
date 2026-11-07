// Package gateway implements the shared Codex proxy data plane used by the
// primary broker and relay edges.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/NotRllyRn/codex-broker/extensions/requestsize"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/proxyextension"
	"github.com/NotRllyRn/codex-broker/internal/responsesproxy"
)

type Client struct {
	ID, Name, Prefix string
	StreamerMode     bool
}

type RouteRequest struct{ SessionID, TurnID, PreferredAccountID, FailedAccountID, FailureKind string }

type Lease struct {
	AccountID, AccountLabel, AccessToken, ChatGPTAccountID string
	ExpiresAtMS                                            int64
	UsageSnapshotAtMS                                      int64
	ShortRemainingPercent, WeeklyRemainingPercent          *int64
	ShortResetsAtMS, WeeklyResetsAtMS                      *int64
	Reserve                                                bool   `json:"-"`
	PermitEpoch                                            string `json:"-"`
	PermitExpiresAtMS                                      int64  `json:"-"`
}

type PoolWait struct {
	NextRetryAtMS     int64
	RetryAfterSeconds int
}

type Observation struct {
	RequestID, AccountID, ClientID, ThreadID, TurnID, Endpoint                 string
	RequestModel, ReasoningEffort, RequestServiceTier, PreviousResponseID      string
	ResponseID, ResponseModel, ResponseServiceTier, TerminalStatus             string
	UpstreamAttempts, UpstreamHTTPStatus                                       int
	RequestStartedAtMS, UpstreamStartedAtMS, ResponseHeadersAtMS               int64
	FirstResponseEventAtMS, TerminalEventAtMS                                  int64
	InputTokens, CachedInputTokens, OutputTokens, ReasoningTokens, TotalTokens *int64
	UsageJSON                                                                  []byte
}

type Authority interface {
	Authenticate(*http.Request, string) (*Client, error)
	Route(context.Context, string, RouteRequest) (*Lease, *PoolWait, error)
	Observe(context.Context, Observation) error
	ReportFailure(context.Context, string, string, string) error
	Activate(context.Context, Lease) error
}

type Metrics interface {
	Begin(Client) func()
	Failover()
	Scrub(int, int)
	RejectScrub()
	NativeCompactionArmed()
}

type Engine struct {
	Extensions         *proxyextension.Manager
	ExtensionError     error
	ExtensionSettings  func(context.Context) (proxyextension.Configuration, error)
	DisabledExtensions func(context.Context) (map[string]bool, error)
	Authority          Authority
	Client             *http.Client
	Metrics            Metrics
	ScrubPolicy        requestsize.ScrubPolicy
	largeOnce          sync.Once
	largeGate          chan struct{}
	Log                func(level, event, message string)
	unreadyUntil       atomic.Int64
}

func New(authority Authority, metrics Metrics) *Engine {
	extensions, err := proxyextension.Load(os.Getenv("WINDOWKEEPER_PROXY_EXTENSIONS_DIR"))
	return &Engine{Authority: authority, Client: responsesproxy.NewClient(), Metrics: metrics, Extensions: extensions, ExtensionError: err}
}

func (e *Engine) Serve(w http.ResponseWriter, r *http.Request) error {
	requestStartedAt := time.Now().UnixMilli()
	key, err := e.Authority.Authenticate(r, bearer(r))
	if err != nil {
		return authError(w, err)
	}
	if e.Metrics != nil {
		defer e.Metrics.Begin(*key)()
	}
	isResponse := r.URL.Path == "/v1/responses"
	isScrubbable := isResponse || r.URL.Path == "/v1/responses/compact"
	threadID, turnID := strings.TrimSpace(r.Header.Get(responsesproxy.ThreadIDHeader)), strings.TrimSpace(r.Header.Get(responsesproxy.TurnIDHeader))
	if utf8.RuneCountInString(threadID) > 200 || utf8.RuneCountInString(turnID) > 200 {
		return Error(w, http.StatusBadRequest, "invalid_correlation_id", "Thread and turn identifiers must not exceed 200 characters.")
	}
	policy := e.ScrubPolicy.Normalized()
	if isScrubbable && r.ContentLength > responsesproxy.MaxInboundRequest {
		return Error(w, 413, "request_too_large", "Request body exceeds the broker limit.")
	}
	var releaseLarge func()
	if isScrubbable && (r.ContentLength < 0 || r.ContentLength >= int64(policy.TriggerBytes)) {
		e.largeOnce.Do(func() { e.largeGate = make(chan struct{}, 1) })
		select {
		case e.largeGate <- struct{}{}:
			releaseLarge = func() { <-e.largeGate }
			defer func() {
				if releaseLarge != nil {
					releaseLarge()
				}
			}()
		case <-r.Context().Done():
			return nil
		}
	}
	limit := int64(responsesproxy.MaxRequest)
	if isScrubbable {
		limit = responsesproxy.MaxInboundRequest
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return Error(w, 413, "request_too_large", "Request body exceeds the broker limit.")
		}
		return Error(w, 400, "invalid_request", "Request body could not be read.")
	}

	if e.ExtensionError != nil {
		return e.extensionFailure(w, e.ExtensionError)
	}
	disabled := map[string]bool{}
	if e.DisabledExtensions != nil {
		disabled, err = e.DisabledExtensions(r.Context())
		if err != nil {
			return e.extensionFailure(w, err)
		}
	}
	if err := e.Extensions.RefreshSettings(r.Context(), e.ExtensionSettings); err != nil {
		return e.extensionFailure(w, err)
	}
	requestID := core.NewID()
	identity := proxyextension.Context{RequestID: requestID, ClientID: key.ID}
	if e.Extensions.Active(disabled) {
		identity = extensionIdentity(r, body, key.ID, requestID)
	}
	ext, err := e.Extensions.Start(r.Context(), identity, disabled)
	if err != nil {
		return e.extensionFailure(w, err)
	}
	outcome := "error"
	defer extensionFinish(ext, r.Context(), &outcome)
	// Hook edits never mutate the caller's request or its authenticated identity.
	if ext.Active() {
		r = r.Clone(r.Context())
	}
	var local *http.Response
	body, local, err = extensionRequest(ext, "request", r, body, nil)
	if err != nil {
		return e.extensionFailure(w, err)
	}
	sendLocal := func(response *http.Response) error {
		err := extensionResponse(ext, w, response, func(dst http.ResponseWriter) error {
			if ext.Has("client_event") && response.StatusCode == http.StatusOK {
				_, err := responsesproxy.CopyNoticeResponse(dst, response, responsesproxy.Notice{}, nil, nil, responsesproxy.StreamOptions{NoticeText: func(responsesproxy.Notice) string { return "" }, ClientEvent: extensionEvent(ext, "client_event")})
				return err
			}
			responsesproxy.CopyResponse(dst, response)
			return nil
		})
		if err != nil {
			return e.extensionFailure(w, err)
		}
		outcome = "local"
		return nil
	}
	if local != nil {
		return sendLocal(local)
	}
	fail := func(status int, code, message string) error {
		if !ext.Has("response") && !ext.Has("body") {
			return Error(w, status, code, message)
		}
		buffer := &boundedResponse{header: http.Header{}}
		_ = Error(buffer, status, code, message)
		response := &http.Response{StatusCode: buffer.status, Header: buffer.header, Body: io.NopCloser(&buffer.body)}
		if err := extensionResponse(ext, w, response, func(dst http.ResponseWriter) error { responsesproxy.CopyResponse(dst, response); return nil }); err != nil {
			return e.extensionFailure(w, err)
		}
		return nil
	}
	body, problem := ext.Prepare(r.URL.Path, body, e.ScrubPolicy, e.Metrics)
	if problem != nil {
		return fail(problem.Status, problem.Code, problem.Message)
	}
	if releaseLarge != nil {
		releaseLarge()
		releaseLarge = nil
	}
	body, local, err = extensionRequest(ext, "prepared", r, body, nil)
	if err != nil {
		return e.extensionFailure(w, err)
	}
	if local != nil {
		return sendLocal(local)
	}
	if len(body) > responsesproxy.MaxRequest {
		return fail(413, "request_too_large", "Extension request exceeds the broker limit.")
	}
	type requestMetadata struct {
		Model              string `json:"model"`
		ServiceTier        string `json:"service_tier"`
		PreviousResponseID string `json:"previous_response_id"`
		Reasoning          struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	var metadata requestMetadata
	input := RouteRequest{SessionID: "proxy-" + requestID, TurnID: core.NewID()}
	attemptCounts, previous := map[string]int{}, ""
	retries, failovers, attemptCount := 0, 0, 0
	for {
		if err := ext.Emit("route", map[string]any{"attempt": attemptCount + 1, "failureKind": input.FailureKind}); err != nil {
			return e.extensionFailure(w, err)
		}
		selected, wait, routeErr := e.Authority.Route(r.Context(), key.ID, input)
		if routeErr != nil {
			e.failure(r, requestID, "", key.ID, "route", requestStartedAt, routeErr)
			return fail(503, "broker_unavailable", "Codex Broker unavailable.")
		}
		if wait != nil {
			w.Header().Set("Retry-After", strconv.Itoa(wait.RetryAfterSeconds))
			return fail(429, "pool_exhausted", "All accounts unavailable · retry "+CompactDuration(wait.RetryAfterSeconds)+".")
		}
		if selected == nil {
			return fail(503, "broker_unavailable", "Codex Broker unavailable.")
		}
		if attemptCounts[selected.AccountID] > 0 && !(input.FailureKind == "auth" && input.FailedAccountID == selected.AccountID && attemptCounts[selected.AccountID] == 1) {
			return fail(502, "routing_failed", "Broker returned an unavailable account.")
		}
		if previous != "" && previous != selected.AccountID {
			if e.Metrics != nil {
				e.Metrics.Failover()
			}
			failovers++
		} else if previous != "" {
			retries++
		}
		previous = selected.AccountID
		attemptCounts[selected.AccountID]++
		target := Upstream(r.URL.Path)
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		attemptCount++
		attemptRequest := r
		if ext.Has("attempt") {
			attemptRequest = r.Clone(r.Context())
		}
		attemptBody, synthetic, hookErr := extensionRequest(ext, "attempt", attemptRequest, body, map[string]any{"attempt": attemptCount, "accountId": selected.AccountID})
		if hookErr != nil {
			return e.extensionFailure(w, hookErr)
		}
		if synthetic != nil {
			return sendLocal(synthetic)
		}
		if len(attemptBody) > responsesproxy.MaxRequest {
			return e.extensionFailure(w, err)
		}
		if isResponse {
			metadata = requestMetadata{}
			_ = json.Unmarshal(attemptBody, &metadata)
		}
		upstreamStartedAt := time.Now().UnixMilli()
		response, forwardErr := responsesproxy.Forward(e.Client, attemptRequest, target, attemptBody, selected.AccessToken, selected.ChatGPTAccountID)
		if forwardErr != nil {
			e.failure(r, requestID, selected.AccountID, key.ID, "upstream_headers", requestStartedAt, forwardErr)
			return fail(502, "upstream_unavailable", "Upstream request failed.")
		}
		response.Body = &observedBody{ReadCloser: response.Body, onError: func(err error) {
			e.failure(r, requestID, selected.AccountID, key.ID, "upstream_body", requestStartedAt, err)
		}}
		responseHeadersAt := time.Now().UnixMilli()
		upstreamStatus := response.StatusCode
		if err := ext.Emit("upstream", map[string]any{"attempt": attemptCount, "accountId": selected.AccountID, "status": response.StatusCode, "headers": safeHeaders(response.Header)}); err != nil {
			response.Body.Close()
			return e.extensionFailure(w, err)
		}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			response.Body.Close()
			return fail(502, "upstream_redirect", "Upstream redirect rejected.")
		}
		if selected.Reserve && response.StatusCode >= 200 && response.StatusCode < 300 {
			if err := e.Authority.Activate(context.WithoutCancel(r.Context()), *selected); err != nil {
				response.Body.Close()
				return fail(503, "relay_cache_unavailable", "Relay state could not be saved safely.")
			}
		}
		kind := responsesproxy.ResponseFailureKind(response)
		if kind == "" || kind == "auth" && attemptCounts[selected.AccountID] >= 2 {
			if isResponse && response.StatusCode == http.StatusOK {
				notice := Notice(*selected)
				if key.StreamerMode {
					notice.AccountID, notice.AccountLabel = "", "Usage stats"
				}
				if failovers > 0 {
					notice.Status = "↻ failover"
				} else if retries > 0 {
					notice.Status = "↻ retry"
				}
				observe := func(terminal responsesproxy.TerminalResponse) {
					ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
					defer cancel()
					if terminal.FailureKind != "" {
						_ = e.Authority.ReportFailure(ctx, key.ID, selected.AccountID, terminal.FailureKind)
					}
					_ = e.Authority.Observe(ctx, Observation{
						RequestID: requestID, AccountID: selected.AccountID, ClientID: key.ID, ThreadID: threadID, TurnID: turnID, Endpoint: r.URL.Path,
						RequestModel: metadata.Model, ReasoningEffort: metadata.Reasoning.Effort, RequestServiceTier: metadata.ServiceTier, PreviousResponseID: metadata.PreviousResponseID,
						ResponseID: terminal.ID, ResponseModel: terminal.Model, ResponseServiceTier: terminal.ServiceTier, TerminalStatus: terminal.Status,
						UpstreamAttempts: attemptCount, UpstreamHTTPStatus: upstreamStatus, RequestStartedAtMS: requestStartedAt, UpstreamStartedAtMS: upstreamStartedAt,
						ResponseHeadersAtMS: responseHeadersAt, FirstResponseEventAtMS: terminal.FirstEventAtMS, TerminalEventAtMS: terminal.TerminalEventAtMS,
						InputTokens: terminal.InputTokens, CachedInputTokens: terminal.CachedInputTokens, OutputTokens: terminal.OutputTokens, ReasoningTokens: terminal.ReasoningOutputTokens, TotalTokens: terminal.TotalTokens, UsageJSON: terminal.UsageJSON,
					})
				}
				options := responsesproxy.StreamOptions{NoticeText: ext.UsageNotice, UpstreamEvent: extensionEvent(ext, "upstream_event"), ClientEvent: ext.ClientEvent(extensionEvent(ext, "client_event"))}
				if copyErr := extensionResponse(ext, w, response, func(dst http.ResponseWriter) error {
					_, err := responsesproxy.CopyNoticeResponse(dst, response, notice, nil, observe, options)
					return err
				}); copyErr != nil {
					if errors.Is(copyErr, responsesproxy.ErrIdleReadTimeout) {
						return fail(http.StatusGatewayTimeout, "upstream_timeout", "Upstream response stalled.")
					}
					return fail(http.StatusBadGateway, "invalid_upstream_stream", "Upstream stream validation failed.")
				}
				outcome = "complete"
				return nil
			}
			if err := extensionResponse(ext, w, response, func(dst http.ResponseWriter) error { responsesproxy.CopyResponse(dst, response); return nil }); err != nil {
				return e.extensionFailure(w, err)
			}
			outcome = "complete"
			return nil
		}
		response.Body.Close()
		if err := ext.Emit("retry", map[string]any{"attempt": attemptCount, "accountId": selected.AccountID, "failureKind": kind}); err != nil {
			return e.extensionFailure(w, err)
		}
		input.PreferredAccountID, input.FailedAccountID, input.FailureKind = selected.AccountID, selected.AccountID, kind
	}
}

func (e *Engine) Upgrade(w http.ResponseWriter, r *http.Request) error {
	if _, err := e.Authority.Authenticate(r, bearer(r)); err != nil {
		return authError(w, err)
	}
	w.Header().Set("Connection", "close")
	return Error(w, http.StatusUpgradeRequired, "http_required", "Use HTTP streaming for Responses API requests.")
}

func bearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return token
}

func authError(w http.ResponseWriter, err error) error {
	status, code, message := http.StatusInternalServerError, "broker_unavailable", "Broker authentication unavailable."
	var problem *core.Error
	if errors.As(err, &problem) {
		status, code, message = problem.Status, strings.ToLower(problem.Code), problem.Detail
	}
	return Error(w, status, code, message)
}

func Notice(selected Lease) responsesproxy.Notice {
	text := func(value *int64) string {
		formatted := core.ISOTime(value)
		if formatted == nil {
			return ""
		}
		return *formatted
	}
	return responsesproxy.Notice{AccountID: selected.AccountID, AccountLabel: selected.AccountLabel, ShortRemainingPercent: selected.ShortRemainingPercent, WeeklyRemainingPercent: selected.WeeklyRemainingPercent, ShortResetsAt: text(selected.ShortResetsAtMS), WeeklyResetsAt: text(selected.WeeklyResetsAtMS)}
}

func Upstream(path string) string {
	switch path {
	case "/transcribe", "/codex/thread/goal/get", "/codex/thread/goal/set":
		return responsesproxy.BackendURL + path
	default:
		return responsesproxy.UpstreamURL + strings.TrimPrefix(path, "/v1")
	}
}

func CompactDuration(seconds int) string {
	minutes := max(0, (seconds+59)/60)
	if minutes >= 24*60 {
		return strconv.Itoa(minutes/(24*60)) + "d" + strconv.Itoa(minutes/60%24) + "h"
	}
	if minutes >= 60 {
		return strconv.Itoa(minutes/60) + "h" + strconv.Itoa(minutes%60) + "m"
	}
	return strconv.Itoa(minutes) + "m"
}

func Error(w http.ResponseWriter, status int, code, message string) error {
	kind := "server_error"
	if status == 429 {
		kind = "rate_limit_error"
	} else if status == 401 || status == 403 {
		kind = "authentication_error"
	} else if status < 500 {
		kind = "invalid_request_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message, "type": kind, "code": code}})
	return nil
}
