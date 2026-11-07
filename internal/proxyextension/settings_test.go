package proxyextension

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/NotRllyRn/codex-broker/extensions/requestsize"
	"github.com/NotRllyRn/codex-broker/extensions/settings"
	"github.com/NotRllyRn/codex-broker/internal/responsesproxy"
)

func TestSettingsLifecycle(t *testing.T) {
	m := New(definition(t, "configurable", `
 export const settings={fields:[{key:'value',type:'text',label:'Value',default:'default'}]};
 export default p=>{
  let history=[];
  p.on('settings_loaded',e=>{history.push('loaded:'+e.settings.value); e.settings.value='mutated'; p.getSettings().value='mutated';});
  p.on('settings_changed',e=>{history.push(e.previous.value+'>'+e.settings.value); return {settings:{value:'ignored'}};});
  p.on('request',()=>({body:history.join(',')+'|'+p.getSettings().value}));
 }`))
	s := start(t, m, Context{})
	check := func(want string) {
		t.Helper()
		e := map[string]any{}
		if err := s.Emit("request", e); err != nil || e["body"] != want {
			t.Fatal(e, err, want)
		}
	}
	check("loaded:default|default")
	read := func(context.Context) (Configuration, error) {
		return Configuration{"configurable": settings.Values{"value": "updated"}}, nil
	}
	if err := m.RefreshSettings(t.Context(), read); err != nil {
		t.Fatal(err)
	}
	check("loaded:default,default>updated|updated")
	check("loaded:default,default>updated|updated")
	e := map[string]any{}
	if err := start(t, m, Context{}).Emit("request", e); err != nil || e["body"] != "loaded:updated|updated" {
		t.Fatal(e, err)
	}
	if err := m.RefreshSettings(t.Context(), func(context.Context) (Configuration, error) {
		return Configuration{"configurable": settings.Values{"value": false}}, nil
	}); err == nil {
		t.Fatal("invalid refresh")
	}
	check("loaded:default,default>updated|updated")
}
func TestSettingsDiscoveryErrors(t *testing.T) {
	for _, source := range []string{
		`export const settings={fields:[{key:'x',type:'unknown',label:'X',default:''}]};export default p=>{}`,
		`export const settings={fields:[{key:'x',type:'number',label:'X',default:'bad'}]};export default p=>{}`,
		`export const settings={get fields(){while(true){}}};export default p=>{}`,
		`throw Error('private');`,
	} {
		if _, err := Compile("invalid", source); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal(err)
		}
	}
}
func TestConcurrentSettingsPublication(t *testing.T) {
	m := New(definition(t, "concurrent", `export const settings={fields:[{key:'n',type:'integer',label:'N',default:0}]};export default p=>p.on('request',()=>({body:String(p.getSettings().n)}))`))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if err := m.RefreshSettings(t.Context(), func(context.Context) (Configuration, error) {
					return Configuration{"concurrent": settings.Values{"n": i}}, nil
				}); err != nil {
					t.Error(err)
					return
				}
				s, err := m.Start(t.Context(), Context{}, nil)
				if err != nil {
					t.Error(err)
					return
				}
				if err = s.Emit("request", map[string]any{}); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestNativeSettingsSnapshot(t *testing.T) {
	m := New()
	original := start(t, m, Context{})
	notice := responsesproxy.Notice{AccountID: "account", AccountLabel: "Personal usage", Status: "retrying"}
	before := original.UsageNotice(notice)
	body := []byte(`{"input":[{"type":"message","role":"user","content":"old"},{"type":"function_call","call_id":"call","name":"shell","arguments":"{}"},{"type":"function_call_output","call_id":"call","output":"` + strings.Repeat("x", 3<<20) + `"},{"type":"message","role":"user","content":"current"}]}`)
	if output, problem := original.Prepare("/v1/responses", body, requestsize.ScrubPolicy{}, nil); problem != nil || !bytes.Equal(output, body) {
		t.Fatal("defaults changed", problem)
	}
	if err := m.RefreshSettings(t.Context(), func(context.Context) (Configuration, error) {
		return Configuration{
			UsageID:        settings.Values{"showAccountLabel": false, "showRetryStatus": false},
			requestsize.ID: settings.Values{"triggerMiB": 2, "targetMiB": 1},
		}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := original.UsageNotice(notice); got != before {
		t.Fatal("active native snapshot changed")
	}
	next := start(t, m, Context{})
	if got := next.UsageNotice(notice); strings.Contains(got, "Personal usage") || strings.Contains(got, "retrying") || !strings.Contains(got, "Usage stats") {
		t.Fatal(got)
	}
	output, problem := next.Prepare("/v1/responses", body, requestsize.ScrubPolicy{}, nil)
	if problem != nil || len(output) >= 1<<20 || !bytes.Contains(output, []byte("current")) {
		t.Fatal(len(output), problem)
	}
}

func TestSettingsStayConsistentDuringHook(t *testing.T) {
	m := New(definition(t, "snapshot", `export const settings={fields:[{key:'value',type:'text',label:'Value',default:'before'}]};export default p=>p.on('request',()=>{const before=p.getSettings().value;publish();return {body:before+'>'+p.getSettings().value}})`))
	s := start(t, m, Context{})
	if err := s.instances[0].vm.Set("publish", func() {
		if err := m.RefreshSettings(t.Context(), func(context.Context) (Configuration, error) {
			return Configuration{"snapshot": settings.Values{"value": "after"}}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"before>before", "after>after"} {
		e := map[string]any{}
		if err := s.Emit("request", e); err != nil || e["body"] != want {
			t.Fatal(e, err, want)
		}
	}
}
func TestSettingsHookFailures(t *testing.T) {
	for _, hook := range []string{"settings_loaded", "settings_changed"} {
		t.Run(hook, func(t *testing.T) {
			m := New(definition(t, "failure", `export const settings={fields:[{key:'value',type:'text',label:'Value',default:'before'}]};export default p=>p.on('`+hook+`',()=>{throw Error('private setting')})`))
			s, err := m.Start(t.Context(), Context{}, nil)
			if hook == "settings_changed" {
				if err != nil {
					t.Fatal(err)
				}
				if err = m.RefreshSettings(t.Context(), func(context.Context) (Configuration, error) {
					return Configuration{"failure": settings.Values{"value": "after"}}, nil
				}); err != nil {
					t.Fatal(err)
				}
				err = s.Emit("request", map[string]any{})
			}
			if err == nil || err.Error() != "extension failure hook "+hook+" failed" {
				t.Fatal(err)
			}
		})
	}
}
