package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/gateway"
	"github.com/NotRllyRn/codex-broker/internal/relayprotocol"
)

const testRelayKey = "cbr_12345678901234567890123456789012"

func relayRequest(server *Server, method, path, body, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.middleware(server.mux).ServeHTTP(response, request)
	return response
}

func TestRelayControlPlaneSnapshotRouteAndEvents(t *testing.T) {
	server, key := proxyFixture(t, 2)
	server.App.Config.RelayKey = testRelayKey
	server.App.Config.WindowPulseEnabled = true
	server.App.Service.Config.WindowPulseEnabled = true
	server.App.Ready = true
	if _, err := server.App.Store.DB.Exec(`INSERT INTO window_pulse_state(account_id,last_attempt_at_ms,cycle_state,weekly_started_at_ms,idle_observation_count) VALUES('account0',1,'IN_CYCLE',1,0),('account1',1,'HELD',NULL,2)`); err != nil {
		t.Fatal(err)
	}
	unauthorized := relayRequest(server, http.MethodGet, "/api/private/v1/relay/snapshot?relay_id=edge", "", "wrong")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized=%d", unauthorized.Code)
	}

	response := relayRequest(server, http.MethodGet, "/api/private/v1/relay/snapshot?relay_id=edge", "", testRelayKey)
	if response.Code != http.StatusOK {
		t.Fatalf("snapshot=%d %s", response.Code, response.Body.String())
	}
	var snapshot relayprotocol.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ProtocolVersion != relayprotocol.Version || snapshot.RelayID != "edge" || len(snapshot.Clients) != 1 || len(snapshot.Leases) != 2 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if strings.Contains(response.Body.String(), "secret-refresh") {
		t.Fatal("snapshot exposed a refresh token")
	}
	var reserve relayprotocol.Lease
	for _, lease := range snapshot.Leases {
		if lease.Reserve {
			reserve = lease
		}
	}
	if reserve.AccountID != "account1" || reserve.PermitEpoch == "" {
		t.Fatalf("reserve=%+v", reserve)
	}

	routeBody, _ := json.Marshal(relayprotocol.RouteRequest{ClientID: key.ID, Request: gateway.RouteRequest{SessionID: "s", TurnID: "t"}})
	routed := relayRequest(server, http.MethodPost, "/api/private/v1/relay/route", string(routeBody), testRelayKey)
	if routed.Code != http.StatusOK {
		t.Fatalf("route=%d %s", routed.Code, routed.Body.String())
	}

	now := time.Now().UnixMilli()
	observation := gateway.Observation{RequestID: "relay-response", AccountID: "account0", ClientID: key.ID, Endpoint: "/v1/responses", TerminalStatus: "completed", UpstreamAttempts: 1, UpstreamHTTPStatus: 200, RequestStartedAtMS: now, UpstreamStartedAtMS: now, ResponseHeadersAtMS: now, FirstResponseEventAtMS: now, TerminalEventAtMS: now}
	events, _ := json.Marshal(relayprotocol.EventBatch{Events: []relayprotocol.Event{{ID: "event-telemetry", RelayID: "edge", Kind: "telemetry", OccurredAtMS: now, Observation: &observation}}})
	for index := 0; index < 2; index++ {
		accepted := relayRequest(server, http.MethodPost, "/api/private/v1/relay/events", string(events), testRelayKey)
		if accepted.Code != 200 {
			t.Fatalf("events=%d %s", accepted.Code, accepted.Body.String())
		}
	}
	var stored int
	if err := server.App.Store.DB.QueryRow("SELECT count(*) FROM proxy_responses WHERE proxy_request_id='relay-response'").Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored=%d err=%v", stored, err)
	}
	if err := server.App.Store.DB.QueryRow("SELECT count(*) FROM relay_events WHERE event_id='event-telemetry'").Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("events stored=%d err=%v", stored, err)
	}
	activation, _ := json.Marshal(relayprotocol.EventBatch{Events: []relayprotocol.Event{{ID: "event-activation", RelayID: "edge", Kind: "activation", AccountID: reserve.AccountID, PermitEpoch: reserve.PermitEpoch, OccurredAtMS: now}}})
	activated := relayRequest(server, http.MethodPost, "/api/private/v1/relay/events", string(activation), testRelayKey)
	if activated.Code != http.StatusOK {
		t.Fatalf("activation=%d %s", activated.Code, activated.Body.String())
	}
	var cycle string
	if err := server.App.Store.DB.QueryRow("SELECT cycle_state FROM window_pulse_state WHERE account_id='account1'").Scan(&cycle); err != nil || cycle != "IN_CYCLE" {
		t.Fatalf("cycle=%q err=%v", cycle, err)
	}
	if err := server.App.Store.DB.QueryRow("SELECT count(*) FROM relay_reservations WHERE relay_id='edge'").Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("reservations=%d err=%v", stored, err)
	}
}

func TestRelayHeartbeatRejectsProtocolMismatch(t *testing.T) {
	server, _ := proxyFixture(t, 1)
	server.App.Config.RelayKey = testRelayKey
	server.App.Ready = true
	body, _ := json.Marshal(relayprotocol.Heartbeat{RelayID: "edge", ProtocolVersion: relayprotocol.Version + 1})
	response := relayRequest(server, http.MethodPost, "/api/private/v1/relay/heartbeat", string(body), testRelayKey)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRelayRouteValidatesInputAndRevocation(t *testing.T) {
	server, key := proxyFixture(t, 1)
	server.App.Config.RelayKey = testRelayKey
	server.App.Ready = true
	invalid := relayRequest(server, http.MethodPost, "/api/private/v1/relay/route", `{}`, testRelayKey)
	if invalid.Code != 422 {
		t.Fatalf("invalid=%d %s", invalid.Code, invalid.Body.String())
	}
	if _, err := server.App.ClientKeys.Revoke(t.Context(), key.ID); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(relayprotocol.RouteRequest{ClientID: key.ID, Request: gateway.RouteRequest{SessionID: "s", TurnID: "t"}})
	revoked := relayRequest(server, http.MethodPost, "/api/private/v1/relay/route", string(body), testRelayKey)
	if revoked.Code != http.StatusUnauthorized {
		t.Fatalf("revoked=%d %s", revoked.Code, revoked.Body.String())
	}
}
