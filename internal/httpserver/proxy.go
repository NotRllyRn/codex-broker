package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/NotRllyRn/codex-broker/internal/broker"
	"github.com/NotRllyRn/codex-broker/internal/gateway"
)

type brokerGatewayAuthority struct{ server *Server }

func (a brokerGatewayAuthority) Authenticate(request *http.Request, token string) (*gateway.Client, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+token)
	key, err := a.server.client(clone)
	if err != nil {
		return nil, err
	}
	return &gateway.Client{ID: key.ID, Name: key.Name, Prefix: key.Prefix}, nil
}

func (a brokerGatewayAuthority) Route(ctx context.Context, keyID string, input gateway.RouteRequest) (*gateway.Lease, *gateway.PoolWait, error) {
	selected, wait, err := a.server.App.Router.Route(ctx, keyID, broker.RouteRequest{
		SessionID: input.SessionID, TurnID: input.TurnID, PreferredAccountID: input.PreferredAccountID,
		FailedAccountID: input.FailedAccountID, FailureKind: input.FailureKind,
	})
	if err != nil {
		return nil, nil, err
	}
	if wait != nil {
		return nil, &gateway.PoolWait{NextRetryAtMS: wait.NextRetryAtMS, RetryAfterSeconds: wait.RetryAfterSeconds}, nil
	}
	return &gateway.Lease{
		AccountID: selected.AccountID, AccountLabel: selected.AccountLabel, AccessToken: selected.AccessToken,
		ChatGPTAccountID: selected.ChatGPTAccountID, ExpiresAtMS: selected.ExpiresAtMS,
		ShortRemainingPercent: selected.ShortRemainingPercent, WeeklyRemainingPercent: selected.WeeklyRemainingPercent,
		ShortResetsAtMS: selected.ShortResetsAtMS, WeeklyResetsAtMS: selected.WeeklyResetsAtMS,
	}, nil, nil
}

func (a brokerGatewayAuthority) Observe(ctx context.Context, value gateway.Observation) error {
	var usageJSON any
	if len(value.UsageJSON) != 0 {
		usageJSON = string(value.UsageJSON)
	}
	var clientID any
	if value.ClientID != "" {
		var exists bool
		if err := a.server.App.Store.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM client_api_keys WHERE key_id=?)", value.ClientID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			clientID = value.ClientID
			_, _ = a.server.App.Store.DB.ExecContext(ctx, "UPDATE client_api_keys SET last_used_at_ms=? WHERE key_id=?", value.TerminalEventAtMS, value.ClientID)
		}
	}
	var accountID string
	if err := a.server.App.Store.DB.QueryRowContext(ctx, "SELECT account_id FROM accounts WHERE public_token=?", value.AccountID).Scan(&accountID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	_, err := a.server.App.Store.DB.ExecContext(ctx, `INSERT OR IGNORE INTO proxy_responses(
		proxy_request_id,account_id,client_key_id,thread_id,turn_id,endpoint,request_model,reasoning_effort,
		request_service_tier,previous_response_id,response_id,response_model,response_service_tier,
		terminal_status,upstream_attempts,upstream_http_status,request_started_at_ms,upstream_started_at_ms,
		response_headers_at_ms,first_response_event_at_ms,terminal_event_at_ms,input_tokens,cached_input_tokens,
		output_tokens,reasoning_output_tokens,total_tokens,usage_json
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		value.RequestID, accountID, clientID, emptyStringNil(value.ThreadID), emptyStringNil(value.TurnID), value.Endpoint,
		emptyStringNil(value.RequestModel), emptyStringNil(value.ReasoningEffort), emptyStringNil(value.RequestServiceTier),
		emptyStringNil(value.PreviousResponseID), emptyStringNil(value.ResponseID), emptyStringNil(value.ResponseModel),
		emptyStringNil(value.ResponseServiceTier), value.TerminalStatus, value.UpstreamAttempts, value.UpstreamHTTPStatus,
		value.RequestStartedAtMS, value.UpstreamStartedAtMS, value.ResponseHeadersAtMS, value.FirstResponseEventAtMS,
		value.TerminalEventAtMS, value.InputTokens, value.CachedInputTokens, value.OutputTokens, value.ReasoningTokens,
		value.TotalTokens, usageJSON)
	if err != nil && a.server.App.Logs != nil {
		a.server.App.Logs.Log("ERROR", "proxy.usage.persist", "Proxy response usage could not be stored.")
	}
	return err
}

func (a brokerGatewayAuthority) ReportFailure(ctx context.Context, clientID, accountID, kind string) error {
	return a.server.App.Router.ReportRelayFailure(ctx, clientID, accountID, kind)
}

func (a brokerGatewayAuthority) Activate(context.Context, gateway.Lease) error { return nil }

func (s *Server) proxyGateway() *gateway.Engine {
	s.gatewayOnce.Do(func() {
		s.gatewayEngine = gateway.New(brokerGatewayAuthority{s}, &s.proxyStats)
		s.gatewayEngine.Client = s.proxyClient
		s.gatewayEngine.ScrubPolicy = s.proxyScrubPolicy
	})
	return s.gatewayEngine
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request) error {
	return s.proxyGateway().Serve(w, r)
}
func (s *Server) responsesUpgrade(w http.ResponseWriter, r *http.Request) error {
	return s.proxyGateway().Upgrade(w, r)
}

func emptyStringNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func compactDuration(seconds int) string { return gateway.CompactDuration(seconds) }
