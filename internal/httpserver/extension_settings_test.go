package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NotRllyRn/codex-broker/extensions/requestsize"
	"github.com/NotRllyRn/codex-broker/extensions/settings"
	"github.com/NotRllyRn/codex-broker/internal/proxyextension"
	"github.com/NotRllyRn/codex-broker/internal/vault"
)

func TestExtensionSettingsAPI(t *testing.T) {
	s, key := proxyFixture(t, 0)
	installProxyExtension(t, s, `export const settings={fields:[
 {key:'label',type:'text',label:'Label',default:'default'},
 {key:'password',type:'password',label:'Password',default:'source-default'}
 ]};export default p=>p.on('request',()=>({response:{status:200,headers:{},body:p.getSettings().label}}))`)
	if err := s.App.Admin.SetPassword(t.Context(), "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	session, err := s.App.Admin.Login(t.Context(), "correct horse battery staple", "test")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, id, body string, login, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/settings/proxy-extensions/"+id+"/settings", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if login {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
		}
		if csrf {
			r.Header.Set("X-CSRF-Token", session.CSRF)
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		method      string
		login, csrf bool
		status      int
	}{{"GET", false, false, 401}, {"POST", false, true, 401}, {"POST", true, false, 403}} {
		if w := request(tc.method, "example", "{}", tc.login, tc.csrf); w.Code != tc.status {
			t.Fatal(w.Code, w.Body)
		}
	}
	for _, method := range []string{"GET", "POST"} {
		if w := request(method, "missing", "{}", true, true); w.Code != 404 {
			t.Fatal(w.Code, w.Body)
		}
	}
	view := func(w *httptest.ResponseRecorder) proxyextension.SettingsView {
		t.Helper()
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "source-default") || strings.Contains(w.Body.String(), "saved-secret") {
			t.Fatal("password leaked")
		}
		var value proxyextension.SettingsView
		if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	initial := view(request("GET", "example", "", true, false))
	if initial.Values["label"] != "default" || !initial.SecretSet["password"] {
		t.Fatal(initial)
	}
	if _, ok := initial.Values["password"]; ok {
		t.Fatal("password returned")
	}
	if w := request("POST", "example", `{"values":{"label":"changed","password":"saved-secret"}}`, true, true); w.Code != 503 {
		t.Fatal(w.Code, w.Body)
	}
	s.App.Vault, err = vault.New(make([]byte, 32), "settings-test")
	if err != nil {
		t.Fatal(err)
	}
	if w := request("POST", "example", `{"values":{"password":"saved-secret"}}`, true, true); w.Code != 503 {
		t.Fatal("accepted ephemeral vault", w.Code)
	}
	s.App.VaultConfigured = true
	running, err := s.proxyGateway().Extensions.Start(t.Context(), proxyextension.Context{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	saved := view(request("POST", "example", `{"values":{"label":"changed","password":"saved-secret"}}`, true, true))
	if saved.Revision == "" || saved.Values["label"] != "changed" || !saved.SecretSet["password"] {
		t.Fatal(saved)
	}
	event := map[string]any{}
	if err := running.Emit("request", event); err != nil {
		t.Fatal(err)
	}
	if event["response"].(map[string]any)["body"] != "changed" {
		t.Fatal("Apply did not publish to active sessions")
	}
	var raw string
	if err := s.App.Store.DB.QueryRowContext(t.Context(), "SELECT value_json FROM settings WHERE setting_key=?", "proxy_extension_settings:example").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "saved-secret") {
		t.Fatal("plaintext password persisted")
	}
	configs, err := proxyextension.Configurations(t.Context(), s.App.Store, s.App.Vault)
	if err != nil || configs["example"]["password"] != "saved-secret" {
		t.Fatal("secret decryption failed", err)
	}
	if got := proxyRequest(s, key.Token, "/v1/responses", `{"input":[]}`); got.Code != 200 || got.Body.String() != "changed" {
		t.Fatal(got.Code, got.Body)
	}
	// A stale editor cannot overwrite a newer revision.
	if w := request("POST", "example", `{"values":{"label":"stale"}}`, true, true); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	post := func(values settings.Values, reset bool) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"values": values, "revision": saved.Revision, "reset": reset})
		return request("POST", "example", string(body), true, true)
	}
	for _, values := range []settings.Values{{"unknown": true}, {"label": false}} {
		if w := post(values, false); w.Code != 400 {
			t.Fatal(w.Code, w.Body)
		}
	}
	if got := view(request("GET", "example", "", true, false)); got.Revision != saved.Revision || got.Values["label"] != "changed" {
		t.Fatal("failed save mutated settings", got)
	}
	// Omitted password preserves encrypted value, then explicit empty clears it.
	saved = view(post(settings.Values{"label": "preserved"}, false))
	configs, err = proxyextension.Configurations(t.Context(), s.App.Store, s.App.Vault)
	if err != nil || configs["example"]["password"] != "saved-secret" {
		t.Fatal("password not preserved", err)
	}
	saved = view(post(settings.Values{"label": "clear", "password": ""}, false))
	if saved.SecretSet["password"] {
		t.Fatal("password not cleared")
	}
	// Reset can be combined with draft edits; password resets to source default.
	saved = view(post(settings.Values{"label": "edited default"}, true))
	configs, err = proxyextension.Configurations(t.Context(), s.App.Store, s.App.Vault)
	if err != nil || configs["example"]["password"] != "source-default" || saved.Values["label"] != "edited default" {
		t.Fatal("reset failed", err)
	}
	// A newly loaded manager reads persisted values after a restart.
	def, err := proxyextension.Compile("example", `export const settings={fields:[{key:'label',type:'text',label:'Label',default:'default'},{key:'password',type:'password',label:'Password',default:'source-default'}]};export default p=>{}`)
	if err != nil {
		t.Fatal(err)
	}
	restarted := proxyextension.New(def)
	persisted, err := restarted.SettingsView(t.Context(), s.App.Store, s.App.Vault, "example")
	if err != nil || persisted.Values["label"] != "edited default" || !persisted.SecretSet["password"] {
		t.Fatal(persisted, err)
	}
	snapshot, err := s.App.RelaySnapshot(t.Context(), "edge")
	if err != nil || snapshot.ExtensionSettings["example"]["label"] != "edited default" || snapshot.ExtensionSettings["example"]["password"] != "source-default" {
		t.Fatal("relay settings missing", err)
	}
	if w := request("POST", "example", `{"values":{"label":"`+strings.Repeat("x", settings.MaxBytes*2)+`"}}`, true, true); w.Code != 400 {
		t.Fatal(w.Code, w.Body)
	}
	// Cross-field compaction constraints are checked without changing defaults.
	if w := request("POST", requestsize.ID, `{"values":{"triggerMiB":20,"targetMiB":30}}`, true, true); w.Code != 400 {
		t.Fatal(w.Code, w.Body)
	}
	if got := view(request("GET", requestsize.ID, "", true, false)); got.Values["triggerMiB"] != float64(50) || got.Values["targetMiB"] != float64(40) {
		t.Fatal(got)
	}
}
