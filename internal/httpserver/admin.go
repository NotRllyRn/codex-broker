package httpserver

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/broker"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/flosch/pongo2/v6"
)

func accountView(account broker.AccountSummary) map[string]any {
	return map[string]any{"account_id": account.AccountID, "public_token": account.PublicToken, "display_name": account.DisplayName, "labels": account.Labels, "enabled": account.Enabled, "overall_state": account.OverallState, "auth_state": account.AuthState, "usage_state": account.UsageState, "short_percent": account.ShortPercent, "short_reset": timestampView(account.ShortResetMS), "weekly_percent": account.WeeklyPercent, "weekly_reset": timestampView(account.WeeklyResetMS), "last_refresh": timestampView(account.LastRefreshMS), "active_operation": account.ActiveOperation, "evidence": account.Evidence, "cycle_state": account.CycleState, "cycle_position": account.CyclePosition, "cycle_size": account.CycleSize, "cycle_release": timestampView(account.CycleReleaseMS)}
}

func timestampView(milliseconds *int64) any {
	formatted := core.ISOTime(milliseconds)
	if formatted == nil {
		return nil
	}
	return *formatted
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) error {
	session, err := s.session(r)
	if err != nil {
		return err
	}
	if session == nil {
		s.redirect(w, r, "/login")
		return nil
	}
	accounts, err := s.App.Service.Accounts(r.Context())
	if err != nil {
		return err
	}
	profiles, err := s.App.Service.ProfileDashboard(r.Context())
	if err != nil {
		return err
	}
	profilesJSON, err := json.Marshal(profiles)
	if err != nil {
		return err
	}
	filter, query := r.URL.Query().Get("state_filter"), strings.ToLower(r.URL.Query().Get("q"))
	var views []map[string]any
	var attention []map[string]any
	cycleReady, cycleHeld := 0, 0
	counts := map[string]int{"HEALTHY": 0, "WARNING": 0, "ACTION_REQUIRED": 0, "ERROR": 0, "DISABLED": 0}
	for _, account := range accounts {
		if filter != "" && account.OverallState != filter {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(account.DisplayName), query) {
			continue
		}
		view := accountView(account)
		views = append(views, view)
		if account.CycleState == "IN_CYCLE" {
			cycleReady++
		} else if account.CycleState == "HELD" || account.CycleState == "RELEASING" {
			cycleHeld++
		}
		if account.OverallState != "HEALTHY" {
			attention = append(attention, view)
		}
		counts[account.OverallState]++
	}
	selected := map[string]bool{}
	for key := range counts {
		selected[key] = filter == key
	}
	return s.renderer.render(w, 200, "dashboard.html", pongo2.Context{"root_path": s.App.Config.RootPath, "accounts": views, "attention": attention, "profiles": profiles, "profiles_json": string(profilesJSON), "status_options": []string{"HEALTHY", "WARNING", "ACTION_REQUIRED", "ERROR", "DISABLED"}, "counts": counts, "csrf": cookie(r, csrfCookie), "vault_configured": s.App.VaultConfigured, "dev": os.Getenv("WINDOWKEEPER_ENV") != "production", "q": r.URL.Query().Get("q"), "state_filter": filter, "selected_states": selected, "cycle_enabled": s.App.Config.WindowPulseEnabled, "cycle_ready": cycleReady, "cycle_held": cycleHeld})
}

func (s *Server) accountNew(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireSession(r); err != nil {
		return err
	}
	return s.renderer.render(w, 200, "account_new.html", pongo2.Context{"root_path": s.App.Config.RootPath, "csrf": cookie(r, csrfCookie), "vault_configured": s.App.VaultConfigured})
}
func (s *Server) accountCreate(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	if !s.App.VaultConfigured {
		return core.NewError("VAULT_KEY_REQUIRED", "Configure the vault key before adding accounts", 503)
	}
	if !s.App.Compatibility.Compatible {
		return core.NewError(s.App.Compatibility.Code, s.App.Compatibility.Detail, 503)
	}
	_ = r.ParseForm()
	method := r.Form.Get("login_method")
	account, err := s.App.Service.CreateAccount(r.Context(), r.Form.Get("display_name"), strings.Split(r.Form.Get("labels"), ","), strings.TrimSpace(r.Form.Get("workspace")), method)
	if err != nil {
		return err
	}
	started, err := s.App.Service.StartLogin(r.Context(), account["public_token"].(string), method, cookie(r, sessionCookie), false, "")
	if err != nil {
		return err
	}
	noStore(w)
	return s.renderer.render(w, 200, "login_progress.html", pongo2.Context{"root_path": s.App.Config.RootPath, "account": account, "started": started, "method": method, "csrf": r.Form.Get("csrf_token")})
}
func (s *Server) accountDetail(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireSession(r); err != nil {
		return err
	}
	detail, err := s.App.Service.AccountDetail(r.Context(), r.PathValue("public"))
	if err != nil {
		return err
	}
	return s.renderer.render(w, 200, "account_detail.html", pongo2.Context{"root_path": s.App.Config.RootPath, "detail": detail, "csrf": cookie(r, csrfCookie)})
}
func (s *Server) authExport(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	content, err := s.App.Service.ExportAuth(r.Context(), r.PathValue("public"))
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="auth.json"`)
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	_, err = w.Write(content)
	return err
}
func (s *Server) refresh(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	operation, err := s.App.Service.Refresh(r.Context(), r.PathValue("public"), "USER")
	if err != nil {
		return err
	}
	s.redirect(w, r, "/operations/"+operation)
	return nil
}
func (s *Server) reauthenticate(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	if !s.App.VaultConfigured {
		return core.NewError("VAULT_KEY_REQUIRED", "Configure the vault key before signing in", 503)
	}
	if !s.App.Compatibility.Compatible {
		return core.NewError(s.App.Compatibility.Code, s.App.Compatibility.Detail, 503)
	}
	_ = r.ParseForm()
	public, method := r.PathValue("public"), r.Form.Get("login_method")
	detail, err := s.App.Service.AccountDetail(r.Context(), public)
	if err != nil {
		return err
	}
	started, err := s.App.Service.StartLogin(r.Context(), public, method, cookie(r, sessionCookie), true, "")
	if err != nil {
		return err
	}
	noStore(w)
	return s.renderer.render(w, 200, "login_progress.html", pongo2.Context{"root_path": s.App.Config.RootPath, "account": detail["account"], "started": started, "method": method, "csrf": r.Form.Get("csrf_token")})
}
func (s *Server) labels(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	_ = r.ParseForm()
	public := r.PathValue("public")
	if err := s.App.Service.SetLabels(r.Context(), public, strings.Split(r.Form.Get("labels"), ",")); err != nil {
		return err
	}
	s.redirect(w, r, "/accounts/"+public)
	return nil
}
func formBool(value string) bool { result, _ := strconv.ParseBool(value); return result }
func (s *Server) enabled(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	_ = r.ParseForm()
	public := r.PathValue("public")
	if err := s.App.Service.SetEnabled(r.Context(), public, formBool(r.Form.Get("enabled"))); err != nil {
		return err
	}
	s.redirect(w, r, "/accounts/"+public)
	return nil
}
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	_ = r.ParseForm()
	if err := s.App.Service.DeleteAccount(r.Context(), r.PathValue("public"), r.Form.Get("confirmation")); err != nil {
		return err
	}
	s.redirect(w, r, "/")
	return nil
}

func (s *Server) operation(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireSession(r); err != nil {
		return err
	}
	operation, err := s.App.Service.Operation(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	var result map[string]any
	encoded, _ := operation["result_json"].(string)
	_ = json.Unmarshal([]byte(encoded), &result)
	if result == nil {
		result = map[string]any{}
	}
	operation["result"] = result
	return s.renderer.render(w, 200, "operation.html", pongo2.Context{"root_path": s.App.Config.RootPath, "operation": operation, "csrf": cookie(r, csrfCookie)})
}
func (s *Server) incidents(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireSession(r); err != nil {
		return err
	}
	values, err := s.App.Service.Incidents(r.Context())
	if err != nil {
		return err
	}
	return s.renderer.render(w, 200, "incidents.html", pongo2.Context{"root_path": s.App.Config.RootPath, "incidents": values, "csrf": cookie(r, csrfCookie)})
}
func (s *Server) logs(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireSession(r); err != nil {
		return err
	}
	level, query := r.URL.Query().Get("level"), r.URL.Query().Get("q")
	return s.renderer.render(w, 200, "logs.html", pongo2.Context{"root_path": s.App.Config.RootPath, "logs": s.App.Logs.Recent(level, query, 100), "log_levels": []string{"DEBUG", "INFO", "WARNING", "ERROR"}, "level": level, "q": query, "csrf": cookie(r, csrfCookie)})
}
func (s *Server) logsExport(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireSession(r); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", "attachment; filename=codex-broker-logs.jsonl")
	noStore(w)
	return s.App.Logs.Export(r.URL.Query().Get("level"), r.URL.Query().Get("q"), w)
}

func settingsMap(application *broker.Application) map[string]any {
	return map[string]any{"usage_poll_seconds": application.Config.UsagePollSeconds, "browser_oauth_mode": application.Config.BrowserOAuthMode}
}
func compatibilityMap(application *broker.Application) map[string]any {
	return map[string]any{"compatible": application.Compatibility.Compatible, "code": application.Compatibility.Code, "detail": application.Compatibility.Detail}
}
func keysView(keys []auth.ClientKey) []map[string]any {
	result := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		result = append(result, map[string]any{"key_id": key.ID, "name": key.Name, "key_prefix": key.Prefix, "created_at_ms": key.CreatedAtMS, "last_used_at_ms": nullInt(key.LastUsedAtMS), "revoked_at_ms": nullInt(key.RevokedAtMS)})
	}
	return result
}
func nullInt(value sql.NullInt64) any {
	if value.Valid {
		return value.Int64
	}
	return nil
}
func (s *Server) settingsContext(r *http.Request, issued any) (pongo2.Context, error) {
	destinations, err := s.App.Webhooks.Destinations(r.Context())
	if err != nil {
		return nil, err
	}
	keys, err := s.App.ClientKeys.List(r.Context())
	if err != nil {
		return nil, err
	}
	open, err := s.App.Service.PublicEnrollmentOpen(r.Context())
	if err != nil {
		return nil, err
	}
	return pongo2.Context{"root_path": s.App.Config.RootPath, "settings": settingsMap(s.App), "vault_configured": s.App.VaultConfigured, "compatibility": compatibilityMap(s.App), "destinations": destinations, "client_keys": keysView(keys), "public_enrollment_open": open, "issued_key": issued, "csrf": cookie(r, csrfCookie)}, nil
}
func (s *Server) settings(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireSession(r); err != nil {
		return err
	}
	context, err := s.settingsContext(r, nil)
	if err != nil {
		return err
	}
	return s.renderer.render(w, 200, "settings.html", context)
}
func (s *Server) publicEnabled(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	_ = r.ParseForm()
	if err := s.App.Service.SetPublicEnrollmentOpen(r.Context(), formBool(r.Form.Get("enabled"))); err != nil {
		return err
	}
	s.redirect(w, r, "/settings")
	return nil
}
func (s *Server) clientKeyCreate(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	_ = r.ParseForm()
	issued, err := s.App.ClientKeys.Create(r.Context(), r.Form.Get("name"))
	if err != nil {
		return err
	}
	context, err := s.settingsContext(r, map[string]any{"token": issued.Token, "key_id": issued.ID, "name": issued.Name})
	if err != nil {
		return err
	}
	noStore(w)
	return s.renderer.render(w, 200, "settings.html", context)
}
func (s *Server) clientKeyRevoke(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	changed, err := s.App.ClientKeys.Revoke(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if !changed {
		return core.NewError("CLIENT_KEY_NOT_FOUND", "Active client key not found", 404)
	}
	s.redirect(w, r, "/settings")
	return nil
}
func (s *Server) clientKeyDelete(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	changed, err := s.App.ClientKeys.DeleteRevoked(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if !changed {
		return core.NewError("CLIENT_KEY_DELETE_BLOCKED", "Revoke the client key before deleting it", 409)
	}
	s.redirect(w, r, "/settings")
	return nil
}

func (s *Server) webhookCreate(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	if !s.App.VaultConfigured {
		return core.NewError("VAULT_KEY_REQUIRED", "Configure the vault key before adding webhooks", 503)
	}
	_ = r.ParseForm()
	_, err := s.App.Webhooks.Create(r.Context(), r.Form.Get("display_name"), r.Form.Get("url"), strings.TrimSpace(r.Form.Get("signing_secret")), r.Form.Get("kind"))
	if err != nil {
		return err
	}
	s.redirect(w, r, "/settings")
	return nil
}
func (s *Server) webhookTest(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	_, err := s.App.Webhooks.Test(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	s.redirect(w, r, "/settings")
	return nil
}
func (s *Server) webhookEnabled(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	_ = r.ParseForm()
	if err := s.App.Webhooks.SetEnabled(r.Context(), r.PathValue("id"), formBool(r.Form.Get("enabled"))); err != nil {
		return err
	}
	s.redirect(w, r, "/settings")
	return nil
}
func (s *Server) webhookDelete(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	if err := s.App.Webhooks.Delete(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	s.redirect(w, r, "/settings")
	return nil
}

func (s *Server) internalDashboard(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireSession(r); err != nil {
		return err
	}
	accounts, err := s.App.Service.Accounts(r.Context())
	if err != nil {
		return err
	}
	views := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		views = append(views, accountView(account))
	}
	return writeJSON(w, 200, map[string]any{"api_version": "codex-broker.dev/internal/v1", "kind": "Dashboard", "data": views})
}
func (s *Server) internalOperation(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireSession(r); err != nil {
		return err
	}
	operation, err := s.App.Service.Operation(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"api_version": "codex-broker.dev/internal/v1", "kind": "Operation", "data": operation})
}
func (s *Server) interaction(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireSession(r); err != nil {
		return err
	}
	value, err := s.App.Service.Interaction(r.PathValue("id"), cookie(r, sessionCookie), r.Header.Get("X-Interaction-Nonce"))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"api_version": "codex-broker.dev/internal/v1", "kind": "LoginInteraction", "data": value})
}
func (s *Server) callback(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	var body struct {
		URL string `json:"callback_url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		return err
	}
	if err := s.App.Service.ForwardCallback(r.Context(), r.PathValue("id"), cookie(r, sessionCookie), r.Header.Get("X-Interaction-Nonce"), body.URL); err != nil {
		return err
	}
	return writeJSON(w, 202, map[string]any{"kind": "BrowserCallbackAccepted", "data": map[string]any{"status": "forwarding"}})
}
func (s *Server) cancelLogin(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	operation, err := s.App.Service.CancelLogin(r.Context(), r.PathValue("id"), cookie(r, sessionCookie))
	if err != nil {
		return err
	}
	return writeJSON(w, 202, map[string]any{"kind": "Operation", "data": map[string]any{"operation_id": operation}})
}
func (s *Server) eventStream(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireSession(r); err != nil {
		return err
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return core.NewError("STREAM_UNAVAILABLE", "Event streaming is unavailable", 500)
	}
	var after uint64
	value := r.Header.Get("Last-Event-ID")
	if value != "" {
		after, _ = strconv.ParseUint(value, 10, 64)
	}
	events, cancel := s.App.Events.Subscribe(after, value != "")
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case event, ok := <-events:
			if !ok {
				return nil
			}
			_, _ = w.Write(event.Encode())
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = w.Write([]byte(": heartbeat\n\n"))
			flusher.Flush()
		}
	}
}
