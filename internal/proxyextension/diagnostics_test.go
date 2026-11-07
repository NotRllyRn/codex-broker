package proxyextension

import (
	"context"
	"github.com/NotRllyRn/codex-broker/extensions/requestsize"
	"github.com/NotRllyRn/codex-broker/extensions/settings"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFailureRollsBackWholeExtensionAndContinues(t *testing.T) {
	m := New(
		definition(t, "before", `export default p=>p.on('request',e=>({body:e.body+'A'}))`),
		definition(t, "broken", `export default p=>{p.on('request',e=>({body:e.body+'WRONG'}));p.on('request',e=>{e.headers.changed=['wrong'];throw Error('example failure')});p.on('prepared',()=>{throw Error('must be skipped')});p.on('finish',()=>p.log('info','cleaned up'))}`),
		definition(t, "after", `export default p=>p.on('request',e=>({body:e.body+'B'}))`),
	)
	var logs []LogEntry
	s, err := m.Start(t.Context(), Context{RequestID: "request", ClientID: "client", ThreadID: "thread"}, nil, func(entry LogEntry) { logs = append(logs, entry) })
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]any{"original": []string{"kept"}}
	e := map[string]any{"body": "input", "headers": headers}
	if err := s.Emit("request", e); err != nil || e["body"] != "inputAB" || headers["changed"] != nil {
		t.Fatal(e, err)
	}
	if err := s.Emit("prepared", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	s.Finish("complete")
	if len(logs) != 2 || logs[0].ExtensionID != "broken" || logs[0].Hook != "request" || !logs[0].Failed || logs[0].RequestID != "request" || !strings.Contains(logs[0].Message, "example failure") || logs[1].Message != "cleaned up" {
		t.Fatal(logs)
	}
	// Failure isolation is request-scoped; the next request tries the extension.
	other, err := m.Start(t.Context(), Context{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if other.instances[1].failed {
		t.Fatal("failure permanently disabled extension")
	}
}
func TestInvalidPatchRollsBackAndSyntheticResponseStillShortCircuits(t *testing.T) {
	m := New(definition(t, "broken", `export default p=>p.on('request',()=>({body:42}))`), definition(t, "local", `export default p=>p.on('request',e=>({response:{body:e.body}}))`), definition(t, "last", `export default p=>p.on('request',()=>{throw Error('must not run')})`))
	s := start(t, m, Context{})
	e := map[string]any{"body": "original"}
	err := s.Transform("request", e, func(candidate map[string]any) error {
		if _, ok := candidate["body"].(string); !ok {
			return context.Canceled
		}
		return nil
	})
	if err != nil || e["response"].(map[string]any)["body"] != "original" || m.latest["broken"].Hook != "request" || s.instances[2].failed {
		t.Fatal(e, err)
	}
}
func TestAuthoredLogsAreAttributedBoundedAndRedacted(t *testing.T) {
	m := New(definition(t, "logging", `export const settings={fields:[{key:'secret',type:'password',label:'Secret',default:'private-example'}]};export default p=>{
  p.log('debug','initializing');
  p.on('client_event',e=>{
   p.log('info','added usage '+p.getSettings().secret,{password:'hidden',nested:{authorization:'hidden'},requestId:'cannot override',responseId:'response-example',secret:p.getSettings().secret});
   for(let i=0;i<9;i++)p.log('warning','example warning');
   return {event:e.event};
  });
 }`))
	var logs []LogEntry
	s, err := m.Start(t.Context(), Context{RequestID: "authoritative", ClientID: "client"}, nil, func(entry LogEntry) { logs = append(logs, entry) })
	if err != nil {
		t.Fatal(err)
	}
	// Quotas span streamed invocations; each individual hook remains within its
	// execution budget even under race instrumentation.
	for i := 0; i < 12; i++ {
		e := map[string]any{"event": map[string]any{"type": "response.output_text.delta"}}
		if err := s.Emit("client_event", e); err != nil || s.instances[0].failed {
			t.Fatal(e, err, m.latest)
		}
	}
	if len(logs) != MaxRequestLogs+1 {
		t.Fatal(len(logs))
	}
	if logs[0].Hook != "initialize" || logs[1].RequestID != "authoritative" || logs[1].Hook != "client_event" || logs[1].Level != "INFO" || logs[1].Fields["password"] != "[REDACTED]" || strings.Contains(logs[1].Message, "private-example") || logs[1].Fields["secret"] != "[REDACTED]" {
		t.Fatal(logs[:2])
	}
	if !strings.Contains(logs[len(logs)-1].Message, "log limit reached") {
		t.Fatal(logs[len(logs)-1])
	}
	for _, entry := range logs {
		if err := entry.Validate(); err != nil {
			t.Fatal(entry, err)
		}
	}
}
func TestExceptionRenderingAndSinkCannotBreakProxy(t *testing.T) {
	m := New(definition(t, "render", `export default p=>p.on('request',()=>{throw {toString(){while(true){}}}})`))
	s, err := m.Start(t.Context(), Context{}, nil, func(LogEntry) { panic("sink failure") })
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	e := map[string]any{"body": "kept"}
	if err := s.Emit("request", e); err != nil || e["body"] != "kept" {
		t.Fatal(e, err)
	}
	if time.Since(before) > 2*time.Second {
		t.Fatal("error rendering was unbounded")
	}
	if m.latest["render"].Message != "Extension error details could not be rendered" {
		t.Fatal(m.latest)
	}
}
func TestBrokenModuleDoesNotPreventHealthyModulesLoading(t *testing.T) {
	dir := t.TempDir()
	for name, source := range map[string]string{"bad.ts": `throw Error('module failed')`, "healthy.ts": `export default p=>p.on('request',()=>({body:'healthy'}))`, "Invalid Name.ts": `broken syntax !!!`} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var logs []LogEntry
	s, err := m.Start(t.Context(), Context{}, nil, func(entry LogEntry) { logs = append(logs, entry) })
	if err != nil {
		t.Fatal(err)
	}
	e := map[string]any{}
	if err = s.Emit("request", e); err != nil || e["body"] != "healthy" || len(logs) != 2 {
		t.Fatal(e, logs, err)
	}
	for _, entry := range logs {
		if entry.Hook != "load" || entry.Validate() != nil {
			t.Fatal(entry)
		}
	}
}

type panicMetrics struct{ scrub bool }

func (p panicMetrics) Scrub(int, int) {
	if p.scrub {
		panic("scrub failed")
	}
}
func (panicMetrics) RejectScrub()           {}
func (panicMetrics) NativeCompactionArmed() { panic("handoff failed") }
func TestNativeFailurePreservesRequestAndTerminalUsage(t *testing.T) {
	m := New()
	s := start(t, m, Context{})
	body := []byte(`{"input":[{"role":"user","content":"` + strings.Repeat("x", 2000) + `"},{"role":"user","content":"current"}]}`)
	output, problem := s.Prepare("/v1/responses", body, requestsize.ScrubPolicy{TriggerBytes: 1000, TargetBytes: 500}, panicMetrics{scrub: true})
	if problem != nil || string(output) != string(body) || m.latest[requestsize.ID].Hook != "prepared" {
		t.Fatal(string(output), problem, m.latest)
	}
	s = start(t, m, Context{})
	_, problem = s.Prepare("/v1/responses", body, requestsize.ScrubPolicy{TriggerBytes: 1000, TargetBytes: 500}, panicMetrics{})
	if problem != nil {
		t.Fatal(problem)
	}
	original := map[string]any{"type": "response.completed", "response": map[string]any{"usage": map[string]any{"input_tokens": float64(77)}}}
	nextRan := false
	transformed, err := s.ClientEvent(func(event map[string]any) (map[string]any, error) { nextRan = true; return event, nil })(original)
	if err != nil || !nextRan || transformed["response"].(map[string]any)["usage"].(map[string]any)["input_tokens"] != float64(77) || m.latest[requestsize.ID].Hook != "client_event" {
		t.Fatal(transformed, err, m.latest)
	}
}

func TestChangedPasswordsAndLargeErrorsAreSanitized(t *testing.T) {
	m := New(definition(t, "secrets", `export const settings={fields:[{key:'secret',type:'password',label:'Secret',default:'previous-secret'}]};export default p=>{let initial=p.getSettings().secret;p.on('settings_changed',()=>{throw Error(initial+' '+p.getSettings().secret+' '+'<'.repeat(20000))})}`))
	var logs []LogEntry
	s, err := m.Start(t.Context(), Context{}, nil, func(entry LogEntry) { logs = append(logs, entry) })
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RefreshSettings(t.Context(), func(context.Context) (Configuration, error) {
		return Configuration{"secrets": settings.Values{"secret": "current-secret"}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Emit("request", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || strings.Contains(logs[0].Message, "previous-secret") || strings.Contains(logs[0].Message, "current-secret") || logs[0].Validate() != nil {
		t.Fatal(logs)
	}
}
