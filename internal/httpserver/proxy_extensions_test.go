package httpserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/proxyextension"
	"github.com/NotRllyRn/codex-broker/internal/webhook"
)

func installProxyExtension(t *testing.T, s *Server, source string) {
	t.Helper()
	def, err := proxyextension.Compile("example", source)
	if err != nil {
		t.Fatal(err)
	}
	s.proxyGateway().Extensions = proxyextension.New(def)
}
func upstreamResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func TestProxyExtensionDefaultEquivalenceAndUsageToggle(t *testing.T) {
	s, key := proxyFixture(t, 1)
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		return upstreamResponse(200, proxyUsageStream("response", "completed", 150)), nil
	})
	before := proxyRequest(s, key.Token, "/v1/responses", `{"input":[]}`)
	installProxyExtension(t, s, `export default p=>{for(const name of ['request','prepared','route','attempt','upstream','retry','response','upstream_event','client_event','finish'])p.on(name,()=>{});}`)
	after := proxyRequest(s, key.Token, "/v1/responses", `{"input":[]}`)
	if before.Code != after.Code || before.Body.String() != after.Body.String() {
		t.Fatalf("no-op changed output\nbefore %s\nafter %s", before.Body, after.Body)
	}
	for _, enabled := range []bool{false, true} {
		if err := proxyextension.SetEnabled(t.Context(), s.App.Store, proxyextension.UsageID, enabled); err != nil {
			t.Fatal(err)
		}
		result := proxyRequest(s, key.Token, "/v1/responses", `{"input":[]}`)
		if result.Code != 200 || strings.Contains(result.Body.String(), "🔨 account0") != enabled || !strings.Contains(result.Body.String(), "Hello") {
			t.Fatal(result.Code, result.Body)
		}
		snapshot, err := s.App.RelaySnapshot(t.Context(), "edge")
		if err != nil {
			t.Fatal(err)
		}
		if (len(snapshot.DisabledExtensions) == 0) != enabled {
			t.Fatal(snapshot.DisabledExtensions)
		}
	}
	var count int
	if err := s.App.Store.DB.QueryRow("SELECT count(*) FROM proxy_responses WHERE total_tokens=150").Scan(&count); err != nil || count != 4 {
		t.Fatal(count, err)
	}
}
func TestProxyExtensionTransformsAndNativeIdentity(t *testing.T) {
	s, key := proxyFixture(t, 1)
	installProxyExtension(t, s, `export default p=>{
 let steps=[];
 p.on('request',(e,c)=>{if(e.headers.Authorization||e.headers.Cookie)throw Error('credentials exposed');if(c.threadId!=='native-thread'||c.turnId!=='native-turn')throw Error('identity');steps.push('request');return {body:JSON.stringify({...JSON.parse(e.body),model:'rewritten'})}});
 p.on('prepared',e=>{steps.push('prepared');return {headers:{...e.headers,'X-Example':['prepared']}}});
 p.on('route',()=>{steps.push('route')});
 p.on('attempt',e=>{steps.push('attempt');if(e.accountId!=='account0')throw Error('account');return {headers:{...e.headers,'X-Steps':[steps.join(',')],Authorization:['must-be-replaced']}}});
 p.on('upstream',e=>{if(e.headers['Set-Cookie'])throw Error('cookie');steps.push('upstream')});
 p.on('response',e=>({status:201,headers:{...e.headers,'X-Steps':[steps.join(',')]}}));
 p.on('upstream_event',({event})=>{if(event.response?.usage)event.response.usage.total_tokens=999;return {event}});
 p.on('client_event',({event})=>{if(event.type==='response.output_text.delta')event.delta='changed';return {event}});
 }`)
	s.proxyClient.Transport = proxyRoundTrip(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"rewritten"`) || r.Header.Get("X-Example") != "prepared" || r.Header.Get("X-Steps") != "request,prepared,route,attempt" || r.Header.Get("Authorization") == "must-be-replaced" {
			t.Errorf("incorrect dispatch %s", body)
		}
		response := upstreamResponse(200, proxyUsageStream("response", "completed", 150))
		response.Header.Set("Set-Cookie", "private")
		return response, nil
	})
	request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"input":[],"client_metadata":{"thread_id":"body-thread","turn_id":"native-turn"}}`))
	request.Header.Set("Authorization", "Bearer "+key.Token)
	request.Header.Set("Thread-ID", "native-thread")
	request.Header.Set("Cookie", "private")
	result := httptest.NewRecorder()
	s.mux.ServeHTTP(result, request)
	if result.Code != 201 || !strings.Contains(result.Body.String(), "changed") || !strings.Contains(result.Body.String(), `"total_tokens":999`) || result.Header().Get("X-Steps") != "request,prepared,route,attempt,upstream" {
		t.Fatal(result.Code, result.Body, result.Header())
	}
	var count int
	if err := s.App.Store.DB.QueryRow("SELECT count(*) FROM proxy_responses WHERE total_tokens=150 AND request_model='rewritten' AND upstream_http_status=200").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
func TestProxyExtensionSyntheticAndBufferedResponses(t *testing.T) {
	for _, hook := range []string{"request", "prepared", "attempt", "body"} {
		t.Run(hook, func(t *testing.T) {
			s, key := proxyFixture(t, 1)
			calls := 0
			s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				return upstreamResponse(200, proxyUsageStream("response", "completed", 150)), nil
			})
			source := `export default p=>p.on('` + hook + `',()=>({response:{status:201,headers:{'Content-Type':['application/json']},body:'{"local":true}'}}))`
			if hook == "body" {
				source = `export default p=>p.on('body',e=>({status:201,headers:{'Content-Type':['application/json']},body:'{"local":true}'}))`
			}
			installProxyExtension(t, s, source)
			result := proxyRequest(s, key.Token, "/v1/responses", `{"input":[]}`)
			expectedCalls := 0
			if hook == "body" {
				expectedCalls = 1
			}
			if result.Code != 201 || result.Body.String() != `{"local":true}` || calls != expectedCalls {
				t.Fatal(result.Code, result.Body, calls)
			}
			var count int
			if err := s.App.Store.DB.QueryRow("SELECT count(*) FROM proxy_responses WHERE total_tokens=150").Scan(&count); err != nil || count != expectedCalls {
				t.Fatal(count, err)
			}
		})
	}
}
func TestProxyExtensionRetriesAndNonSSE(t *testing.T) {
	s, key := proxyFixture(t, 2)
	calls := 0
	installProxyExtension(t, s, `export default p=>{let attempts=0,retries=0;p.on('attempt',e=>{attempts++;if(e.attempt!==attempts)throw Error('attempt')});p.on('retry',e=>{if(e.failureKind!=='quota')throw Error('kind');retries++});p.on('response',e=>({headers:{...e.headers,'X-Attempts':[String(attempts)],'X-Retries':[String(retries)]}}));p.on('body',e=>({body:e.body+' changed'}));}`)
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return upstreamResponse(429, `{"error":"quota"}`), nil
		}
		return upstreamResponse(200, "plain response"), nil
	})
	result := proxyRequest(s, key.Token, "/v1/responses", `{"input":[]}`)
	if result.Code != 200 || result.Body.String() != "plain response changed" || result.Header().Get("X-Attempts") != "2" || result.Header().Get("X-Retries") != "1" {
		t.Fatal(result.Code, result.Body, result.Header())
	}
}
func TestProxyExtensionFailureAndAuthentication(t *testing.T) {
	s, key := proxyFixture(t, 1)
	calls := 0
	installProxyExtension(t, s, `export default p=>p.on('request',()=>{throw Error('private payload')})`)
	s.proxyClient.Transport = proxyRoundTrip(func(*http.Request) (*http.Response, error) { calls++; return upstreamResponse(200, "unexpected"), nil })
	for _, token := range []string{"invalid", key.Token} {
		result := proxyRequest(s, token, "/v1/responses", `{}`)
		expected := 502
		if token == "invalid" {
			expected = 401
		}
		if result.Code != expected || strings.Contains(result.Body.String(), "private payload") {
			t.Fatal(result.Code, result.Body)
		}
	}
	if calls != 0 {
		t.Fatal(calls)
	}
}
func TestProxyExtensionSettings(t *testing.T) {
	s, _ := proxyFixture(t, 1)
	var err error
	s.renderer, err = newRenderer("templates")
	if err != nil {
		t.Fatal(err)
	}
	s.App.Webhooks = webhook.New(s.App.Store, nil)
	if err = s.App.Admin.SetPassword(t.Context(), "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	session, err := s.App.Admin.Login(t.Context(), "correct horse battery staple", "test")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, login, csrf bool) *httptest.ResponseRecorder {
		form := url.Values{"enabled": {"false"}}
		if csrf {
			form.Set("csrf_token", session.CSRF)
		}
		r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if login {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		return w
	}
	path := "/settings/proxy-extensions/account-usage/enabled"
	for _, test := range []struct {
		login, csrf bool
		status      int
	}{{false, true, 401}, {true, false, 403}, {true, true, 303}} {
		got := request("POST", path, test.login, test.csrf)
		if got.Code != test.status {
			t.Fatal(got.Code, got.Body)
		}
	}
	got := request("GET", "/settings", true, false)
	if got.Code != 200 || !strings.Contains(got.Body.String(), "Proxy extensions") || !strings.Contains(got.Body.String(), "account-usage") || !strings.Contains(got.Body.String(), "Disabled") {
		t.Fatal(got.Code, got.Body)
	}
	if got := request("POST", "/settings/proxy-extensions/missing/enabled", true, true); got.Code != 404 {
		t.Fatal(got.Code)
	}
}

func TestProxyExtensionToggleSnapshotAndReplayCleanup(t *testing.T) {
	s, key := proxyFixture(t, 1)
	s.proxyClient.Transport = proxyRoundTrip(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "🔨") {
			t.Error("disabled extension replay cleanup was skipped")
		}
		if err := proxyextension.SetEnabled(t.Context(), s.App.Store, proxyextension.UsageID, false); err != nil {
			t.Fatal(err)
		}
		return upstreamResponse(200, proxyUsageStream("response", "completed", 150)), nil
	})
	for i := 0; i < 2; i++ {
		got := proxyRequest(s, key.Token, "/v1/responses", `{"input":[{"role":"assistant","content":"🔨 Personal usage · 5h 80% 1h0m · 7d 90% 1d0h\n\nPrevious answer"}]}`)
		if got.Code != 200 || strings.Contains(got.Body.String(), "🔨") != (i == 0) {
			t.Fatal(got.Code, got.Body)
		}
	}
}
func TestProxyExtensionBrokerErrorsAndLocalSSE(t *testing.T) {
	s, key := proxyFixture(t, 0)
	installProxyExtension(t, s, `export default p=>p.on('body',e=>({status:418,body:'handled broker error'}))`)
	got := proxyRequest(s, key.Token, "/v1/responses", `{}`)
	if got.Code != 418 || got.Body.String() != "handled broker error" {
		t.Fatal(got.Code, got.Body)
	}
	installProxyExtension(t, s, `export default p=>{p.on('request',()=>({response:{status:200,headers:{},body:'data: {"type":"response.completed","response":{}}\n\n'}}));p.on('client_event',({event})=>({event:{...event,example:true}}));}`)
	got = proxyRequest(s, key.Token, "/v1/responses", `{}`)
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"example":true`) {
		t.Fatal(got.Code, got.Body)
	}
}
