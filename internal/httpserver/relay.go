package httpserver

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/relayprotocol"
)

func (s *Server) requireRelay(r *http.Request) error {
	configured, supplied := s.App.Config.RelayKey, bearer(r)
	if configured == "" || subtle.ConstantTimeCompare([]byte(configured), []byte(supplied)) != 1 {
		return core.NewError("RELAY_UNAUTHORIZED", "Relay authentication failed", http.StatusUnauthorized)
	}
	if !s.App.Ready {
		return core.NewError("BROKER_NOT_READY", "Codex Broker is unavailable", http.StatusServiceUnavailable)
	}
	return nil
}

func (s *Server) relaySnapshot(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireRelay(r); err != nil {
		return err
	}
	relayID := strings.TrimSpace(r.URL.Query().Get("relay_id"))
	if !validRelayID(relayID) {
		return core.NewError("RELAY_ID_INVALID", "Relay ID is invalid", 422)
	}
	snapshot, err := s.App.RelaySnapshot(r.Context(), relayID)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, snapshot)
}

func (s *Server) relayRoute(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireRelay(r); err != nil {
		return err
	}
	var body relayprotocol.RouteRequest
	if err := decodeJSON(r, &body); err != nil {
		return err
	}
	request := body.Request
	validFailure := request.FailureKind == "" || request.FailureKind == "quota" || request.FailureKind == "auth" || request.FailureKind == "rate_limit"
	if body.ClientID == "" || utf8.RuneCountInString(body.ClientID) > 100 || utf8.RuneCountInString(request.SessionID) < 1 || utf8.RuneCountInString(request.SessionID) > 200 || utf8.RuneCountInString(request.TurnID) < 1 || utf8.RuneCountInString(request.TurnID) > 200 || utf8.RuneCountInString(request.PreferredAccountID) > 100 || utf8.RuneCountInString(request.FailedAccountID) > 100 || (request.FailedAccountID == "") != (request.FailureKind == "") || !validFailure {
		return core.NewError("RELAY_ROUTE_INVALID", "Relay route request is invalid", 422)
	}
	lease, wait, err := s.App.RelayRoute(r.Context(), body.ClientID, body.Request)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, relayprotocol.RouteResponse{Lease: lease, Wait: wait})
}

func (s *Server) relayEvents(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireRelay(r); err != nil {
		return err
	}
	s.relayEventsMu.Lock()
	defer s.relayEventsMu.Unlock()
	var body relayprotocol.EventBatch
	if err := decodeJSON(r, &body); err != nil {
		return err
	}
	if len(body.Events) > 100 {
		return core.NewError("RELAY_EVENTS_INVALID", "Relay event batch is too large", 422)
	}
	for _, event := range body.Events {
		if event.ID == "" || !validRelayID(event.RelayID) || event.OccurredAtMS <= 0 {
			return core.NewError("RELAY_EVENT_INVALID", "Relay event is invalid", 422)
		}
		var exists bool
		if err := s.App.Store.DB.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM relay_events WHERE event_id=?)", event.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		var processErr error
		switch event.Kind {
		case "activation":
			processErr = s.App.Router.ActivateRelayReserve(r.Context(), event.RelayID, event.AccountID, event.PermitEpoch, event.OccurredAtMS)
		case "failure":
			if event.FailureKind != "auth" && event.FailureKind != "quota" && event.FailureKind != "rate_limit" {
				processErr = core.NewError("RELAY_EVENT_INVALID", "Relay failure event is invalid", 422)
			} else {
				processErr = s.App.Router.ReportRelayFailure(r.Context(), event.ClientID, event.AccountID, event.FailureKind)
			}
		case "telemetry":
			if event.Observation == nil {
				processErr = core.NewError("RELAY_EVENT_INVALID", "Relay telemetry event is invalid", 422)
			} else {
				processErr = (brokerGatewayAuthority{s}).Observe(r.Context(), *event.Observation)
			}
		default:
			processErr = core.NewError("RELAY_EVENT_INVALID", "Relay event type is invalid", 422)
		}
		if processErr != nil {
			return processErr
		}
		if _, err := s.App.Store.DB.ExecContext(r.Context(), "INSERT OR IGNORE INTO relay_events VALUES(?,?,?,?,?)", event.ID, event.RelayID, event.Kind, event.OccurredAtMS, time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	return writeJSON(w, 200, map[string]any{"accepted": len(body.Events)})
}

func (s *Server) relayHeartbeat(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireRelay(r); err != nil {
		return err
	}
	var body relayprotocol.Heartbeat
	if err := decodeJSON(r, &body); err != nil {
		return err
	}
	if !validRelayID(body.RelayID) || body.ProtocolVersion != relayprotocol.Version {
		return core.NewError("RELAY_PROTOCOL_INCOMPATIBLE", "Relay protocol is incompatible", 409)
	}
	ready := 0
	if body.Ready {
		ready = 1
	}
	_, err := s.App.Store.DB.ExecContext(r.Context(), `INSERT INTO relay_heartbeats VALUES(?,?,?,?,?,?) ON CONFLICT(relay_id) DO UPDATE SET protocol_version=excluded.protocol_version,snapshot_id=excluded.snapshot_id,cache_generated_at_ms=excluded.cache_generated_at_ms,ready=excluded.ready,last_seen_at_ms=excluded.last_seen_at_ms`, body.RelayID, body.ProtocolVersion, emptyStringNil(body.SnapshotID), nullablePositive(body.CacheGeneratedAtMS), ready, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]string{"status": "ok"})
}

func validRelayID(value string) bool {
	return value != "" && len(value) <= 80 && strings.IndexFunc(value, func(item rune) bool {
		return item != '.' && item != '_' && item != '-' && (item < '0' || item > '9') && (item < 'A' || item > 'Z') && (item < 'a' || item > 'z')
	}) < 0
}

func nullablePositive(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}
