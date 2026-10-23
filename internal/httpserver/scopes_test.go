package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/broker"
	"github.com/NotRllyRn/codex-broker/internal/gateway"
	"github.com/NotRllyRn/codex-broker/internal/relayprotocol"
	"github.com/NotRllyRn/codex-broker/internal/webhook"
)

func TestAdminCreatesAndEditsKeyScopes(t *testing.T) {
	s, existing := proxyFixture(t, 1)
	ctx := t.Context()
	var err error
	s.renderer, err = newRenderer("templates")
	if err != nil {
		t.Fatal(err)
	}
	s.App.Webhooks = webhook.New(s.App.Store, nil)
	if err := s.App.Service.SetLabels(ctx, "account0", []string{"Work"}); err != nil {
		t.Fatal(err)
	}
	labels, err := s.App.ClientKeys.Labels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	field := "label_" + labels[0].ID
	if err := s.App.Admin.SetPassword(ctx, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	session, err := s.App.Admin.Login(ctx, "correct horse battery staple", "test")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, form url.Values, authenticated bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if authenticated {
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
		}
		response := httptest.NewRecorder()
		s.mux.ServeHTTP(response, req)
		return response
	}
	page := request("GET", "/settings", nil, true)
	if page.Code != 200 {
		t.Fatalf("settings=%d %s", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Body.String(), "All accounts") || !strings.Contains(page.Body.String(), "Edit account access") {
		t.Fatal("missing access controls")
	}
	form := url.Values{"name": {"Scoped"}, field: {"allow"}, "csrf_token": {session.CSRF}}
	created := request("POST", "/settings/client-keys", form, true)
	if created.Code != 200 {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	if !strings.Contains(created.Body.String(), `value="allow" selected`) {
		t.Fatal("saved selection not rendered")
	}
	keys, err := s.App.ClientKeys.List(ctx)
	if err != nil || len(keys) != 2 {
		t.Fatalf("keys=%+v err=%v", keys, err)
	}
	var scoped auth.ClientKey
	for _, key := range keys {
		if key.ID != existing.ID {
			scoped = key
		}
	}
	if len(scoped.Scopes) != 1 || scoped.Scopes[0].Mode != "allow" {
		t.Fatalf("creation scope=%+v", scoped.Scopes)
	}
	path := "/settings/client-keys/" + scoped.ID + "/scopes"
	if got := request("POST", path, url.Values{field: {"block"}}, true); got.Code != 403 {
		t.Fatalf("CSRF=%d", got.Code)
	}
	if got := request("POST", path, form, false); got.Code != 401 {
		t.Fatalf("session=%d", got.Code)
	}
	form.Set(field, "block")
	if got := request("POST", path, form, true); got.Code != 303 {
		t.Fatalf("edit=%d %s", got.Code, got.Body.String())
	}
	ids, err := s.App.ClientKeys.AllowedAccountIDs(ctx, scoped.ID)
	if err != nil || len(ids) != 0 {
		t.Fatalf("blocked ids=%v err=%v", ids, err)
	}
	form.Set(field, "")
	if got := request("POST", path, form, true); got.Code != 303 {
		t.Fatalf("clear=%d", got.Code)
	}
	ids, err = s.App.ClientKeys.AllowedAccountIDs(ctx, scoped.ID)
	if err != nil || len(ids) != 1 {
		t.Fatalf("cleared ids=%v err=%v", ids, err)
	}
	// Dashboard search must find a label as well as an account name.
	page = request("GET", "/?q=wOrK", nil, true)
	if page.Code != 200 || !strings.Contains(page.Body.String(), `class="orbit-account`) {
		t.Fatalf("label search=%d %s", page.Code, page.Body.String())
	}
	page = request("GET", "/?q=unmatched", nil, true)
	if page.Code != 200 || strings.Contains(page.Body.String(), `class="orbit-account`) {
		t.Fatal("search returned nonmatching account")
	}
}

func TestScopesFilterCentralRoutingAndRelaySnapshots(t *testing.T) {
	s, key := proxyFixture(t, 3)
	ctx := t.Context()
	for account, labels := range map[string][]string{"account0": {"Work", "Blocked"}, "account1": {"Work"}} {
		if err := s.App.Service.SetLabels(ctx, account, labels); err != nil {
			t.Fatal(err)
		}
	}
	labels, err := s.App.ClientKeys.Labels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var scopes []auth.LabelScope
	for _, label := range labels {
		mode := "allow"
		if label.Name == "Blocked" {
			mode = "block"
		}
		scopes = append(scopes, auth.LabelScope{ID: label.ID, Mode: mode})
	}
	if err := s.App.ClientKeys.SetScopes(ctx, key.ID, scopes); err != nil {
		t.Fatal(err)
	}
	lease, _, err := s.App.Router.Route(ctx, key.ID, broker.RouteRequest{PreferredAccountID: "account0"})
	if err != nil || lease == nil || lease.AccountID != "account1" {
		t.Fatalf("preferred bypass: %+v %v", lease, err)
	}
	s.App.Config.RelayKey = testRelayKey
	s.App.Ready = true
	snapshotResponse := relayRequest(s, "GET", "/api/private/v1/relay/snapshot?relay_id=edge", "", testRelayKey)
	if snapshotResponse.Code != 200 {
		t.Fatalf("snapshot=%d %s", snapshotResponse.Code, snapshotResponse.Body.String())
	}
	var snapshot relayprotocol.Snapshot
	if err := json.Unmarshal(snapshotResponse.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	client := snapshot.Clients[0]
	if !client.Restricted || !slices.Equal(client.AllowedAccountIDs, []string{"account1"}) {
		t.Fatalf("relay scope=%+v", client)
	}
	selected, _, err := s.App.RelayRoute(ctx, key.ID, gateway.RouteRequest{PreferredAccountID: "account2"})
	if err != nil || selected == nil || selected.AccountID != "account1" {
		t.Fatalf("online relay bypass: %+v %v", selected, err)
	}
	// Exhaustion of the only permitted account must not spill into the wider pool.
	if _, err := s.App.Store.DB.Exec("UPDATE usage_current SET short_used_percent_raw=100,short_resets_at_s=strftime('%s','now')+3600 WHERE account_id='account1'"); err != nil {
		t.Fatal(err)
	}
	lease, wait, err := s.App.Router.Route(ctx, key.ID, broker.RouteRequest{PreferredAccountID: "account2"})
	if err != nil || lease != nil || wait == nil {
		t.Fatalf("exhaustion widened scope: %+v %+v %v", lease, wait, err)
	}
}
