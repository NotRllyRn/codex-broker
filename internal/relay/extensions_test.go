package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/proxyextension"
)

func TestRelayExtensionSnapshotSurvivesOfflineCache(t *testing.T) {
	relay, token := relayFixture(t)
	relay.state.Snapshot.DisabledExtensions = []string{proxyextension.UsageID}
	if err := relay.saveLocked(); err != nil {
		t.Fatal(err)
	}
	loaded := &Relay{config: relay.config, state: state{Exclusions: map[string]int64{}}}
	if err := loaded.load(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(loaded)
	def, err := proxyextension.Compile("example", `export default p=>p.on('response',e=>({headers:{...e.headers,'X-Example':['relay']}}))`)
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
	if w.Code != 200 || w.Header().Get("X-Example") != "relay" || strings.Contains(w.Body.String(), "🔨") || !strings.Contains(w.Body.String(), "Hello") {
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
