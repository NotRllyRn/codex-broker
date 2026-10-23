package relay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/NotRllyRn/codex-broker/internal/gateway"
	"github.com/NotRllyRn/codex-broker/internal/relayprotocol"
	"github.com/NotRllyRn/codex-broker/internal/responsesproxy"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func relayFixture(t *testing.T) (*Relay, string) {
	t.Helper()
	token := "cbk_test-client-secret"
	digest := sha256.Sum256([]byte(token))
	now := time.Now().UnixMilli()
	short, weekly := int64(80), int64(90)
	r := &Relay{
		config: config.Config{RelayID: "relay-test", RelayKey: "cbr_12345678901234567890123456789012", RelayCacheFile: filepath.Join(t.TempDir(), "cache.enc"), RelayMaxStaleSeconds: 3600},
		state: state{
			Exclusions: map[string]int64{},
			Snapshot: relayprotocol.Snapshot{
				ProtocolVersion: relayprotocol.Version,
				RelayID:         "relay-test", SnapshotID: "snapshot", GeneratedAtMS: now,
				Clients: []relayprotocol.Client{{ID: "key", Name: "test", Prefix: "cbk_test", Digest: digest[:]}, {ID: "other"}},
				Leases: []relayprotocol.Lease{{Lease: gateway.Lease{
					AccountID: "active", AccountLabel: "Active", AccessToken: "access-secret", ChatGPTAccountID: "chatgpt",
					ExpiresAtMS: now + time.Hour.Milliseconds(), ShortRemainingPercent: &short, WeeklyRemainingPercent: &weekly,
					WeeklyResetsAtMS: pointer(now + 6*24*time.Hour.Milliseconds()),
				}, CycleState: "IN_CYCLE"}},
			},
		},
	}
	return r, token
}

func pointer(value int64) *int64 { return &value }

func TestEncryptedCacheRoundTripDoesNotExposeSecrets(t *testing.T) {
	r, token := relayFixture(t)
	r.mu.Lock()
	err := r.saveLocked()
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(r.config.RelayCacheFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte("access-secret")) || bytes.Contains(content, []byte(token)) {
		t.Fatal("encrypted cache exposed credentials")
	}
	loaded := &Relay{config: r.config, state: state{Exclusions: map[string]int64{}}}
	if err := loaded.load(); err != nil {
		t.Fatal(err)
	}
	if got := loaded.state.Snapshot.Leases[0].AccessToken; got != "access-secret" {
		t.Fatalf("access token=%q", got)
	}
}

func TestRelayAuthenticationRejectsInvalidAndStaleCache(t *testing.T) {
	r, token := relayFixture(t)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := r.Authenticate(request, token); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Authenticate(request, "cbk_wrong"); err == nil {
		t.Fatal("invalid key authenticated")
	}
	r.state.Snapshot.GeneratedAtMS = time.Now().Add(-2 * time.Hour).UnixMilli()
	if _, err := r.Authenticate(request, token); err == nil {
		t.Fatal("stale cache authenticated")
	}
	r.state.Snapshot.GeneratedAtMS = time.Now().Add(10 * time.Minute).UnixMilli()
	if _, err := r.Authenticate(request, token); err == nil {
		t.Fatal("future-dated cache authenticated")
	}
}

func TestRelayRemainsReadyToReportKnownWait(t *testing.T) {
	r, _ := relayFixture(t)
	zero := int64(0)
	reset := time.Now().Add(10 * time.Minute).UnixMilli()
	r.state.Snapshot.Leases[0].ShortRemainingPercent = &zero
	r.state.Snapshot.Leases[0].ShortResetsAtMS = &reset
	if !r.Ready() {
		t.Fatal("unexpired exhausted lease should keep relay ready")
	}
	lease, wait, err := r.localRoute("key", gateway.RouteRequest{})
	if err != nil || lease != nil || wait == nil || wait.NextRetryAtMS != reset {
		t.Fatalf("lease=%+v wait=%+v err=%v", lease, wait, err)
	}
}

func TestRelayOfflineFailureAndEmergencyReserve(t *testing.T) {
	r, _ := relayFixture(t)
	now := time.Now().UnixMilli()
	zero := int64(0)
	hundred := int64(100)
	r.state.Snapshot.Leases[0].ShortRemainingPercent = &zero
	r.state.Snapshot.Leases[0].WeeklyRemainingPercent = &zero
	r.state.Snapshot.Leases = append(r.state.Snapshot.Leases, relayprotocol.Lease{Lease: gateway.Lease{AccountID: "reserve", AccountLabel: "Reserve", AccessToken: "reserve-token", ChatGPTAccountID: "reserve-chatgpt", ExpiresAtMS: now + time.Hour.Milliseconds(), ShortRemainingPercent: &hundred, WeeklyRemainingPercent: &hundred}, CycleState: "HELD", Reserve: true, PermitEpoch: "permit", PermitExpiresAtMS: now + time.Hour.Milliseconds()})
	lease, wait, err := r.localRoute("key", gateway.RouteRequest{})
	if err != nil || wait != nil || lease.AccountID != "reserve" {
		t.Fatalf("lease=%+v wait=%+v err=%v", lease, wait, err)
	}
	if err := r.Activate(context.Background(), *lease); err != nil {
		t.Fatal(err)
	}
	if len(r.state.Outbox) != 1 || r.state.Outbox[0].Kind != "activation" || r.state.Outbox[0].PermitEpoch != "permit" {
		t.Fatalf("outbox=%+v", r.state.Outbox)
	}
	if r.state.Snapshot.Leases[1].Reserve {
		t.Fatal("reserve remained held")
	}
}

func TestRelayOfflineQuotaFailureBlocksAllClients(t *testing.T) {
	r, _ := relayFixture(t)
	now := time.Now().UnixMilli()
	remaining := int64(90)
	r.state.Snapshot.Leases = append(r.state.Snapshot.Leases, relayprotocol.Lease{Lease: gateway.Lease{AccountID: "second", AccessToken: "token", ChatGPTAccountID: "chat", ExpiresAtMS: now + time.Hour.Milliseconds(), WeeklyRemainingPercent: &remaining, WeeklyResetsAtMS: pointer(now + 5*24*time.Hour.Milliseconds())}, CycleState: "IN_CYCLE"})
	lease, _, err := r.localRoute("key", gateway.RouteRequest{FailedAccountID: "second", FailureKind: "quota", PreferredAccountID: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.AccountID != "active" {
		t.Fatalf("selected=%s", lease.AccountID)
	}
	other, _, err := r.localRoute("other", gateway.RouteRequest{PreferredAccountID: "second"})
	if err != nil || other.AccountID != "active" {
		t.Fatalf("other=%+v err=%v", other, err)
	}
}

func TestRelayQuotaBlockSurvivesCacheAndUnlocksOnFreshUsageOrReset(t *testing.T) {
	for _, unlock := range []string{"usage", "reset"} {
		t.Run(unlock, func(t *testing.T) {
			r, _ := relayFixture(t)
			now := time.Now().UnixMilli()
			r.state.Snapshot.Leases[0].ShortResetsAtMS = pointer(now + time.Hour.Milliseconds())
			if err := r.ReportFailure(context.Background(), "key", "active", "quota"); err != nil {
				t.Fatal(err)
			}
			loaded := &Relay{config: r.config}
			if err := loaded.load(); err != nil {
				t.Fatal(err)
			}
			lease, wait, err := loaded.localRoute("other", gateway.RouteRequest{})
			if err != nil || lease != nil || wait == nil || wait.NextRetryAtMS != *r.state.Snapshot.Leases[0].ShortResetsAtMS {
				t.Fatalf("lease=%+v wait=%+v err=%v", lease, wait, err)
			}
			if unlock == "usage" {
				loaded.state.Snapshot.Leases[0].UsageSnapshotAtMS = now + 1000
			} else {
				block := loaded.state.QuotaBlocks["active"]
				block.RetryAtMS = now - 1
				loaded.state.QuotaBlocks["active"] = block
			}
			lease, wait, err = loaded.localRoute("other", gateway.RouteRequest{})
			if err != nil || lease == nil || wait != nil {
				t.Fatalf("lease=%+v wait=%+v err=%v", lease, wait, err)
			}
		})
	}
}

func TestRelayReportsQuotaBeforeQueuedTelemetry(t *testing.T) {
	r, _ := relayFixture(t)
	for range 101 {
		r.state.Outbox = append(r.state.Outbox, relayprotocol.Event{ID: "old", Kind: "telemetry"})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var batch relayprotocol.EventBatch
		if err := json.NewDecoder(request.Body).Decode(&batch); err != nil {
			t.Error(err)
		}
		if len(batch.Events) != 1 || batch.Events[0].Kind != "failure" || batch.Events[0].FailureKind != "quota" {
			t.Errorf("batch=%+v", batch)
		}
		_, _ = io.WriteString(w, "{}")
	}))
	defer server.Close()
	r.config.RelayBrokerURL, r.client, r.online = server.URL, server.Client(), true
	if err := r.ReportFailure(context.Background(), "key", "active", "quota"); err != nil {
		t.Fatal(err)
	}
	if len(r.state.Outbox) != 102 {
		t.Fatalf("outbox=%d", len(r.state.Outbox))
	}
}

func TestRelayRoutesConcurrently(t *testing.T) {
	r, _ := relayFixture(t)
	var group sync.WaitGroup
	failures := make(chan error, 100)
	for index := 0; index < 100; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			lease, _, err := r.localRoute("key", gateway.RouteRequest{})
			if err == nil && lease.AccountID != "active" {
				err = io.ErrUnexpectedEOF
			}
			failures <- err
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestOnlineLeaseIsAddedToOfflineSnapshot(t *testing.T) {
	r, _ := relayFixture(t)
	lease := gateway.Lease{AccountID: "new", AccessToken: "token", ChatGPTAccountID: "chat", ExpiresAtMS: time.Now().Add(time.Hour).UnixMilli()}
	if err := r.updateLease(lease); err != nil {
		t.Fatal(err)
	}
	if len(r.state.Snapshot.Leases) != 2 || r.state.Snapshot.Leases[1].AccountID != "new" || r.state.Snapshot.Leases[1].CycleState != "IN_CYCLE" {
		t.Fatalf("leases=%+v", r.state.Snapshot.Leases)
	}
}

func TestCriticalEventsDisplaceTelemetryAtOutboxLimit(t *testing.T) {
	r, _ := relayFixture(t)
	r.state.Outbox = make([]relayprotocol.Event, outboxLimit)
	for index := range r.state.Outbox {
		r.state.Outbox[index] = relayprotocol.Event{Kind: "telemetry"}
	}
	if err := r.queueLocked(relayprotocol.Event{Kind: "activation"}, true); err != nil {
		t.Fatal(err)
	}
	if len(r.state.Outbox) != outboxLimit || r.state.Outbox[len(r.state.Outbox)-1].Kind != "activation" {
		t.Fatalf("outbox length=%d last=%q", len(r.state.Outbox), r.state.Outbox[len(r.state.Outbox)-1].Kind)
	}
	if err := r.queueLocked(relayprotocol.Event{Kind: "telemetry"}, false); err == nil {
		t.Fatal("telemetry exceeded bounded outbox")
	}
}

func TestRelaySynchronizesSnapshotAndDurableOutbox(t *testing.T) {
	r, _ := relayFixture(t)
	r.state.Outbox = []relayprotocol.Event{{ID: "queued", RelayID: r.config.RelayID, Kind: "failure", AccountID: "active", ClientID: "key", FailureKind: "quota", OccurredAtMS: time.Now().UnixMilli()}}
	digest := sha256.Sum256([]byte("cbk_synced"))
	seenEvents, seenHeartbeat := false, false
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+r.config.RelayKey {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch request.URL.Path {
		case "/api/private/v1/relay/events":
			seenEvents = true
			write(w, 200, map[string]any{"accepted": 1})
		case "/api/private/v1/relay/snapshot":
			now := time.Now().UnixMilli()
			remaining := int64(50)
			write(w, 200, relayprotocol.Snapshot{ProtocolVersion: relayprotocol.Version, RelayID: r.config.RelayID, SnapshotID: "fresh", GeneratedAtMS: now, Clients: []relayprotocol.Client{{ID: "synced", Digest: digest[:]}}, Leases: []relayprotocol.Lease{{Lease: gateway.Lease{AccountID: "fresh-account", AccessToken: "fresh-token", ChatGPTAccountID: "chat", ExpiresAtMS: now + time.Hour.Milliseconds(), WeeklyRemainingPercent: &remaining}, CycleState: "IN_CYCLE"}}})
		case "/api/private/v1/relay/heartbeat":
			seenHeartbeat = true
			write(w, 200, map[string]string{"status": "ok"})
		default:
			http.NotFound(w, request)
		}
	}))
	defer upstream.Close()
	r.client = upstream.Client()
	r.config.RelayBrokerURL = upstream.URL
	r.sync(context.Background())
	if !seenEvents || !seenHeartbeat || !r.online || !r.Ready() || len(r.state.Outbox) != 0 || r.state.Snapshot.SnapshotID != "fresh" {
		t.Fatalf("events=%v heartbeat=%v online=%v ready=%v state=%+v", seenEvents, seenHeartbeat, r.online, r.Ready(), r.state)
	}
}

func TestRelayServerUsesSharedGatewayForEveryRoute(t *testing.T) {
	r, token := relayFixture(t)
	server := NewServer(r)
	typed := server
	tests := []struct{ method, path, target string }{{"GET", "/v1/models?x=1", responsesproxy.UpstreamURL + "/models?x=1"}, {"GET", "/models", responsesproxy.UpstreamURL + "/models"}, {"POST", "/v1/images/generations", responsesproxy.UpstreamURL + "/images/generations"}, {"POST", "/images/edits", responsesproxy.UpstreamURL + "/images/edits"}, {"POST", "/transcribe", responsesproxy.BackendURL + "/transcribe"}, {"POST", "/codex/thread/goal/set", responsesproxy.BackendURL + "/codex/thread/goal/set"}, {"POST", "/thread/goal/get", responsesproxy.UpstreamURL + "/thread/goal/get"}, {"POST", "/v1/responses/compact", responsesproxy.UpstreamURL + "/responses/compact"}, {"POST", "/v1/responses", responsesproxy.UpstreamURL + "/responses"}}
	for _, test := range tests {
		t.Run(test.method+test.path, func(t *testing.T) {
			var target string
			typed.engine.Client = &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
				target = req.URL.String()
				body := "{}"
				header := http.Header{}
				if test.path == "/v1/responses" {
					body = "data: {\"type\":\"response.created\",\"response\":{}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n"
					header.Set("Content-Type", "text/event-stream")
				}
				return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			body := "{}"
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != 200 || target != test.target {
				t.Fatalf("status=%d target=%q body=%s", response.Code, target, response.Body.String())
			}
		})
	}
}

func TestOfflineRelayScopes(t *testing.T) {
	r, _ := relayFixture(t)
	now := time.Now().UnixMilli()
	r.state.Snapshot.Leases = append(r.state.Snapshot.Leases, relayprotocol.Lease{Lease: gateway.Lease{AccountID: "reserve", ExpiresAtMS: now + time.Hour.Milliseconds()}, Reserve: true, PermitEpoch: "permit", PermitExpiresAtMS: now + time.Hour.Milliseconds()})
	client := &r.state.Snapshot.Clients[0]
	client.Restricted = true
	client.AllowedAccountIDs = []string{"reserve"}
	lease, _, err := r.localRoute("key", gateway.RouteRequest{PreferredAccountID: "active"})
	if err != nil || lease == nil || lease.AccountID != "reserve" {
		t.Fatalf("reserve selection: %+v %v", lease, err)
	}
	if lease, _, err := r.localRoute("key", gateway.RouteRequest{FailedAccountID: "active", FailureKind: "quota"}); err == nil || lease != nil {
		t.Fatalf("unauthorized failed account: %+v %v", lease, err)
	}
	client.AllowedAccountIDs = nil
	if lease, _, err := r.localRoute("key", gateway.RouteRequest{}); err == nil || lease != nil {
		t.Fatalf("empty scoped pool escaped: %+v %v", lease, err)
	}
	if lease, _, err := r.localRoute("other", gateway.RouteRequest{}); err != nil || lease == nil || lease.AccountID != "active" {
		t.Fatalf("unrestricted routing changed: %+v %v", lease, err)
	}
	r.state.Snapshot.ProtocolVersion--
	if r.Ready() {
		t.Fatal("older snapshot accepted")
	}
}
