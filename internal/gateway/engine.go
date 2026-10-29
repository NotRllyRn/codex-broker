// Package gateway implements the shared Codex proxy data plane used by the
// primary broker and relay edges.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/NotRllyRn/codex-broker/internal/core"
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
	Authority    Authority
	Client       *http.Client
	Metrics      Metrics
	ScrubPolicy  responsesproxy.ScrubPolicy
	largeOnce    sync.Once
	largeGate    chan struct{}
	Log          func(level, event, message string)
	unreadyUntil atomic.Int64
}

func New(authority Authority, metrics Metrics) *Engine {
	return &Engine{Authority: authority, Client: responsesproxy.NewClient(), Metrics: metrics}
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
	policy := e.ScrubPolicy
	if policy.TriggerBytes == 0 {
		policy = responsesproxy.DefaultScrubPolicy
	}
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
	armNativeCompaction := false
	if isScrubbable {
		body = responsesproxy.PrepareNotice(body)
		result, scrubErr := responsesproxy.Scrub(body, policy)
		if scrubErr != nil {
			if e.Metrics != nil {
				e.Metrics.RejectScrub()
			}
			if errors.Is(scrubErr, responsesproxy.ErrInvalidScrubRequest) && len(body) <= responsesproxy.MaxRequest {
				return Error(w, 400, "invalid_request", "Large Responses request is not valid JSON.")
			}
			return Error(w, 413, "request_too_large_after_scrub", "Request remains too large after historical content cleanup.")
		}
		armNativeCompaction = isResponse && result.Changed && !responsesproxy.IsCompactionRequest(r.URL.Path, body)
		body = result.Body
		if result.Changed && e.Metrics != nil {
			e.Metrics.Scrub(result.BeforeBytes, result.AfterBytes)
		}
		if len(body) > responsesproxy.MaxRequest {
			if e.Metrics != nil {
				e.Metrics.RejectScrub()
			}
			return Error(w, 413, "request_too_large_after_scrub", "Request remains too large after historical content cleanup.")
		}
	}
	if releaseLarge != nil {
		releaseLarge()
		releaseLarge = nil
	}
	var metadata struct {
		Model              string `json:"model"`
		ServiceTier        string `json:"service_tier"`
		PreviousResponseID string `json:"previous_response_id"`
		Reasoning          struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if isResponse {
		_ = json.Unmarshal(body, &metadata)
	}
	requestID := core.NewID()
	input := RouteRequest{SessionID: "proxy-" + requestID, TurnID: core.NewID()}
	attemptCounts, previous := map[string]int{}, ""
	retries, failovers, attemptCount := 0, 0, 0
	for {
		selected, wait, routeErr := e.Authority.Route(r.Context(), key.ID, input)
		if routeErr != nil {
			e.failure(r, requestID, "", key.ID, "route", requestStartedAt, routeErr)
			return Error(w, 503, "broker_unavailable", "Codex Broker unavailable.")
		}
		if wait != nil {
			w.Header().Set("Retry-After", strconv.Itoa(wait.RetryAfterSeconds))
			return Error(w, 429, "pool_exhausted", "All accounts unavailable · retry "+CompactDuration(wait.RetryAfterSeconds)+".")
		}
		if selected == nil {
			return Error(w, 503, "broker_unavailable", "Codex Broker unavailable.")
		}
		if attemptCounts[selected.AccountID] > 0 && !(input.FailureKind == "auth" && input.FailedAccountID == selected.AccountID && attemptCounts[selected.AccountID] == 1) {
			return Error(w, 502, "routing_failed", "Broker returned an unavailable account.")
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
		upstreamStartedAt := time.Now().UnixMilli()
		response, forwardErr := responsesproxy.Forward(e.Client, r, target, body, selected.AccessToken, selected.ChatGPTAccountID)
		if forwardErr != nil {
			e.failure(r, requestID, selected.AccountID, key.ID, "upstream_headers", requestStartedAt, forwardErr)
			return Error(w, 502, "upstream_unavailable", "Upstream request failed.")
		}
		response.Body = &observedBody{ReadCloser: response.Body, onError: func(err error) {
			e.failure(r, requestID, selected.AccountID, key.ID, "upstream_body", requestStartedAt, err)
		}}
		responseHeadersAt := time.Now().UnixMilli()
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			response.Body.Close()
			return Error(w, 502, "upstream_redirect", "Upstream redirect rejected.")
		}
		if selected.Reserve && response.StatusCode >= 200 && response.StatusCode < 300 {
			if err := e.Authority.Activate(context.WithoutCancel(r.Context()), *selected); err != nil {
				response.Body.Close()
				return Error(w, 503, "relay_cache_unavailable", "Relay state could not be saved safely.")
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
						UpstreamAttempts: attemptCount, UpstreamHTTPStatus: response.StatusCode, RequestStartedAtMS: requestStartedAt, UpstreamStartedAtMS: upstreamStartedAt,
						ResponseHeadersAtMS: responseHeadersAt, FirstResponseEventAtMS: terminal.FirstEventAtMS, TerminalEventAtMS: terminal.TerminalEventAtMS,
						InputTokens: terminal.InputTokens, CachedInputTokens: terminal.CachedInputTokens, OutputTokens: terminal.OutputTokens, ReasoningTokens: terminal.ReasoningOutputTokens, TotalTokens: terminal.TotalTokens, UsageJSON: terminal.UsageJSON,
					})
				}
				options := responsesproxy.StreamOptions{ArmNativeCompaction: armNativeCompaction}
				if e.Metrics != nil {
					options.OnNativeCompactionArmed = e.Metrics.NativeCompactionArmed
				}
				if _, copyErr := responsesproxy.CopyNoticeResponse(w, response, notice, nil, observe, options); copyErr != nil {
					if errors.Is(copyErr, responsesproxy.ErrIdleReadTimeout) {
						return Error(w, http.StatusGatewayTimeout, "upstream_timeout", "Upstream response stalled.")
					}
					return Error(w, http.StatusBadGateway, "invalid_upstream_stream", "Upstream stream validation failed.")
				}
				return nil
			}
			responsesproxy.CopyResponse(w, response)
			return nil
		}
		response.Body.Close()
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
