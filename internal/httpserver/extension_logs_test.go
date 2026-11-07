package httpserver

import (
	"encoding/json"
	"github.com/NotRllyRn/codex-broker/internal/relayprotocol"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/logbook"
	"github.com/NotRllyRn/codex-broker/internal/proxyextension"
	"github.com/NotRllyRn/codex-broker/internal/webhook"
)

func TestExtensionFailuresKeepProxyWorking(t *testing.T) {
	s, key := proxyFixture(t, 1)
	calls := 0
	s.proxyClient.Transport = proxyRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		if string(raw) != `{"input":[]}` {
			t.Error("failed extension changed request", string(raw))
		}
		return upstreamResponse(200, proxyUsageStream("response", "completed", 150)), nil
	})
	for _, tc := range []struct{ hook, source string }{
		{"initialize", `export default p=>{throw Error('example failure')}`},
		{"settings_loaded", `export default p=>p.on('settings_loaded',()=>{throw Error('example failure')})`},
		{"request", `export default p=>{p.on('request',()=>({body:'wrong'}));p.on('request',()=>{throw Error('example failure')})}`},
		{"prepared", `export default p=>p.on('prepared',()=>({bodyBase64:'invalid base64'}))`},
		{"attempt", `export default p=>p.on('attempt',()=>({headers:{'X-Example':['wrong\nheader']}}))`},
		{"request", `export default p=>p.on('request',()=>({response:{status:0,body:'wrong'}}))`},
		{"route", `export default p=>p.on('route',()=>{throw Error('example failure')})`},
		{"upstream", `export default p=>p.on('upstream',()=>{throw Error('example failure')})`},
		{"response", `export default p=>p.on('response',()=>({status:0}))`},
		{"response", `export default p=>p.on('response',()=>({buffer:'wrong'}))`},
		{"upstream_event", `export default p=>p.on('upstream_event',()=>({event:'invalid'}))`},
		{"client_event", `export default p=>p.on('client_event',()=>{throw Error('example failure')})`},
		{"body", `export default p=>p.on('body',()=>({bodyBase64:42}))`},
		{"finish", `export default p=>p.on('finish',()=>{throw Error('example failure')})`},
	} {
		t.Run(tc.hook, func(t *testing.T) {
			installProxyExtension(t, s, tc.source)
			before := calls
			result := proxyRequest(s, key.Token, "/v1/responses", `{"input":[]}`)
			if result.Code != 200 || !strings.Contains(result.Body.String(), "Hello") || calls != before+1 {
				t.Fatal(result.Code, result.Body, calls)
			}
			latest, err := proxyextension.LastErrors(t.Context(), s.App.Store)
			if err != nil || latest["example"].Hook != tc.hook || latest["example"].RequestID == "" || latest["example"].ClientID != key.ID {
				t.Fatal(latest, err)
			}
		})
	}
	var count int
	if err := s.App.Store.DB.QueryRow("SELECT count(*) FROM proxy_responses WHERE total_tokens=150").Scan(&count); err != nil || count != calls {
		t.Fatal(count, calls, err)
	}
}
func TestStreamFailureKeepsEarlierAndLaterExtensions(t *testing.T) {
	s, key := proxyFixture(t, 1)
	defs := []proxyextension.Definition{}
	for _, tc := range []struct{ id, source string }{
		{"before", `export default p=>p.on('client_event',({event})=>({event:{...event,before:true}}))`},
		{"broken", `export default p=>{p.on('client_event',({event})=>({event:{...event,wrong:true}}));p.on('client_event',()=>{throw Error('stream failure')})}`},
		{"after", `export default p=>p.on('client_event',({event})=>({event:{...event,after:true}}))`},
	} {
		def, err := proxyextension.Compile(tc.id, tc.source)
		if err != nil {
			t.Fatal(err)
		}
		defs = append(defs, def)
	}
	s.proxyGateway().Extensions = proxyextension.New(defs...)
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		return upstreamResponse(200, proxyUsageStream("response", "completed", 150)), nil
	})
	result := proxyRequest(s, key.Token, "/v1/responses", `{"input":[]}`)
	if result.Code != 200 || !strings.Contains(result.Body.String(), `"before":true`) || !strings.Contains(result.Body.String(), `"after":true`) || strings.Contains(result.Body.String(), `"wrong":true`) || !strings.Contains(result.Body.String(), "response.completed") {
		t.Fatal(result.Code, result.Body)
	}
	logs := s.App.Logs.Recent("ERROR", "broken", 100)
	if len(logs) != 1 {
		t.Fatal("broken hook retried for later frames", logs)
	}
}
func TestExtensionLogsAndLatestErrorSurviveRestart(t *testing.T) {
	s, key := proxyFixture(t, 0)
	installProxyExtension(t, s, `export const settings={fields:[{key:'secret',type:'password',label:'Secret',default:'private-example'}]};export default p=>p.on('request',()=>{
  p.log('info','added usage',{responseId:'response-example',token:'do not show'});
  throw Error('<img src=x onerror=alert(1)> '+p.getSettings().secret);
 })`)
	_ = proxyRequest(s, key.Token, "/v1/responses", `{"input":[]}`)
	logs := s.App.Logs.Recent("", "example", 100)
	if len(logs) != 2 || logs[0]["event"] != "proxy.extension.failed" || logs[1]["event"] != "proxy.extension.log" {
		t.Fatal(logs)
	}
	encoded, _ := json.Marshal(logs)
	if strings.Contains(string(encoded), "private-example") || strings.Contains(string(encoded), "do not show") {
		t.Fatal("secret leaked")
	}
	latest, err := proxyextension.LastErrors(t.Context(), s.App.Store)
	if err != nil || latest["example"].AtMS <= 0 {
		t.Fatal(latest, err)
	}
	s.proxyGateway().Extensions = proxyextension.New() // Simulate a fresh manager after restart.
	def, err := proxyextension.Compile("example", `export default p=>{}`)
	if err != nil {
		t.Fatal(err)
	}
	s.proxyGateway().Extensions = proxyextension.New(def)
	s.renderer, err = newRenderer("templates")
	if err != nil {
		t.Fatal(err)
	}
	s.App.Webhooks = webhook.New(s.App.Store, nil)
	if err := s.App.Admin.SetPassword(t.Context(), "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	session, err := s.App.Admin.Login(t.Context(), "correct horse battery staple", "test")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/settings", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, request)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "View last error") || !strings.Contains(w.Body.String(), latest["example"].LastError().At) || strings.Contains(w.Body.String(), "<img src=x") || !strings.Contains(w.Body.String(), "&lt;img") {
		t.Fatal(w.Code, w.Body)
	}
	// A reopened log book restores recent events for the dashboard/export.
	dir := t.TempDir()
	book, err := logbook.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	book.LogFields("INFO", "proxy.extension.log", "example", map[string]any{"extension_id": "example"})
	book.Close()
	book, err = logbook.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	if len(book.Recent("", "example", 10)) != 1 {
		t.Fatal("history lost after reopening logs")
	}
}

func TestRelayExtensionLogsAreAuthenticatedDeduplicatedAndVisible(t *testing.T) {
	s, _ := proxyFixture(t, 0)
	s.App.Config.RelayKey = testRelayKey
	s.App.Ready = true
	entry := proxyextension.LogEntry{ID: "log-example", ExtensionID: "example", Level: "ERROR", Hook: "request", Message: "relay failure", AtMS: time.Now().UnixMilli(), Context: proxyextension.Context{RequestID: "request-example"}, Failed: true}
	event := relayprotocol.Event{ID: "event-example", RelayID: "edge", Kind: "extension_log", OccurredAtMS: entry.AtMS, ExtensionLog: &entry}
	raw, _ := json.Marshal(relayprotocol.EventBatch{Events: []relayprotocol.Event{event}})
	for _, tc := range []struct {
		key    string
		status int
	}{{"wrong", 401}, {testRelayKey, 200}, {testRelayKey, 200}} {
		response := relayRequest(s, "POST", "/api/private/v1/relay/events", string(raw), tc.key)
		if response.Code != tc.status {
			t.Fatal(response.Code, response.Body)
		}
	}
	logs := s.App.Logs.Recent("ERROR", "example", 100)
	if len(logs) != 1 || logs[0]["relay_id"] != "edge" {
		t.Fatal(logs)
	}
	latest, err := proxyextension.LastErrors(t.Context(), s.App.Store)
	if err != nil || latest["example"].RelayID != "edge" {
		t.Fatal(latest, err)
	}
	event.ID = "invalid-log"
	event.ExtensionLog.Level = "invalid"
	raw, _ = json.Marshal(relayprotocol.EventBatch{Events: []relayprotocol.Event{event}})
	if response := relayRequest(s, "POST", "/api/private/v1/relay/events", string(raw), testRelayKey); response.Code != 422 {
		t.Fatal(response.Code, response.Body)
	}
}
