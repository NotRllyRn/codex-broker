package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NotRllyRn/codex-broker/internal/broker"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/responsesproxy"
)

func (s *Server) responses(w http.ResponseWriter, r *http.Request) error {
	requestStartedAt := time.Now().UnixMilli()
	key, err := s.client(r)
	if err != nil {
		status, code, message := http.StatusInternalServerError, "broker_unavailable", "Broker authentication unavailable."
		var problem *core.Error
		if errors.As(err, &problem) {
			status, code, message = problem.Status, strings.ToLower(problem.Code), problem.Detail
		}
		return proxyError(w, status, code, message)
	}
	defer s.proxyStats.begin(*key)()
	isResponse := r.URL.Path == "/v1/responses"
	threadID, turnID := strings.TrimSpace(r.Header.Get(responsesproxy.ThreadIDHeader)), strings.TrimSpace(r.Header.Get(responsesproxy.TurnIDHeader))
	if utf8.RuneCountInString(threadID) > 200 || utf8.RuneCountInString(turnID) > 200 {
		return proxyError(w, http.StatusBadRequest, "invalid_correlation_id", "Thread and turn identifiers must not exceed 200 characters.")
	}
	policy := s.proxyScrubPolicy
	if policy.TriggerBytes == 0 {
		policy = responsesproxy.DefaultScrubPolicy
	}
	if isResponse && r.ContentLength > responsesproxy.MaxInboundRequest {
		return proxyError(w, 413, "request_too_large", "Request body exceeds the broker limit.")
	}
	var releaseLarge func()
	if isResponse && (r.ContentLength < 0 || r.ContentLength >= int64(policy.TriggerBytes)) {
		s.proxyLargeOnce.Do(func() { s.proxyLargeGate = make(chan struct{}, 1) })
		select {
		case s.proxyLargeGate <- struct{}{}:
			releaseLarge = func() { <-s.proxyLargeGate }
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
	if isResponse {
		limit = responsesproxy.MaxInboundRequest
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return proxyError(w, 413, "request_too_large", "Request body exceeds the broker limit.")
		}
		return proxyError(w, 400, "invalid_request", "Request body could not be read.")
	}
	if isResponse {
		body = responsesproxy.PrepareNotice(body)
		result, scrubErr := responsesproxy.Scrub(body, policy)
		if scrubErr != nil {
			s.proxyStats.rejectScrub()
			if errors.Is(scrubErr, responsesproxy.ErrInvalidScrubRequest) && len(body) <= responsesproxy.MaxRequest {
				return proxyError(w, 400, "invalid_request", "Large Responses request is not valid JSON.")
			}
			return proxyError(w, 413, "request_too_large_after_scrub", "Request remains too large after historical content cleanup.")
		}
		body = result.Body
		if result.Changed {
			s.proxyStats.scrub(result.BeforeBytes, result.AfterBytes)
		}
		if len(body) > responsesproxy.MaxRequest {
			s.proxyStats.rejectScrub()
			return proxyError(w, 413, "request_too_large_after_scrub", "Request remains too large after historical content cleanup.")
		}
	}
	if releaseLarge != nil {
		releaseLarge()
		releaseLarge = nil
	}
	var requestMetadata struct {
		Model              string `json:"model"`
		ServiceTier        string `json:"service_tier"`
		PreviousResponseID string `json:"previous_response_id"`
		Reasoning          struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if isResponse {
		_ = json.Unmarshal(body, &requestMetadata)
	}
	input := broker.RouteRequest{SessionID: "proxy-" + core.NewID(), TurnID: core.NewID()}
	attempts := map[string]int{}
	previous := ""
	retries, failovers, attemptCount := 0, 0, 0
	for {
		selected, wait, err := s.App.Router.Route(r.Context(), key.ID, input)
		if err != nil {
			return proxyError(w, 503, "broker_unavailable", "Codex Broker unavailable.")
		}
		if wait != nil {
			w.Header().Set("Retry-After", strconv.Itoa(wait.RetryAfterSeconds))
			return proxyError(w, 429, "pool_exhausted", "All accounts unavailable · retry "+compactDuration(wait.RetryAfterSeconds)+".")
		}
		if attempts[selected.AccountID] > 0 && !(input.FailureKind == "auth" && input.FailedAccountID == selected.AccountID && attempts[selected.AccountID] == 1) {
			return proxyError(w, 502, "routing_failed", "Broker returned an unavailable account.")
		}
		if previous != "" && previous != selected.AccountID {
			s.proxyStats.failover()
			failovers++
		} else if previous != "" {
			retries++
		}
		previous = selected.AccountID
		attempts[selected.AccountID]++
		target := proxyUpstream(r.URL.Path)
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		attemptCount++
		upstreamStartedAt := time.Now().UnixMilli()
		response, err := responsesproxy.Forward(s.proxyClient, r, target, body, selected.AccessToken, selected.ChatGPTAccountID)
		if err != nil {
			return proxyError(w, 502, "upstream_unavailable", "Upstream request failed.")
		}
		responseHeadersAt := time.Now().UnixMilli()
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			response.Body.Close()
			return proxyError(w, 502, "upstream_redirect", "Upstream redirect rejected.")
		}
		kind := responsesproxy.FailureKind(response.StatusCode)
		if kind == "" || (kind == "auth" && attempts[selected.AccountID] >= 2) {
			if r.URL.Path == "/v1/responses" && response.StatusCode == http.StatusOK {
				notice := proxyNotice(*selected)
				if failovers > 0 {
					notice.Status = "↻ failover"
				} else if retries > 0 {
					notice.Status = "↻ retry"
				}
				observe := func(terminal responsesproxy.TerminalResponse) {
					ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
					defer cancel()
					var usageJSON any
					if len(terminal.UsageJSON) != 0 {
						usageJSON = string(terminal.UsageJSON)
					}
					_, persistErr := s.App.Store.DB.ExecContext(ctx, `INSERT INTO proxy_responses(
						proxy_request_id,account_id,client_key_id,thread_id,turn_id,endpoint,request_model,reasoning_effort,
						request_service_tier,previous_response_id,response_id,response_model,response_service_tier,
						terminal_status,upstream_attempts,upstream_http_status,request_started_at_ms,upstream_started_at_ms,
						response_headers_at_ms,first_response_event_at_ms,terminal_event_at_ms,input_tokens,cached_input_tokens,
						output_tokens,reasoning_output_tokens,total_tokens,usage_json
					) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
						core.NewID(), selected.AccountID, key.ID, emptyStringNil(threadID), emptyStringNil(turnID), r.URL.Path,
						emptyStringNil(requestMetadata.Model), emptyStringNil(requestMetadata.Reasoning.Effort),
						emptyStringNil(requestMetadata.ServiceTier), emptyStringNil(requestMetadata.PreviousResponseID),
						emptyStringNil(terminal.ID), emptyStringNil(terminal.Model), emptyStringNil(terminal.ServiceTier),
						terminal.Status, attemptCount, response.StatusCode, requestStartedAt, upstreamStartedAt, responseHeadersAt,
						terminal.FirstEventAtMS, terminal.TerminalEventAtMS, terminal.InputTokens, terminal.CachedInputTokens,
						terminal.OutputTokens, terminal.ReasoningOutputTokens, terminal.TotalTokens, usageJSON)
					if persistErr != nil && s.App.Logs != nil {
						s.App.Logs.Log("ERROR", "proxy.usage.persist", "Proxy response usage could not be stored.")
					}
				}
				if _, err := responsesproxy.CopyNoticeResponse(w, response, notice, nil, observe); err != nil {
					return proxyError(w, http.StatusBadGateway, "invalid_upstream_stream", "Upstream stream validation failed.")
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

func emptyStringNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func compactDuration(seconds int) string {
	minutes := max(0, (seconds+59)/60)
	if minutes >= 24*60 {
		return strconv.Itoa(minutes/(24*60)) + "d" + strconv.Itoa(minutes/60%24) + "h"
	}
	if minutes >= 60 {
		return strconv.Itoa(minutes/60) + "h" + strconv.Itoa(minutes%60) + "m"
	}
	return strconv.Itoa(minutes) + "m"
}

func (s *Server) responsesUpgrade(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.client(r); err != nil {
		status, code, message := http.StatusInternalServerError, "broker_unavailable", "Broker authentication unavailable."
		var problem *core.Error
		if errors.As(err, &problem) {
			status, code, message = problem.Status, strings.ToLower(problem.Code), problem.Detail
		}
		return proxyError(w, status, code, message)
	}
	w.Header().Set("Connection", "close")
	return proxyError(w, http.StatusUpgradeRequired, "http_required", "Use HTTP streaming for Responses API requests.")
}

func proxyNotice(selected broker.RouteLease) responsesproxy.Notice {
	text := func(value *int64) string {
		formatted := core.ISOTime(value)
		if formatted == nil {
			return ""
		}
		return *formatted
	}
	return responsesproxy.Notice{
		AccountID: selected.AccountID, AccountLabel: selected.AccountLabel,
		ShortRemainingPercent: selected.ShortRemainingPercent, WeeklyRemainingPercent: selected.WeeklyRemainingPercent,
		ShortResetsAt: text(selected.ShortResetsAtMS), WeeklyResetsAt: text(selected.WeeklyResetsAtMS),
	}
}

func proxyUpstream(path string) string {
	switch path {
	case "/transcribe":
		return responsesproxy.BackendURL + path
	case "/codex/thread/goal/get", "/codex/thread/goal/set":
		return responsesproxy.BackendURL + path
	default:
		return responsesproxy.UpstreamURL + strings.TrimPrefix(path, "/v1")
	}
}

func proxyError(w http.ResponseWriter, status int, code, message string) error {
	kind := "server_error"
	if status == 429 {
		kind = "rate_limit_error"
	} else if status == 401 || status == 403 {
		kind = "authentication_error"
	} else if status < 500 {
		kind = "invalid_request_error"
	}
	// A write failure must not trigger the normal problem response after headers.
	_ = writeJSON(w, status, map[string]any{"error": map[string]string{"message": message, "type": kind, "code": code}})
	return nil
}
