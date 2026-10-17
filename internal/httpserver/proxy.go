package httpserver

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/NotRllyRn/codex-broker/internal/broker"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/responsesproxy"
)

func (s *Server) responses(w http.ResponseWriter, r *http.Request) error {
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
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, responsesproxy.MaxRequest))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return proxyError(w, 413, "request_too_large", "Request body exceeds 64 MiB.")
		}
		return proxyError(w, 400, "invalid_request", "Request body could not be read.")
	}
	if r.URL.Path == "/v1/responses" {
		body = responsesproxy.PrepareNotice(body)
	}
	input := broker.RouteRequest{SessionID: "proxy-" + core.NewID(), TurnID: core.NewID()}
	attempts := map[string]int{}
	previous := ""
	retries, failovers := 0, 0
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
		response, err := responsesproxy.Forward(s.proxyClient, r, target, body, selected.AccessToken, selected.ChatGPTAccountID)
		if err != nil {
			return proxyError(w, 502, "upstream_unavailable", "Upstream request failed.")
		}
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
				if _, err := responsesproxy.CopyNoticeResponse(w, response, notice, nil); err != nil {
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
