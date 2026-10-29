package httpserver

import (
	"context"
	"net/http"

	"github.com/NotRllyRn/codex-broker/internal/broker"
	"github.com/NotRllyRn/codex-broker/internal/gateway"
	"github.com/NotRllyRn/codex-broker/internal/proxytelemetry"
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
	return &gateway.Client{ID: key.ID, Name: key.Name, Prefix: key.Prefix, StreamerMode: key.StreamerMode}, nil
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
		ChatGPTAccountID: selected.ChatGPTAccountID,
		ExpiresAtMS:      selected.ExpiresAtMS, UsageSnapshotAtMS: selected.UsageSnapshotAtMS,

		ShortRemainingPercent: selected.ShortRemainingPercent, WeeklyRemainingPercent: selected.WeeklyRemainingPercent,
		ShortResetsAtMS: selected.ShortResetsAtMS, WeeklyResetsAtMS: selected.WeeklyResetsAtMS,
	}, nil, nil
}

func (a brokerGatewayAuthority) Observe(ctx context.Context, value gateway.Observation) error {
	_, err := proxytelemetry.Record(ctx, a.server.App.Store, value)
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
		if s.App.Logs != nil {
			s.gatewayEngine.Log = s.App.Logs.Log
		}
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
