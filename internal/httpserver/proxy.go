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
			return proxyError(w, 413, "request_too_large", "Request body exceeds 32 MiB.")
		}
		return proxyError(w, 400, "invalid_request", "Request body could not be read.")
	}
	input := broker.RouteRequest{SessionID: "proxy-" + core.NewID(), TurnID: core.NewID()}
	attempts := map[string]int{}
	previous := ""
	for {
		selected, wait, err := s.App.Router.Route(r.Context(), key.ID, input)
		if err != nil {
			return proxyError(w, 503, "broker_unavailable", "No broker account is available.")
		}
		if wait != nil {
			w.Header().Set("Retry-After", strconv.Itoa(wait.RetryAfterSeconds))
			return proxyError(w, 429, "pool_exhausted", "All broker accounts are temporarily unavailable.")
		}
		if attempts[selected.AccountID] > 0 && !(input.FailureKind == "auth" && input.FailedAccountID == selected.AccountID && attempts[selected.AccountID] == 1) {
			return proxyError(w, 502, "routing_failed", "Broker returned an unavailable account.")
		}
		if previous != "" && previous != selected.AccountID {
			s.proxyStats.failover()
		}
		previous = selected.AccountID
		attempts[selected.AccountID]++
		target := responsesproxy.UpstreamURL + strings.TrimPrefix(r.URL.Path, "/v1")
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
			responsesproxy.CopyResponse(w, response)
			return nil
		}
		response.Body.Close()
		input.PreferredAccountID, input.FailedAccountID, input.FailureKind = selected.AccountID, selected.AccountID, kind
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
