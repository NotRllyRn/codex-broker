// Package proxyextension hosts trusted, synchronous TypeScript proxy extensions.
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
	"github.com/dop251/goja"
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
	program   *goja.Program
	schema    settings.Schema
	loadError error
}
type Info = extensions.Info
type Manager struct {
	definitions         []Definition
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
	id       string
	vm       *goja.Runtime
	settings string
	failed   bool
	hook     string
	logs     []LogEntry
	secrets  []string
	handlers map[string][]goja.Callable
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

// Load reads operator-provisioned files once. There is deliberately no installer,
// module resolution, filesystem, network, timer or process API in the VM.
func Load(directory string) (*Manager, error) {
	m := &Manager{state: map[string]string{}}
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
		def, err := Compile(id, string(source))
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
	if !validID.MatchString(id) || extensions.Builtin(id) || len(source) > 1<<20 {
		return Definition{}, errors.New("invalid extension source")
	}
	result := api.Transform(source, api.TransformOptions{Loader: api.LoaderTS, Format: api.FormatCommonJS, Target: api.ES2020, Sourcefile: id + ".ts"})
	if len(result.Errors) != 0 {
		return Definition{}, fmt.Errorf("extension %s: %s", id, result.Errors[0].Text)
	}
	program, err := goja.Compile(id, string(result.Code), true)
	if err != nil {
		return Definition{}, err
	}
	schema, err := readSchema(id, program)
	return Definition{ID: id, program: program, schema: schema}, err
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
		in := &instance{id: def.ID, vm: goja.New(), settings: m.settingsJSON(def.ID), handlers: map[string][]goja.Callable{}}
		s.rememberSecrets(in)
		in.hook = "initialize"
		if err := m.configurationError(def.ID); err != nil {
			s.failed(in, "settings", err)
			continue
		}
		vm := in.vm
		vm.SetMaxCallStackSize(256)
		err := in.run(ctx, func() error {
			_, _ = vm.RunString("var module = {exports:{}}; var exports = module.exports;")
			_, err := vm.RunProgram(def.program)
			if err != nil {
				return err
			}
			factory, ok := goja.AssertFunction(vm.Get("module").ToObject(vm).Get("exports").ToObject(vm).Get("default"))
			if !ok {
				return errors.New("default export must be a factory")
			}
			apiObject := vm.NewObject()
			_ = apiObject.Set("log", func(level, message string, fields any) {
				raw, err := json.Marshal(map[string]any{"level": level, "message": message, "fields": fields})
				if err != nil {
					panic(vm.NewTypeError("log fields must be JSON"))
				}
				severity, text, values, err := logFields(string(raw))
				if err != nil {
					panic(vm.NewTypeError("invalid log entry: %s", err))
				}
				if s.logCount >= MaxRequestLogs || s.logBytes+len(raw) > MaxRequestLogBytes {
					if !s.logLimited {
						s.logLimited = true
						in.logs = append(in.logs, LogEntry{Level: "WARNING", Message: "Extension log limit reached; further authored logs dropped for this request."})
					}
					return
				}
				s.logCount++
				s.logBytes += len(raw)
				in.logs = append(in.logs, LogEntry{Level: severity, Message: text, Fields: values, AtMS: time.Now().UnixMilli()})
			})
			_ = apiObject.Set("getSettings", func() goja.Value {
				value, err := vm.RunString("JSON.parse(" + quote(in.settings) + ")")
				if err != nil {
					panic(err)
				}
				return value
			})
			_ = apiObject.Set("on", func(name string, handler goja.Callable) {
				if !hooks[name] || handler == nil || len(in.handlers[name]) >= 32 {
					panic(vm.NewTypeError("invalid hook registration"))
				}
				in.handlers[name] = append(in.handlers[name], handler)
			})
			// Atomic per-extension/client/thread JSON state, bounded and process-local.
			_ = apiObject.Set("updateThread", func(update goja.Callable) goja.Value {
				if update == nil {
					panic(vm.NewTypeError("updateThread requires a callback"))
				}
				if identity.ThreadID == "" {
					panic(vm.NewTypeError("native thread identity unavailable"))
				}
				m.mu.Lock()
				defer m.mu.Unlock()
				key := def.ID + "\x00" + identity.ClientID + "\x00" + identity.ThreadID
				raw := m.state[key]
				if raw == "" {
					raw = "null"
				}
				value, err := vm.RunString("JSON.parse(" + quote(raw) + ")")
				if err != nil {
					panic(err)
				}
				next, err := update(goja.Undefined(), value)
				if err != nil {
					panic(err)
				}
				if synchronous(next) != nil {
					panic(vm.NewTypeError("async state updates are unsupported"))
				}
				encoded, err := stringify(vm, next)
				if err != nil || len(encoded) > 64<<10 {
					panic(vm.NewTypeError("thread state must be JSON under 64 KiB"))
				}
				if _, exists := m.state[key]; !exists && len(m.state) >= 1024 {
					panic(vm.NewTypeError("thread state capacity reached"))
				}
				if encoded == "null" {
					delete(m.state, key)
				} else {
					m.state[key] = encoded
				}
				return next
			})
			value, err := factory(goja.Undefined(), apiObject)
			if err != nil {
				return err
			}
			return synchronous(value)
		})
		s.flushLogs(in)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			s.failed(in, "initialize", err)
			continue
		}
		s.instances = append(s.instances, in)
		if err := s.emitInstance(in, "settings_loaded", map[string]any{"settings": decodeSettings(in.settings)}, nil); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			s.failed(in, "settings_loaded", err)
		}
	}
	return s, nil
}
func quote(s string) string { b, _ := json.Marshal(s); return string(b) }
func synchronous(v goja.Value) error {
	if obj, ok := v.(*goja.Object); ok {
		if _, ok := goja.AssertFunction(obj.Get("then")); ok {
			return errors.New("async hooks are unsupported")
		}
	}
	return nil
}
func stringify(vm *goja.Runtime, v goja.Value) (string, error) {
	fn, _ := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("stringify"))
	result, err := fn(goja.Undefined(), v)
	if err != nil {
		return "", err
	}
	if goja.IsUndefined(result) {
		return "", errors.New("expected JSON")
	}
	return result.String(), nil
}
func (in *instance) run(ctx context.Context, fn func() error) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("extension host operation panicked")
		}
	}()
	// Join the watchdog before clearing the interrupt; a late interrupt must not
	// poison a later hook. No goroutine runs JS concurrently with the caller.
	if err := ctx.Err(); err != nil {
		return err
	}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		timer := time.NewTimer(HookTimeout)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			in.vm.Interrupt("cancelled")
		case <-timer.C:
			in.vm.Interrupt("hook timeout")
		case <-done:
		}
	}()
	defer func() { close(done); <-stopped; in.vm.ClearInterrupt() }()
	return fn()
}
func (s *Session) Has(name string) bool {
	for _, in := range s.instances {
		if !in.failed && len(in.handlers[name]) > 0 {
			return true
		}
	}
	return false
}

// Emit chains explicit patches; undefined means unchanged. A response patch
// short-circuits request processing. Go-owned values never cross into the VM.
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
		if len(in.handlers[name]) == 0 {
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
	for _, handler := range in.handlers[name] {
		err := in.run(s.ctx, func() error {
			raw, err := json.Marshal(event)
			if err != nil || len(raw) > MaxPayload {
				return errors.New("hook input exceeds limit")
			}
			value, err := in.vm.RunString("JSON.parse(" + quote(string(raw)) + ")")
			if err != nil {
				return err
			}
			identity, _ := json.Marshal(s.identity)
			ctx, err := in.vm.RunString("JSON.parse(" + quote(string(identity)) + ")")
			if err != nil {
				return err
			}
			result, err := handler(goja.Undefined(), value, ctx)
			if err != nil {
				return err
			}
			if err = synchronous(result); err != nil {
				return err
			}
			if name == "settings_loaded" || name == "settings_changed" || goja.IsUndefined(result) || goja.IsNull(result) {
				return nil
			}
			encoded, err := stringify(in.vm, result)
			if err != nil {
				return err
			}
			if len(encoded) > MaxPayload {
				return errors.New("hook output exceeds limit")
			}
			var patch map[string]any
			if err = json.Unmarshal([]byte(encoded), &patch); err != nil {
				return err
			}
			for k, v := range patch {
				event[k] = v
			}
			if validate != nil {
				return validate(event)
			}
			return nil
		})
		s.flushLogs(in)
		if err != nil {
			return err
		}
		if event["response"] != nil && (name == "request" || name == "prepared" || name == "attempt") {
			return nil
		}
	}
	return nil
}

// Finish gets a fresh bounded context so cancellation still permits cleanup.
func (s *Session) Finish(outcome string) {
	cleanup := *s
	cleanup.ctx = context.Background()
	for _, in := range s.instances {
		for _, handler := range in.handlers["finish"] {
			handlers := in.handlers["finish"]
			in.handlers["finish"] = []goja.Callable{handler}
			err := cleanup.emitInstance(in, "finish", map[string]any{"outcome": outcome}, nil)
			in.handlers["finish"] = handlers
			if err != nil {
				cleanup.failed(in, "finish", err)
			}
		}
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
