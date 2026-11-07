package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/NotRllyRn/codex-broker/internal/gateway"
	"github.com/NotRllyRn/codex-broker/internal/proxyextension"
)

type Server struct {
	relay  *Relay
	engine *gateway.Engine
	mux    *http.ServeMux
}

func NewServer(relay *Relay) *Server {
	s := &Server{relay: relay, engine: gateway.New(relay, nil), mux: http.NewServeMux()}
	s.engine.ExtensionSettings = func(context.Context) (proxyextension.Configuration, error) {
		relay.mu.RLock()
		defer relay.mu.RUnlock()
		return relay.state.Snapshot.ExtensionSettings, nil
	}
	s.engine.DisabledExtensions = func(context.Context) (map[string]bool, error) {
		relay.mu.RLock()
		defer relay.mu.RUnlock()
		result := map[string]bool{}
		for _, id := range relay.state.Snapshot.DisabledExtensions {
			result[id] = true
		}
		return result, nil
	}
	s.mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) { write(w, 200, map[string]string{"status": "ok"}) })
	s.mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, _ *http.Request) {
		if relay.Ready() && s.engine.Ready() {
			write(w, 200, map[string]string{"status": "ok"})
		} else {
			write(w, 503, map[string]string{"status": "unavailable"})
		}
	})
	s.mux.HandleFunc("GET /v1/responses", func(w http.ResponseWriter, r *http.Request) { _ = s.engine.Upgrade(w, r) })
	for _, route := range []string{"POST /v1/responses", "POST /v1/responses/compact", "GET /models", "GET /v1/models", "POST /images/generations", "POST /images/edits", "POST /v1/images/generations", "POST /v1/images/edits", "POST /transcribe", "GET /thread/goal/get", "POST /thread/goal/get", "GET /thread/goal/set", "POST /thread/goal/set", "GET /codex/thread/goal/get", "POST /codex/thread/goal/get", "GET /codex/thread/goal/set", "POST /codex/thread/goal/set"} {
		s.mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) { _ = s.engine.Serve(w, r) })
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
