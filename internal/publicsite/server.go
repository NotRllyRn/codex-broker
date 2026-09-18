package publicsite

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/config"
	"github.com/NotRllyRn/codex-broker/internal/webassets"
	"github.com/flosch/pongo2/v6"
)

const cookieName = "cb_public_enrollment"

type Handle struct {
	EnrollmentID     string `json:"enrollment_id"`
	LoginAttemptID   string `json:"login_attempt_id"`
	SessionToken     string `json:"session_token"`
	InteractionNonce string `json:"interaction_nonce"`
}

func (h Handle) Encode() string {
	value, _ := json.Marshal(h)
	return base64.RawURLEncoding.EncodeToString(value)
}
func decodeHandle(value string) *Handle {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil
	}
	var handle Handle
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&handle) != nil {
		return nil
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil
	}
	for _, value := range []string{handle.EnrollmentID, handle.LoginAttemptID, handle.SessionToken, handle.InteractionNonce} {
		if len(value) < 32 || len(value) > 200 {
			return nil
		}
	}
	return &handle
}

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

type Server struct {
	config        config.Config
	client        *http.Client
	template      *pongo2.Template
	start, status *throttle
	mux           *http.ServeMux
}

func New(settings config.Config) (http.Handler, error) {
	templates, err := fs.Sub(webassets.Files, "public_templates")
	if err != nil {
		return nil, err
	}
	template, err := pongo2.NewSet("public", pongo2.NewFSLoader(templates)).FromFile("enroll.html")
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if settings.PublicEnrollmentCACert != "" {
		pem, err := os.ReadFile(settings.PublicEnrollmentCACert)
		if err != nil {
			return nil, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("public enrollment CA certificate is invalid")
		}
		tlsConfig.RootCAs = roots
	}
	server := &Server{settings, &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, TLSClientConfig: tlsConfig, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext}}, template, newThrottle(settings.PublicEnrollmentAttemptsPerHour, time.Hour), newThrottle(120, time.Minute), http.NewServeMux()}
	server.routes()
	return server.headers(server.mux), nil
}
func (s *Server) routes() {
	static, _ := fs.Sub(webassets.Files, "public_static")
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	s.mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]any{"status": "ok"}) })
	s.mux.HandleFunc("GET /", s.page)
	s.mux.HandleFunc("POST /start", s.begin)
	s.mux.HandleFunc("GET /status", s.poll)
}
func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setHeaders(w)
		if r.URL.Path != "/health/live" {
			var availability map[string]any
			if s.call(r.Context(), "/api/private/v1/public-enrollments/availability", map[string]string{}, &availability) != nil {
				w.WriteHeader(503)
				return
			}
			if enabled, _ := availability["enabled"].(bool); !enabled {
				w.WriteHeader(404)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
func setHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (s *Server) call(ctx context.Context, path string, body any, result any) error {
	encoded, _ := json.Marshal(body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.PublicEnrollmentBrokerURL+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+s.config.PublicEnrollmentKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return errors.New("broker rejected public enrollment")
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result)
}
func (s *Server) page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.template.ExecuteWriter(pongo2.Context{"error": nil}, w)
}
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
func existing(r *http.Request) *Handle {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	return decodeHandle(cookie.Value)
}
func (s *Server) begin(w http.ResponseWriter, r *http.Request) {
	if existing(r) != nil {
		writeJSON(w, 200, map[string]any{"state": "STARTING"})
		return
	}
	if !s.start.allow(clientIP(r)) {
		writeJSON(w, 429, map[string]any{"state": "FAILED"})
		return
	}
	tokenBytes := make([]byte, 32)
	_, _ = rand.Read(tokenBytes)
	session := base64.RawURLEncoding.EncodeToString(tokenBytes)
	var started struct {
		EnrollmentID     string `json:"enrollment_id"`
		LoginAttemptID   string `json:"login_attempt_id"`
		InteractionNonce string `json:"interaction_nonce"`
	}
	if s.call(r.Context(), "/api/private/v1/public-enrollments", map[string]string{"session_token": session}, &started) != nil {
		writeJSON(w, 503, map[string]any{"state": "UNAVAILABLE"})
		return
	}
	handle := Handle{started.EnrollmentID, started.LoginAttemptID, session, started.InteractionNonce}
	if decodeHandle(handle.Encode()) == nil {
		writeJSON(w, 503, map[string]any{"state": "UNAVAILABLE"})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: handle.Encode(), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, Path: "/", MaxAge: s.config.LoginTimeoutSeconds})
	writeJSON(w, 200, map[string]any{"state": "STARTING"})
}
func (s *Server) poll(w http.ResponseWriter, r *http.Request) {
	if !s.status.allow(clientIP(r)) {
		writeJSON(w, 429, map[string]any{"state": "UNAVAILABLE"})
		return
	}
	handle := existing(r)
	if handle == nil {
		writeJSON(w, 404, map[string]any{"state": "FAILED"})
		return
	}
	body := map[string]string{"enrollment_id": handle.EnrollmentID, "login_attempt_id": handle.LoginAttemptID, "session_token": handle.SessionToken, "interaction_nonce": handle.InteractionNonce}
	var status map[string]any
	if s.call(r.Context(), "/api/private/v1/public-enrollments/status", body, &status) != nil {
		writeJSON(w, 503, map[string]any{"state": "UNAVAILABLE"})
		return
	}
	state, _ := status["state"].(string)
	if state == "" {
		state = "FAILED"
	}
	if state == "WAITING_FOR_USER" {
		verification, _ := status["verification_url"].(string)
		parsed, err := url.Parse(verification)
		if err != nil || parsed.Scheme != "https" || parsed.Host != "auth.openai.com" {
			writeJSON(w, 503, map[string]any{"state": "UNAVAILABLE"})
			return
		}
		userCode, _ := status["user_code"].(string)
		status = map[string]any{"state": state, "verification_url": verification, "user_code": userCode, "expires_at_ms": status["expires_at_ms"]}
	} else if state == "COMPLETED" {
		email, _ := status["email"].(string)
		status = map[string]any{"state": state, "email": email}
	} else {
		status = map[string]any{"state": state}
	}
	if state == "COMPLETED" || state == "FAILED" {
		http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true, Secure: true})
	}
	writeJSON(w, 200, status)
}
