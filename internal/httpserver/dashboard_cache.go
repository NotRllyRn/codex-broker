package httpserver

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net/http"
	"sync"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/webassets"
	"github.com/flosch/pongo2/v6"
)

// Changing embedded assets also changes the browser's snapshot namespace.
var dashboardVersion = func() string {
	hash := sha256.New()
	_ = fs.WalkDir(webassets.Files, ".", func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			body, _ := webassets.Files.ReadFile(path)
			hash.Write([]byte(path))
			hash.Write(body)
		}
		return err
	})
	return fmt.Sprintf("%x", hash.Sum(nil))[:16]
}()

type cachedResponse struct {
	body    []byte
	header  http.Header
	status  int
	etag    string
	expires time.Time
}

type responseCache struct {
	mu      sync.Mutex
	entries map[string]cachedResponse
	bytes   int
}

type responseBuffer struct {
	bytes.Buffer
	header http.Header
	status int
}

func (w *responseBuffer) Header() http.Header    { return w.header }
func (w *responseBuffer) WriteHeader(status int) { w.status = status }

// Authenticate even on cache hits. Never cache errors, redirects, or credentials
// from another session. The short TTL bounds staleness without a new event bus.
func (s *Server) cached(ttl time.Duration, next endpoint) endpoint {
	cache := &responseCache{}
	return func(w http.ResponseWriter, r *http.Request) error {
		if err := s.requireSession(r); err != nil {
			return err
		}
		key := fmt.Sprintf("%x", auth.Digest(cookie(r, sessionCookie)+"\x00"+cookie(r, csrfCookie)+"\x00"+cookie(r, "codex-broker.streamer")+"\x00"+r.URL.RequestURI()))
		entry, err := cache.get(key, r.Header.Get("X-Dashboard-Refresh") == "true", func() (cachedResponse, error) {
			buffer := &responseBuffer{header: make(http.Header), status: http.StatusOK}
			if err := next(buffer, r); err != nil {
				return cachedResponse{}, err
			}
			return cachedResponse{body: buffer.Bytes(), header: buffer.header, status: buffer.status, etag: fmt.Sprintf(`"%x"`, sha256.Sum256(buffer.Bytes())), expires: time.Now().Add(ttl)}, nil
		})
		if err != nil {
			return err
		}
		for name, values := range entry.header {
			w.Header()[name] = append([]string(nil), values...)
		}
		if entry.status != http.StatusOK {
			w.WriteHeader(entry.status)
			_, err := w.Write(entry.body)
			return err
		}
		w.Header().Set("Cache-Control", "private, no-cache")
		w.Header().Del("Pragma")
		w.Header().Set("Vary", "Cookie")
		w.Header().Set("ETag", entry.etag)
		w.Header().Set("X-Dashboard-TTL", fmt.Sprint(max(0, time.Until(entry.expires).Milliseconds())))
		if r.Header.Get("If-None-Match") == entry.etag {
			w.WriteHeader(http.StatusNotModified)
			return nil
		}
		_, err = w.Write(entry.body)
		return err
	}
}

// Coalesce builds per endpoint, but release the lock before writing to a client.
func (cache *responseCache) get(key string, force bool, build func() (cachedResponse, error)) (cachedResponse, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if entry, ok := cache.entries[key]; ok && time.Now().Before(entry.expires) && !force {
		return entry, nil
	}
	entry, err := build()
	if err != nil || entry.status != http.StatusOK {
		return entry, err
	}
	if len(cache.entries) >= 64 || cache.bytes+len(entry.body) > 16<<20 {
		cache.entries, cache.bytes = nil, 0
	}
	if cache.entries == nil {
		cache.entries = make(map[string]cachedResponse)
	}
	cache.bytes -= len(cache.entries[key].body)
	delete(cache.entries, key)
	if len(entry.body) <= 16<<20 {
		cache.entries[key] = entry
		cache.bytes += len(entry.body)
	}
	return entry, nil
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) error {
	if r.URL.Query().Get("render") == "1" {
		return s.dashboardView(w, r)
	}
	session, err := s.session(r)
	if err != nil {
		return err
	}
	if session == nil {
		s.redirect(w, r, "/login")
		return nil
	}
	scope := fmt.Sprintf("%x", auth.Digest(cookie(r, sessionCookie)+"\x00"+cookie(r, csrfCookie)+s.App.Config.RootPath+dashboardVersion))
	return s.render(w, r, 200, "dashboard_shell.html", pongo2.Context{"root_path": s.App.Config.RootPath, "csrf": cookie(r, csrfCookie), "cache_scope": scope})
}
