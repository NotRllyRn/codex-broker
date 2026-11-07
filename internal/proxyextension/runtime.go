// Package proxyextension hosts trusted Node/TypeScript proxy extensions.
package proxyextension

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/NotRllyRn/codex-broker/extensions"
	"github.com/NotRllyRn/codex-broker/extensions/requestsize"
	"github.com/NotRllyRn/codex-broker/extensions/settings"
	"github.com/NotRllyRn/codex-broker/internal/core"
	"github.com/evanw/esbuild/pkg/api"
)

const MaxPayload = 128 << 20
const HookTimeout = 100 * time.Millisecond

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

var hooks = map[string]bool{"request": true, "prepared": true, "route": true, "attempt": true, "upstream": true, "retry": true, "response": true, "upstream_event": true, "client_event": true, "body": true, "finish": true, "settings_loaded": true, "settings_changed": true}

type Failure struct{ ID, Hook string }

func (e *Failure) Error() string { return fmt.Sprintf("extension %s hook %s failed", e.ID, e.Hook) }

type Definition struct {
	ID        string
	code      string
	filename  string
	schema    settings.Schema
	loadError error
}
type Info = extensions.Info
type Manager struct {
	definitions         []Definition
	dataDir             string
	workersMu           sync.Mutex
	workers             map[string]*nodeWorker
	mu                  sync.Mutex
	state               map[string]string
	settingsMu          sync.RWMutex
	configuration       map[string]string
	configurationErrors map[string]error
	diagnosticsMu       sync.RWMutex
	latest              map[string]LogEntry
	loadReports         sync.Once
}
type Context struct {
	RequestID string `json:"requestId"`
	ClientID  string `json:"clientId"`
	ThreadID  string `json:"threadId,omitempty"`
	TurnID    string `json:"turnId,omitempty"`
}
type instance struct {
	id         string
	worker     *nodeWorker
	generation uint64
	session    string
	settings   string
	failed     bool
	hook       string
	logs       []LogEntry
	secrets    []string
	handlers   map[string]int
}
type Session struct {
	nativeFailed       map[string]bool
	instances          []*instance
	ctx                context.Context
	identity           Context
	Usage              bool
	compaction         requestsize.Request
	manager            *Manager
	native             Configuration
	sink               LogSink
	logCount, logBytes int
	logLimited         bool
}

// Load compiles operator-provisioned TypeScript files once. Node resolves runtime
// dependencies relative to the source file; extensions have normal OS access.
func Load(directory string, dataDirs ...string) (*Manager, error) {
	m := &Manager{state: map[string]string{}}
	if len(dataDirs) > 0 {
		m.dataDir = dataDirs[0]
	}
	if directory == "" {
		return m, nil
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ts") || strings.HasSuffix(entry.Name(), ".d.ts") {
			continue
		}
		if len(m.definitions) >= 32 {
			return nil, errors.New("too many proxy extensions")
		}
		id := strings.TrimSuffix(entry.Name(), ".ts")
		source, readErr := os.ReadFile(filepath.Join(directory, entry.Name()))
		def, err := compileFile(id, string(source), filepath.Join(directory, entry.Name()))
		if readErr != nil {
			err = readErr
		}
		if err != nil {
			if !validID.MatchString(id) || extensions.Builtin(id) {
				sum := sha256.Sum256([]byte(entry.Name()))
				id = fmt.Sprintf("invalid-%x", sum[:8])
			}
			def = Definition{ID: id, loadError: fmt.Errorf("Extension source %q: %w", entry.Name(), err)}
		}
		m.definitions = append(m.definitions, def)
		if def.loadError != nil {
			session := &Session{manager: m}
			session.writeLog(&instance{id: def.ID, hook: "load"}, "ERROR", def.loadError.Error(), nil, true)
		}
	}
	return m, nil
}
func Compile(id, source string) (Definition, error) {
	filename, _ := filepath.Abs(id + ".ts")
	return compileFile(id, source, filename)
}
func compileFile(id, source, filename string) (Definition, error) {
	if !validID.MatchString(id) || extensions.Builtin(id) || len(source) > 1<<20 {
		return Definition{}, errors.New("invalid extension source")
	}
	filename, err := filepath.Abs(filename)
	if err != nil {
		return Definition{}, err
	}
	result := api.Build(api.BuildOptions{Stdin: &api.StdinOptions{Contents: source, ResolveDir: filepath.Dir(filename), Sourcefile: filename, Loader: api.LoaderTS}, Bundle: true, Packages: api.PackagesExternal, Platform: api.PlatformNode, Format: api.FormatCommonJS, Target: api.ES2022, Write: false, LogLevel: api.LogLevelSilent})
	if len(result.Errors) > 0 {
		return Definition{}, fmt.Errorf("extension %s: %s", id, result.Errors[0].Text)
	}
	def := Definition{ID: id, code: string(result.OutputFiles[0].Contents), filename: filename}
	schema, err := readSchema(def)
	def.schema = schema
	return def, err
}
func New(definitions ...Definition) *Manager {
	return &Manager{definitions: definitions, state: map[string]string{}}
}
func (m *Manager) List(disabled map[string]bool) []Info {
	result := extensions.Builtins(disabled)
	if m != nil {
		for _, def := range m.definitions {
			result = append(result, Info{ID: def.ID, Name: def.ID, Settings: def.schema, Enabled: !disabled[def.ID]})
		}
	}
	if m != nil {
		m.diagnosticsMu.RLock()
		for i := range result {
			if entry, ok := m.latest[result[i].ID]; ok {
				result[i].LastError = entry.LastError()
			}
		}
		m.diagnosticsMu.RUnlock()
	}
	return result
}
func (m *Manager) Active(disabled map[string]bool) bool {
	if m != nil {
		for _, def := range m.definitions {
			if !disabled[def.ID] && def.loadError == nil {
				return true
			}
		}
	}
	return false
}
func (s *Session) Active() bool {
	for _, in := range s.instances {
		if !in.failed {
			return true
		}
	}
	return false
}
func (m *Manager) Start(ctx context.Context, identity Context, disabled map[string]bool, sinks ...LogSink) (*Session, error) {
	s := &Session{ctx: ctx, identity: identity, Usage: !disabled[UsageID], compaction: requestsize.New(!disabled[requestsize.ID]), manager: m, native: Configuration{}}
	if len(sinks) > 0 {
		s.sink = sinks[0]
	}
	for _, entry := range extensions.Builtins(nil) {
		s.native[entry.ID] = decodeSettings(m.settingsJSON(entry.ID))
		if err := m.configurationError(entry.ID); err != nil {
			if s.nativeFailed == nil {
				s.nativeFailed = map[string]bool{}
			}
			s.nativeFailed[entry.ID] = true
			s.writeLog(&instance{id: entry.ID, hook: "settings"}, "ERROR", err.Error(), nil, true)
		}
	}
	if m == nil {
		return s, nil
	}
	m.ReportLoadErrors(s.sink)
	for _, def := range m.definitions {
		if disabled[def.ID] || def.loadError != nil {
			continue
		}
		in := &instance{id: def.ID, worker: m.worker(def.ID), session: core.NewID(), settings: m.settingsJSON(def.ID), handlers: map[string]int{}}
		s.rememberSecrets(in)
		in.hook = "initialize"
		if err := m.configurationError(def.ID); err != nil {
			s.failed(in, "settings", err)
			continue
		}
		var names []string
		for name := range hooks {
			names = append(names, name)
		}
		_, err := s.nodeCall(in, map[string]any{"op": "start", "code": def.code, "filename": def.filename, "hooks": names, "dataDir": dataDirectory(m.dataDir, def.ID)})
		s.flushLogs(in)
		if err != nil {
			if ctx.Err() != nil {
				s.Finish("cancelled")
				return nil, ctx.Err()
			}
			s.failed(in, "initialize", err)
			continue
		}
		s.instances = append(s.instances, in)
		if err := s.emitInstance(in, "settings_loaded", map[string]any{"settings": decodeSettings(in.settings)}, nil); err != nil {
			if ctx.Err() != nil {
				s.Finish("cancelled")
				return nil, ctx.Err()
			}
			s.failed(in, "settings_loaded", err)
		}
	}
	return s, nil
}
func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func (s *Session) nodeCall(in *instance, command map[string]any) (map[string]json.RawMessage, error) {
	command["session"] = in.session
	command["identity"] = s.identity
	command["settings"] = decodeSettings(in.settings)
	// The worker serializes hooks; state is read after acquiring its gate via a
	// protocol read so concurrent requests never use stale snapshots.
	reply, generation, err := in.worker.call(s.ctx, in.generation, command, func(message map[string]json.RawMessage) (any, error) {
		switch string(message["kind"]) {
		case `"state_read"`:
			key := in.id + "\x00" + s.identity.ClientID + "\x00" + s.identity.ThreadID
			s.manager.mu.Lock()
			raw := s.manager.state[key]
			s.manager.mu.Unlock()
			if raw == "" {
				raw = "null"
			}
			return map[string]any{"value": json.RawMessage(raw)}, nil
		case `"state"`:
			key := in.id + "\x00" + s.identity.ClientID + "\x00" + s.identity.ThreadID
			raw := string(message["value"])
			s.manager.mu.Lock()
			defer s.manager.mu.Unlock()
			if raw != "null" {
				if _, exists := s.manager.state[key]; !exists && len(s.manager.state) >= 1024 {
					return map[string]any{"error": "thread state capacity reached"}, nil
				}
				s.manager.state[key] = raw
			} else {
				delete(s.manager.state, key)
			}
			return map[string]any{}, nil
		case `"log"`:
			raw := message["entry"]
			level, text, fields, err := logFields(string(raw))
			if err != nil {
				return nil, err
			}
			if s.logCount >= MaxRequestLogs || s.logBytes+len(raw) > MaxRequestLogBytes {
				if !s.logLimited {
					s.logLimited = true
					in.logs = append(in.logs, LogEntry{Level: "WARNING", Message: "Extension log limit reached; further authored logs dropped for this request."})
				}
				return nil, nil
			}
			s.logCount++
			s.logBytes += len(raw)
			in.logs = append(in.logs, LogEntry{Level: level, Message: text, Fields: fields, AtMS: time.Now().UnixMilli()})
			return nil, nil
		}
		return nil, errors.New("unknown Node worker message")
	})
	if generation != 0 {
		in.generation = generation
	}
	if raw := reply["handlers"]; len(raw) > 0 {
		if decodeErr := json.Unmarshal(raw, &in.handlers); err == nil {
			err = decodeErr
		}
	}
	return reply, err
}
func (s *Session) Has(name string) bool {
	for _, in := range s.instances {
		if !in.failed && in.handlers[name] > 0 {
			return true
		}
	}
	return false
}

// Emit chains explicit patches; undefined means unchanged. A response patch
// short-circuits request processing. Go-owned values cross the worker boundary only as JSON.
func (s *Session) Emit(name string, event map[string]any) error { return s.Transform(name, event, nil) }

// Transform commits one extension's patches only after all its handlers and the
// host validator succeed. A failure preserves earlier extensions' transformations.
func (s *Session) Transform(name string, event map[string]any, validate func(map[string]any) error) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	for _, in := range s.instances {
		if in.failed {
			continue
		}
		if err := s.manager.configurationError(in.id); err != nil {
			s.failed(in, "settings", err)
			continue
		}
		if name != "finish" && s.manager != nil {
			next := s.manager.settingsJSON(in.id)
			if next != in.settings {
				previous := decodeSettings(in.settings)
				in.settings = next
				s.rememberSecrets(in)
				if err := s.emitInstance(in, "settings_changed", map[string]any{"settings": decodeSettings(next), "previous": previous}, nil); err != nil {
					if s.ctx.Err() != nil {
						return s.ctx.Err()
					}
					s.failed(in, "settings_changed", err)
					continue
				}
			}
		}
		if in.handlers[name] == 0 {
			continue
		}
		candidate := make(map[string]any, len(event))
		for key, value := range event {
			candidate[key] = value
		}
		if err := s.emitInstance(in, name, candidate, validate); err != nil {
			if s.ctx.Err() != nil {
				return s.ctx.Err()
			}
			s.failed(in, name, err)
			continue
		}
		for key := range event {
			delete(event, key)
		}
		for key, value := range candidate {
			event[key] = value
		}
		if event["response"] != nil && (name == "request" || name == "prepared" || name == "attempt") {
			return nil
		}
	}
	return nil
}
func (s *Session) emitInstance(in *instance, name string, event map[string]any, validate func(map[string]any) error) error {
	in.hook = name
	for index, count := 0, in.handlers[name]; index < count; index++ {
		if err := s.emitHandler(in, name, index, event, validate); err != nil {
			return err
		}
		if event["response"] != nil && (name == "request" || name == "prepared" || name == "attempt") {
			return nil
		}
	}
	return nil
}
func (s *Session) emitHandler(in *instance, name string, index int, event map[string]any, validate func(map[string]any) error) error {
	reply, err := s.nodeCall(in, map[string]any{"op": "hook", "hook": name, "index": index, "event": event})
	s.flushLogs(in)
	if err != nil {
		return err
	}
	if name == "settings_loaded" || name == "settings_changed" || string(reply["patch"]) == "null" {
		return nil
	}
	var patch map[string]any
	if err = json.Unmarshal(reply["patch"], &patch); err != nil {
		return err
	}
	for k, v := range patch {
		event[k] = v
	}
	if validate != nil {
		return validate(event)
	}
	return nil
}

// Finish gets a fresh bounded context. A killed worker cannot restore closures.
func (s *Session) Finish(outcome string) {
	cleanup := *s
	cleanup.ctx = context.Background()
	for _, in := range s.instances {
		in.hook = "finish"
		for index, count := 0, in.handlers["finish"]; index < count; index++ {
			if err := cleanup.emitHandler(in, "finish", index, map[string]any{"outcome": outcome}, nil); err != nil {
				cleanup.failed(in, "finish", err)
			}
		}
		_, _ = cleanup.nodeCall(in, map[string]any{"op": "release"})
	}
}
func IDs(disabled map[string]bool) []string {
	var ids []string
	for id, off := range disabled {
		if off {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
