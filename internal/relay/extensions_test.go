package relay

import (
	"encoding/json"
	"fmt"
	"github.com/NotRllyRn/codex-broker/internal/relayprotocol"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NotRllyRn/codex-broker/extensions/requestsize"
	"github.com/NotRllyRn/codex-broker/extensions/settings"
	"github.com/NotRllyRn/codex-broker/internal/proxyextension"
)

func TestRelayExtensionSnapshotSurvivesOfflineCache(t *testing.T) {
	relay, token := relayFixture(t)
	relay.state.Snapshot.ExtensionSettings = proxyextension.Configuration{"example": settings.Values{"label": "configured relay"}}
	relay.state.Snapshot.DisabledExtensions = []string{proxyextension.UsageID, requestsize.ID}
	if err := relay.saveLocked(); err != nil {
		t.Fatal(err)
	}
	loaded := &Relay{config: relay.config, state: state{Exclusions: map[string]int64{}}}
	if err := loaded.load(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(loaded)
	server.engine.ScrubPolicy = requestsize.ScrubPolicy{TriggerBytes: 2, TargetBytes: 1}
	def, err := proxyextension.Compile("example", `export const settings={fields:[{key:'label',type:'text',label:'Label',default:'default'}]};export default p=>p.on('response',e=>({headers:{...e.headers,'X-Example':[p.getSettings().label]}}))`)
	if err != nil {
		t.Fatal(err)
	}
	server.engine.Extensions = proxyextension.New(def)
	server.engine.Client = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		body := `data: {"type":"response.output_item.added","item":{"id":"answer","type":"message","role":"assistant","phase":"final_answer"}}` + "\n\n" +
			`data: {"type":"response.output_text.delta","item_id":"answer","delta":"Hello"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"id":"response","usage":{"total_tokens":8}}}` + "\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"input":[]}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("X-Example") != "configured relay" || strings.Contains(w.Body.String(), "🔨") || !strings.Contains(w.Body.String(), "Hello") {
		t.Fatal(w.Code, w.Body, w.Header())
	}
	found := false
	for _, event := range loaded.state.Outbox {
		if event.Observation != nil && event.Observation.TotalTokens != nil && *event.Observation.TotalTokens == 8 {
			found = true
		}
	}
	if !found {
		t.Fatal("relay did not record real usage")
	}
}

func TestRelayExtensionLogsPersistOfflineAndFlushInBoundedBatches(t *testing.T) {
	relay, token := relayFixture(t)
	relay.state.Snapshot.ExtensionLogging = true
	server := NewServer(relay)
	def, err := proxyextension.Compile("logging", `export default p=>p.on('request',()=>{p.log('info','received request',{responseId:'example'});throw Error('relay failure')})`)
	if err != nil {
		t.Fatal(err)
	}
	server.engine.Extensions = proxyextension.New(def)
	server.engine.Client = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("upstream content"))}, nil
	})}
	request := httptest.NewRequest("GET", "/models", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	server.ServeHTTP(w, request)
	if w.Code != 200 || w.Body.String() != "upstream content" {
		t.Fatal(w.Code, w.Body)
	}
	loaded := &Relay{config: relay.config, state: state{Exclusions: map[string]int64{}}}
	if err := loaded.load(); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, event := range loaded.state.Outbox {
		if event.ExtensionLog != nil {
			found++
			if event.ExtensionLog.ExtensionID != "logging" || event.ExtensionLog.RelayID != relay.config.RelayID {
				t.Fatal(event)
			}
		}
	}
	if found != 2 {
		t.Fatal(loaded.state.Outbox)
	}
	// Make the payload exceed a single receiver request; each batch must fit.
	sample := loaded.state.Outbox[len(loaded.state.Outbox)-1]
	loaded.state.Outbox = nil
	for i := 0; i < 80; i++ {
		event := sample
		event.ID = fmt.Sprintf("event-%d", i)
		entry := *sample.ExtensionLog
		entry.Message = strings.Repeat("x", 8000)
		event.ExtensionLog = &entry
		loaded.state.Outbox = append(loaded.state.Outbox, event)
	}
	sizes := []int{}
	loaded.client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		sizes = append(sizes, len(raw))
		var batch relayprotocol.EventBatch
		if err := json.Unmarshal(raw, &batch); err != nil || len(batch.Events) > 32 || len(raw) > 1<<20 {
			t.Fatal(len(batch.Events), len(raw), err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	for len(loaded.state.Outbox) > 0 {
		if err := loaded.flush(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(sizes) != 3 {
		t.Fatal(sizes)
	}
}

func TestOlderPrimaryDoesNotBlockOperationalEventsBehindLogs(t *testing.T) {
	relay, _ := relayFixture(t)
	entry := proxyextension.LogEntry{ID: "log", ExtensionID: "example", Level: "ERROR", Message: "example", AtMS: 1}
	relay.state.Snapshot.ExtensionLogging = false
	relay.state.Outbox = []relayprotocol.Event{{ID: "diagnostic", Kind: "extension_log", ExtensionLog: &entry}, {ID: "operational", Kind: "failure"}}
	relay.client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		var batch relayprotocol.EventBatch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil || len(batch.Events) != 1 || batch.Events[0].ID != "operational" {
			t.Fatal(batch, err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	if err := relay.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(relay.state.Outbox) != 1 || relay.state.Outbox[0].ID != "diagnostic" {
		t.Fatal(relay.state.Outbox)
	}
}
func TestOperationalRecordsEvictDiagnosticsBeforeUsage(t *testing.T) {
	relay := &Relay{}
	for _, incoming := range []relayprotocol.Event{{ID: "usage", Kind: "telemetry"}, {ID: "critical", Kind: "failure"}} {
		relay.state.Outbox = make([]relayprotocol.Event, outboxLimit)
		for i := range relay.state.Outbox {
			relay.state.Outbox[i] = relayprotocol.Event{ID: fmt.Sprint(i), Kind: "telemetry"}
		}
		relay.state.Outbox[0].Kind = "extension_log"
		if err := relay.queueLocked(incoming, incoming.Kind == "failure"); err != nil {
			t.Fatal(err)
		}
		if len(relay.state.Outbox) != outboxLimit || relay.state.Outbox[0].ID != "1" || relay.state.Outbox[outboxLimit-1].ID != incoming.ID {
			t.Fatal("diagnostics displaced operational events")
		}
	}
}
