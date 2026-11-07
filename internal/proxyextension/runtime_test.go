package proxyextension

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/store"
)

func definition(t *testing.T, id, source string) Definition {
	t.Helper()
	d, err := Compile(id, source)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func start(t *testing.T, m *Manager, id Context) *Session {
	t.Helper()
	s, err := m.Start(t.Context(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestTypeScriptCompositionAndIsolation(t *testing.T) {
	a := definition(t, "first", `export default (p: any) => { let n: number=0;p.on('request',(e:any)=>({body:e.body+'a'+(++n)})); }`)
	b := definition(t, "second", `export default p => p.on('request',e=>({body:e.body+'b'}))`)
	m := New(a, b)
	for i := 0; i < 2; i++ {
		s := start(t, m, Context{})
		event := map[string]any{"body": "x"}
		if err := s.Emit("request", event); err != nil {
			t.Fatal(err)
		}
		if event["body"] != "xa1b" {
			t.Fatal(event)
		}
	}
	s, err := m.Start(t.Context(), Context{}, map[string]bool{"first": true, UsageID: true})
	if err != nil {
		t.Fatal(err)
	}
	event := map[string]any{"body": "x"}
	_ = s.Emit("request", event)
	if event["body"] != "xb" || s.Usage {
		t.Fatal(event, s.Usage)
	}
	// The enabled snapshot is immutable for the lifetime of an active request.
	if !start(t, m, Context{}).Usage {
		t.Fatal("default usage disabled")
	}
}
func TestShortCircuit(t *testing.T) {
	m := New(definition(t, "local", `export default p=>{p.on('request',()=>({response:{status:200,headers:{},body:'local'}}));p.on('request',()=>{throw Error('must not run')})}`))
	e := map[string]any{}
	if err := start(t, m, Context{}).Emit("request", e); err != nil || e["response"] == nil {
		t.Fatal(e, err)
	}
}
func TestBoundedFailures(t *testing.T) {
	for name, source := range map[string]string{
		"throw":   `export default p=>p.on('request',()=>{throw Error('private content')})`,
		"loop":    `export default p=>p.on('request',()=>{while(true){}})`,
		"promise": `export default p=>p.on('request',async()=>({body:'no'}))`,
		"cycle":   `export default p=>p.on('request',()=>{const a={};a.self=a;return a})`,
		"getter":  `export default p=>p.on('request',()=>({get body(){while(true){}}}))`,
	} {
		t.Run(name, func(t *testing.T) {
			s := start(t, New(definition(t, name, source)), Context{})
			before := time.Now()
			event := map[string]any{"body": "unchanged"}
			if err := s.Emit("request", event); err != nil || event["body"] != "unchanged" || !s.instances[0].failed {
				t.Fatal(event, err)
			}
			if s.manager.latest[name].Hook != "request" {
				t.Fatal("failure was not recorded")
			}
			if time.Since(before) > 2*time.Second {
				t.Fatal("hook not interrupted")
			}
		})
	}
	if _, err := Compile("bad", `while(true){}`); err == nil {
		t.Fatal("accepted unbounded module initialization")
	}
	for _, source := range []string{`export default async p=>{}`, `export default p=>p.on('unknown',()=>{})`, `export default p=>require('fs')`} {
		m := New(definition(t, "bad", source))
		session, err := m.Start(t.Context(), Context{}, nil)
		if err != nil || session.Active() || m.latest["bad"].Hook != "initialize" {
			t.Fatal("invalid factory was not isolated", err)
		}
	}
}
func TestCancellationAndFinish(t *testing.T) {
	m := New(definition(t, "cancel", `export default p=>{p.on('request',()=>{while(true){}});p.on('finish',()=>p.updateThread(()=>42));p.on('prepared',()=>({body:String(p.updateThread(x=>x))}));}`))
	ctx, cancel := context.WithCancel(t.Context())
	s, err := m.Start(ctx, Context{ClientID: "client", ThreadID: "thread"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(10*time.Millisecond, cancel)
	if err := s.Emit("request", map[string]any{}); err == nil {
		t.Fatal("cancel did not interrupt")
	}
	s.Finish("cancelled")
	other := start(t, m, Context{ClientID: "client", ThreadID: "thread"})
	e := map[string]any{}
	if err := other.Emit("prepared", e); err != nil || e["body"] != "42" {
		t.Fatal(e, err)
	}
}
func TestAtomicThreadState(t *testing.T) {
	m := New(definition(t, "count", `export default p=>{p.on('request',()=>({body:String(p.updateThread(n=>(n??0)+1))}));}`))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := m.Start(t.Context(), Context{ClientID: "client", ThreadID: "thread"}, nil)
			if err == nil {
				err = s.Emit("request", map[string]any{})
			}
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for _, test := range []struct{ client, thread, want string }{{"client", "thread", "21"}, {"other", "thread", "1"}, {"client", "other", "1"}} {
		e := map[string]any{}
		err := start(t, m, Context{ClientID: test.client, ThreadID: test.thread}).Emit("request", e)
		if err != nil || e["body"] != test.want {
			t.Fatal(e, err)
		}
	}
	event := map[string]any{}
	if err := start(t, m, Context{}).Emit("request", event); err != nil || event["body"] != nil || m.latest["count"].Hook != "request" {
		t.Fatal("missing identity was not isolated", event, err)
	}
}
func TestLoadOrderAndSettings(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"second", "first"} {
		if err := os.WriteFile(filepath.Join(dir, id+".ts"), []byte(`export default p=>{}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	list := m.List(nil)
	if len(list) != 4 || list[0].ID != UsageID || list[2].ID != "first" {
		t.Fatal(list)
	}
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, enabled := range []bool{false, false, true, true, false} {
		if err := SetEnabled(t.Context(), db, UsageID, enabled); err != nil {
			t.Fatal(err)
		}
		if err := SetEnabled(t.Context(), db, "first", false); err != nil {
			t.Fatal(err)
		}
		disabled, err := Disabled(t.Context(), db)
		if err != nil || disabled[UsageID] == enabled || !disabled["first"] {
			t.Fatal(disabled, err)
		}
	}
}

func TestFinishRunsAllHandlersAfterFailure(t *testing.T) {
	m := New(definition(t, "cleanup", `export default p=>{p.on('finish',()=>{throw Error('failure')});p.on('finish',()=>p.updateThread(()=>1));p.on('request',()=>({body:String(p.updateThread(x=>x))}));}`))
	identity := Context{ClientID: "client", ThreadID: "thread"}
	start(t, m, identity).Finish("error")
	e := map[string]any{}
	if err := start(t, m, identity).Emit("request", e); err != nil || e["body"] != "1" {
		t.Fatal(e, err)
	}
}

func TestThreadUpdateFailures(t *testing.T) {
	for _, callback := range []string{`null`, `()=>{throw Error('failure')}`, `()=>{while(true){}}`, `async()=>42`} {
		m := New(definition(t, "state", `export default p=>p.on('request',()=>p.updateThread(`+callback+`))`))
		event := map[string]any{}
		if err := start(t, m, Context{ClientID: "client", ThreadID: "thread"}).Emit("request", event); err != nil || m.latest["state"].Hook != "request" || len(m.state) != 0 {
			t.Fatal("invalid update was not isolated", callback, event, err)
		}
	}
}
