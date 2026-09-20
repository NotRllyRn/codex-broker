package httpserver

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/broker"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/NotRllyRn/codex-broker/internal/webassets"
	"github.com/flosch/pongo2/v6"
)

const sessionCookie = "wk_session"
const csrfCookie = "wk_csrf"

type Server struct {
	App                                       *broker.Application
	renderer                                  *renderer
	mux                                       *http.ServeMux
	loginThrottle, clientAuth, enrollmentAuth *throttle
}
type endpoint func(http.ResponseWriter, *http.Request) error

type throttle struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	attempts map[string][]time.Time
}

func newThrottle(limit int, window time.Duration) *throttle {
	return &throttle{limit: limit, window: window, attempts: map[string][]time.Time{}}
}
func (t *throttle) allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	values := t.attempts[key][:0]
	for _, value := range t.attempts[key] {
		if now.Sub(value) <= t.window {
			values = append(values, value)
		}
	}
	if len(values) >= t.limit {
		t.attempts[key] = values
		return false
	}
	t.attempts[key] = append(values, now)
	return true
}
func (t *throttle) clear(key string) {
	t.mu.Lock()
	delete(t.attempts, key)
	t.mu.Unlock()
}

func New(application *broker.Application) (http.Handler, error) {
	templates, err := newRenderer("templates")
	if err != nil {
		return nil, err
	}
	server := &Server{App: application, renderer: templates, mux: http.NewServeMux(), loginThrottle: newThrottle(5, time.Minute), clientAuth: newThrottle(5, time.Minute), enrollmentAuth: newThrottle(20, time.Minute)}
	server.routes()
	return server.rootPath(server.middleware(server.mux)), nil
}

func (s *Server) rootPath(next http.Handler) http.Handler {
	if s.App.Config.RootPath == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == s.App.Config.RootPath {
			http.Redirect(w, r, s.App.Config.RootPath+"/", http.StatusTemporaryRedirect)
			return
		}
		if !strings.HasPrefix(r.URL.Path, s.App.Config.RootPath+"/") {
			http.NotFound(w, r)
			return
		}
		clone := r.Clone(r.Context())
		clone.URL.Path = strings.TrimPrefix(r.URL.Path, s.App.Config.RootPath)
		next.ServeHTTP(w, clone)
	})
}

func (s *Server) routes() {
	route := func(pattern string, handler endpoint) {
		s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if err := handler(w, r); err != nil {
				s.writeError(w, r, err)
			}
		})
	}
	static, _ := fs.Sub(webassets.Files, "static")
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	route("GET /health/live", func(w http.ResponseWriter, r *http.Request) error {
		return writeJSON(w, 200, map[string]any{"status": "ok"})
	})
	route("GET /health/ready", s.ready)
	route("GET /api/v1/health", s.apiHealth)
	route("POST /api/v1/route", s.route)
	route("POST /api/private/v1/public-enrollments/availability", s.enrollmentAvailability)
	route("POST /api/private/v1/public-enrollments", s.enrollmentStart)
	route("POST /api/private/v1/public-enrollments/status", s.enrollmentStatus)
	route("GET /login", s.loginPage)
	route("POST /login", s.login)
	route("POST /logout", s.logout)
	route("GET /", s.dashboard)
	route("GET /accounts/new", s.accountNew)
	route("POST /accounts", s.accountCreate)
	route("GET /accounts/{public}", s.accountDetail)
	route("POST /accounts/{public}/auth-export", s.authExport)
	route("POST /accounts/{public}/refresh", s.refresh)
	route("POST /accounts/{public}/reauthenticate", s.reauthenticate)
	route("POST /accounts/{public}/labels", s.labels)
	route("POST /accounts/{public}/enabled", s.enabled)
	route("POST /accounts/{public}/delete", s.deleteAccount)
	route("GET /operations/{id}", s.operation)
	route("GET /incidents", s.incidents)
	route("GET /logs", s.logs)
	route("GET /logs/export", s.logsExport)
	route("GET /settings", s.settings)
	route("POST /settings/public-enrollment", s.publicEnabled)
	route("POST /settings/client-keys", s.clientKeyCreate)
	route("POST /settings/client-keys/{id}/revoke", s.clientKeyRevoke)
	route("POST /settings/client-keys/{id}/delete", s.clientKeyDelete)
	route("POST /settings/webhooks", s.webhookCreate)
	route("POST /settings/webhooks/{id}/test", s.webhookTest)
	route("POST /settings/webhooks/{id}/enabled", s.webhookEnabled)
	route("POST /settings/webhooks/{id}/delete", s.webhookDelete)
	route("GET /api/internal/v1/dashboard", s.internalDashboard)
	route("GET /api/internal/v1/operations/{id}", s.internalOperation)
	route("GET /api/internal/v1/login-attempts/{id}/interaction", s.interaction)
	route("POST /api/internal/v1/login-attempts/{id}/browser-callback", s.callback)
	route("POST /api/internal/v1/login-attempts/{id}/cancel", s.cancelLogin)
	route("GET /api/internal/v1/events/state", s.eventStream)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store, max-age=0")
			w.Header().Set("Pragma", "no-cache")
		}
		if strings.HasPrefix(r.URL.Path, "/static/") {
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var problem *core.Error
	if !errors.As(err, &problem) {
		problem = core.NewError("INTERNAL_ERROR", "The request could not be completed", 500)
		s.App.Logs.Log("ERROR", "http.error", fmt.Sprintf("%T", err))
	}
	w.Header().Set("Content-Type", "application/problem+json")
	_ = writeJSON(w, problem.Status, map[string]any{"type": "urn:codex-broker:problem:" + strings.ToLower(strings.ReplaceAll(problem.Code, "_", "-")), "title": strings.Title(strings.ToLower(strings.ReplaceAll(problem.Code, "_", " "))), "status": problem.Status, "detail": problem.Detail, "instance": r.URL.Path, "code": problem.Code})
}
func writeJSON(w http.ResponseWriter, status int, value any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(value)
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("Pragma", "no-cache")
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) error {
	status := 503
	value := "unavailable"
	if s.App.Ready {
		status = 200
		value = "ok"
	}
	return writeJSON(w, status, map[string]any{"status": value})
}
func bearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return token
}
func (s *Server) client(r *http.Request) (*auth.ClientKey, error) {
	address := s.clientAddress(r)
	if !s.clientAuth.allow(address) {
		return nil, core.NewError("CLIENT_AUTH_THROTTLED", "Client authentication is temporarily unavailable", 429)
	}
	token := bearer(r)
	if !strings.HasPrefix(token, "cbk_") {
		return nil, core.NewError("CLIENT_KEY_INVALID", "Client authentication failed", 401)
	}
	key, err := s.App.ClientKeys.Authenticate(r.Context(), token)
	if err == nil {
		s.clientAuth.clear(address)
	}
	return key, err
}
func (s *Server) gateway(r *http.Request) error {
	address := s.clientAddress(r)
	if !s.enrollmentAuth.allow(address) {
		return core.NewError("PUBLIC_ENROLLMENT_THROTTLED", "Authentication failed", 429)
	}
	configured := s.App.Config.PublicEnrollmentKey
	token := bearer(r)
	if configured == "" || subtle.ConstantTimeCompare([]byte(configured), []byte(token)) != 1 {
		return core.NewError("PUBLIC_ENROLLMENT_UNAUTHORIZED", "Authentication failed", 401)
	}
	s.enrollmentAuth.clear(address)
	if !s.App.Ready {
		return core.NewError("BROKER_NOT_READY", "Codex Broker is unavailable", 503)
	}
	return nil
}
func (s *Server) apiHealth(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.client(r); err != nil {
		return err
	}
	return s.ready(w, r)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) error {
	key, err := s.client(r)
	if err != nil {
		return err
	}
	var body struct {
		SessionID string `json:"session_id"`
		TurnID    string `json:"turn_id"`
		Preferred string `json:"preferred_account_id"`
		Failed    string `json:"failed_account_id"`
		Failure   string `json:"failure_kind"`
	}
	if err = decodeJSON(r, &body); err != nil {
		return err
	}
	if utf8.RuneCountInString(body.SessionID) < 1 || utf8.RuneCountInString(body.SessionID) > 200 || utf8.RuneCountInString(body.TurnID) < 1 || utf8.RuneCountInString(body.TurnID) > 200 || utf8.RuneCountInString(body.Preferred) > 100 || utf8.RuneCountInString(body.Failed) > 100 || (body.Failed == "") != (body.Failure == "") || (body.Failure != "" && body.Failure != "quota" && body.Failure != "auth" && body.Failure != "rate_limit") {
		return core.NewError("VALIDATION_ERROR", "The route request is invalid", 422)
	}
	lease, wait, err := s.App.Router.Route(r.Context(), key.ID, broker.RouteRequest{SessionID: body.SessionID, TurnID: body.TurnID, PreferredAccountID: body.Preferred, FailedAccountID: body.Failed, FailureKind: body.Failure})
	if err != nil {
		return err
	}
	if wait != nil {
		w.Header().Set("Retry-After", strconv.Itoa(wait.RetryAfterSeconds))
		return writeJSON(w, 429, map[string]any{"status": "wait", "code": "POOL_EXHAUSTED", "next_retry_at": core.ISOTime(&wait.NextRetryAtMS), "retry_after_seconds": wait.RetryAfterSeconds})
	}
	return writeJSON(w, 200, map[string]any{"status": "ok", "account_id": lease.AccountID, "account_label": lease.AccountLabel, "access_token": lease.AccessToken, "chatgpt_account_id": lease.ChatGPTAccountID, "expires_at": core.ISOTime(&lease.ExpiresAtMS), "short_remaining_percent": lease.ShortRemainingPercent, "weekly_remaining_percent": lease.WeeklyRemainingPercent, "short_resets_at": isoPointer(lease.ShortResetsAtMS), "weekly_resets_at": isoPointer(lease.WeeklyResetsAtMS)})
}
func isoPointer(value *int64) any {
	if value == nil {
		return nil
	}
	return core.ISOTime(value)
}

func decodeJSON(r *http.Request, value any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return core.NewError("VALIDATION_ERROR", "The request body is invalid", 422)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return core.NewError("VALIDATION_ERROR", "The request body is invalid", 422)
	}
	return nil
}
func (s *Server) enrollmentAvailability(w http.ResponseWriter, r *http.Request) error {
	if err := s.gateway(r); err != nil {
		return err
	}
	enabled, err := s.App.Service.PublicEnrollmentOpen(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"enabled": enabled})
}
func (s *Server) enrollmentStart(w http.ResponseWriter, r *http.Request) error {
	if err := s.gateway(r); err != nil {
		return err
	}
	var body struct {
		Session string `json:"session_token"`
	}
	if err := decodeJSON(r, &body); err != nil {
		return err
	}
	if len(body.Session) < 32 || len(body.Session) > 200 {
		return core.NewError("VALIDATION_ERROR", "The request body is invalid", 422)
	}
	value, err := s.App.Service.StartPublicEnrollment(r.Context(), body.Session)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, value)
}
func (s *Server) enrollmentStatus(w http.ResponseWriter, r *http.Request) error {
	if err := s.gateway(r); err != nil {
		return err
	}
	var body struct {
		Enrollment string `json:"enrollment_id"`
		Attempt    string `json:"login_attempt_id"`
		Session    string `json:"session_token"`
		Nonce      string `json:"interaction_nonce"`
	}
	if err := decodeJSON(r, &body); err != nil {
		return err
	}
	if len(body.Enrollment) < 32 || len(body.Enrollment) > 100 || len(body.Attempt) < 32 || len(body.Attempt) > 100 || len(body.Session) < 32 || len(body.Session) > 200 || len(body.Nonce) < 32 || len(body.Nonce) > 200 {
		return core.NewError("VALIDATION_ERROR", "The request body is invalid", 422)
	}
	value, err := s.App.Service.PublicEnrollmentStatus(r.Context(), body.Enrollment, body.Attempt, body.Session, body.Nonce)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, value)
}

func cookie(r *http.Request, name string) string {
	value, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return value.Value
}
func (s *Server) session(r *http.Request) (*auth.SessionRow, error) {
	return s.App.Admin.Session(r.Context(), cookie(r, sessionCookie))
}
func (s *Server) requireSession(r *http.Request) error {
	value, err := s.session(r)
	if err != nil {
		return err
	}
	if value == nil {
		return core.NewError("ADMIN_LOGIN_REQUIRED", "Administrator login required", 401)
	}
	return nil
}
func (s *Server) requireForm(r *http.Request) error {
	token := cookie(r, sessionCookie)
	if token == "" {
		return core.NewError("ADMIN_LOGIN_REQUIRED", "Administrator login required", 401)
	}
	csrf := r.Header.Get("X-CSRF-Token")
	if csrf == "" {
		_ = r.ParseForm()
		csrf = r.Form.Get("csrf_token")
	}
	return s.App.Admin.RequireCSRF(r.Context(), token, csrf)
}
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, path string) {
	http.Redirect(w, r, s.App.Config.RootPath+path, http.StatusSeeOther)
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) error {
	session, _ := s.session(r)
	if session != nil {
		s.redirect(w, r, "/")
		return nil
	}
	return s.renderer.render(w, 200, "login.html", pongo2.Context{"error": nil, "root_path": s.App.Config.RootPath})
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) error {
	noStore(w)
	address := s.clientAddress(r)
	if !s.loginThrottle.allow(address) {
		return s.renderer.render(w, 429, "login.html", pongo2.Context{"error": "Login temporarily unavailable. Wait one minute and retry.", "root_path": s.App.Config.RootPath})
	}
	_ = r.ParseForm()
	created, err := s.App.Admin.Login(r.Context(), r.Form.Get("password"), truncate(r.UserAgent(), 200))
	if err != nil {
		return s.renderer.render(w, 200, "login.html", pongo2.Context{"error": "The password was not accepted", "root_path": s.App.Config.RootPath})
	}
	s.loginThrottle.clear(address)
	secure := s.App.Config.CookieSecure == "true" || (s.App.Config.CookieSecure == "auto" && s.requestScheme(r) == "https")
	path := s.App.Config.RootPath
	if path == "" {
		path = "/"
	}
	max := s.App.Config.SessionAbsoluteHours * 3600
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: created.Token, Path: path, MaxAge: max, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: created.CSRF, Path: path, MaxAge: max, Secure: secure, SameSite: http.SameSiteLaxMode})
	s.redirect(w, r, "/")
	return nil
}
func (s *Server) requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if s.trustedProxy(r) && strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]), "https") {
		return "https"
	}
	return "http"
}

func (s *Server) trustedProxy(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, prefix := range s.App.Config.TrustedProxies {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func (s *Server) clientAddress(r *http.Request) string {
	if s.trustedProxy(r) {
		if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); forwarded != "" {
			return forwarded
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireForm(r); err != nil {
		return err
	}
	_ = s.App.Admin.Logout(r.Context(), cookie(r, sessionCookie))
	path := s.App.Config.RootPath
	if path == "" {
		path = "/"
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: path, MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Path: path, MaxAge: -1})
	s.redirect(w, r, "/login")
	return nil
}

func truncate(value string, limit int) string {
	if runes := []rune(value); len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}
