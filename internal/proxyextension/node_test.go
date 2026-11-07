package proxyextension

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNodeImportsAsyncAndDurableDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WINDOWKEEPER_DATA_DIR", root)
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, "node_modules", "example-package"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "lib"), 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"lib/helper.ts": `export const suffix: string = '!';`,
		"node_modules/example-package/package.json": `{"name":"example-package","type":"module","exports":"./index.js"}`,
		"node_modules/example-package/index.js":     `export default 'package';`,
		"persist.ts": `import {readFile,writeFile} from 'node:fs/promises';import {join} from 'node:path';import label from 'example-package';import {suffix} from './lib/helper';
 export default async p=>{let count=0;const path=join(p.getDataDir(),'count.json');p.on('request',async()=>{let value=0;try{value=JSON.parse(await readFile(path,'utf8'))}catch(e){if(e.code!=='ENOENT')throw e}await writeFile(path,JSON.stringify(value+1));console.log('ordinary stdout');return {body:label+suffix+':'+(value+1)+':'+(++count)}})}`,
	}
	for path, body := range files {
		if err := os.WriteFile(filepath.Join(source, path), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"package!:1:1", "package!:2:1"} {
		m, err := Load(source)
		if err != nil {
			t.Fatal(err)
		}
		s := start(t, m, Context{})
		event := map[string]any{}
		if err = s.Emit("request", event); err != nil || event["body"] != want {
			t.Fatal(event, err, m.latest)
		}
		s.Finish("complete")
		m.Close()
	}
	if raw, err := os.ReadFile(filepath.Join(root, "extensions", "persist", "count.json")); err != nil || string(raw) != "2" {
		t.Fatal(string(raw), err)
	}
}

func TestNodeSQLite(t *testing.T) {
	t.Setenv("WINDOWKEEPER_DATA_DIR", t.TempDir())
	m := New(definition(t, "database", `import {DatabaseSync} from 'node:sqlite';import {join} from 'node:path';export default p=>{p.on('request',()=>{const db=new DatabaseSync(join(p.getDataDir(),'history.sqlite'));try{db.exec('CREATE TABLE IF NOT EXISTS history(body TEXT)');db.prepare('INSERT INTO history VALUES (?)').run('example');return {body:String(db.prepare('SELECT count(*) AS n FROM history').get().n)}}finally{db.close()}})}`))
	for _, want := range []string{"1", "2"} {
		s := start(t, m, Context{})
		event := map[string]any{}
		if err := s.Emit("request", event); err != nil || event["body"] != want {
			t.Fatal(event, err, m.latest)
		}
		s.Finish("complete")
	}
}

func TestWorkerCrashIsolationRestartAndThreadState(t *testing.T) {
	m := New(definition(t, "crash", `export default p=>{p.on('request',e=>{if(e.body==='crash')process.exit(1);return {body:String(p.updateThread(n=>(n??0)+1))}})}`), definition(t, "healthy", `export default p=>p.on('request',e=>({body:e.body+'!'}))`))
	id := Context{ClientID: "client", ThreadID: "thread"}
	first := start(t, m, id)
	old := start(t, m, id)
	event := map[string]any{}
	_ = first.Emit("request", event)
	if event["body"] != "1!" {
		t.Fatal(event)
	}
	event = map[string]any{"body": "crash"}
	_ = first.Emit("request", event)
	if event["body"] != "crash!" {
		t.Fatal(event)
	}
	next := start(t, m, id)
	event = map[string]any{}
	_ = next.Emit("request", event)
	if event["body"] != "2!" {
		t.Fatal(event, m.latest)
	}
	event = map[string]any{"body": "old"}
	_ = old.Emit("request", event)
	if event["body"] != "old!" || !old.instances[0].failed {
		t.Fatal(event)
	}
	first.Finish("complete")
	old.Finish("complete")
	next.Finish("complete")
}

func TestCancellationWhileQueuedDoesNotKillOtherRequest(t *testing.T) {
	m := New(definition(t, "queue", `export default p=>p.on('request',async e=>{await new Promise(r=>setTimeout(r,30));return {body:e.body+'!'}})`))
	first := start(t, m, Context{})
	ctx, cancel := context.WithCancel(t.Context())
	second, err := m.Start(ctx, Context{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	worker := first.instances[0].worker
	worker.gate <- struct{}{}
	cancel()
	if err = second.Emit("request", map[string]any{}); err != context.Canceled {
		t.Fatal(err)
	}
	<-worker.gate
	event := map[string]any{"body": "kept"}
	if err = first.Emit("request", event); err != nil || event["body"] != "kept!" {
		t.Fatal(event, err)
	}
	second.Finish("cancelled")
	first.Finish("complete")
}

func TestNoNodeRequiredForNativeExtensions(t *testing.T) {
	t.Setenv("WINDOWKEEPER_EXTENSION_NODE", "/nonexistent-example-node")
	m, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	s := start(t, m, Context{})
	if !s.Usage || s.Active() {
		t.Fatal("native fast path changed")
	}
	_, err = Compile("example", `export default p=>{}`)
	if err == nil || !strings.Contains(err.Error(), "start Node") {
		t.Fatal(err)
	}
}

func TestWorkerReleasesRequestClosures(t *testing.T) {
	m := New(definition(t, "release", `export default p=>p.on('request',()=>({body:'ok'}))`))
	s := start(t, m, Context{})
	s.Finish("complete")
	event := map[string]any{"body": "unchanged"}
	_ = s.Emit("request", event)
	if event["body"] != "unchanged" || !s.instances[0].failed {
		t.Fatal(event)
	}
}

func TestAsyncTimeoutRecoversOnNextRequest(t *testing.T) {
	m := New(definition(t, "timeout", `export default p=>p.on('request',async e=>{if(e.body==='hang')await new Promise(()=>{});return {body:'recovered'}})`))
	s := start(t, m, Context{})
	event := map[string]any{"body": "hang"}
	before := time.Now()
	_ = s.Emit("request", event)
	if event["body"] != "hang" || time.Since(before) > 2*time.Second {
		t.Fatal(event)
	}
	next := start(t, m, Context{})
	event = map[string]any{}
	_ = next.Emit("request", event)
	if event["body"] != "recovered" {
		t.Fatal(event)
	}
	s.Finish("error")
	next.Finish("complete")
}

func TestHooksRegisteredDuringInvocationRemainAvailable(t *testing.T) {
	m := New(definition(t, "registration", `export default p=>{p.on('request',()=>{p.on('body',()=>({body:'registered'}));p.on('finish',()=>p.updateThread(()=>42))})}`))
	s := start(t, m, Context{ClientID: "client", ThreadID: "thread"})
	if err := s.Emit("request", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if !s.Has("body") {
		t.Fatal("new hook was not published")
	}
	event := map[string]any{}
	_ = s.Emit("body", event)
	if event["body"] != "registered" {
		t.Fatal(event)
	}
	s.Finish("complete")
	if m.state["registration\x00client\x00thread"] != "42" {
		t.Fatal(m.state)
	}
}

func TestLongLivedWorkerKeepsRequestModulesSeparateAndCloses(t *testing.T) {
	m := New(definition(t, "lifetime", `let n=0;export default p=>p.on('request',()=>({body:process.pid+':'+(++n)}))`))
	var previous string
	for i := 0; i < 2; i++ {
		s := start(t, m, Context{})
		event := map[string]any{}
		_ = s.Emit("request", event)
		body, ok := event["body"].(string)
		if !ok || !strings.HasSuffix(body, ":1") || (i > 0 && body != previous) {
			t.Fatal(event)
		}
		previous = body
		s.Finish("complete")
	}
	process := m.worker("lifetime").process
	m.Close()
	if err := process.cmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Fatal("worker was not reaped")
	}
}

func TestBackgroundLogsCannotBeAttributedToAnotherRequest(t *testing.T) {
	m := New(definition(t, "background", `export default p=>{p.on('request',()=>{setTimeout(()=>p.log('error','late log'),5)});p.on('body',async()=>{await new Promise(r=>setTimeout(r,20));p.log('info','active log')})}`))
	t.Cleanup(m.Close)
	var logs []LogEntry
	sink := func(entry LogEntry) { logs = append(logs, entry) }
	first, err := m.Start(t.Context(), Context{RequestID: "first"}, nil, sink)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Start(t.Context(), Context{RequestID: "second"}, nil, sink)
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Emit("request", map[string]any{})
	_ = second.Emit("body", map[string]any{})
	first.Finish("complete")
	second.Finish("complete")
	if len(logs) != 1 || logs[0].Message != "active log" || logs[0].RequestID != "second" {
		t.Fatal(logs)
	}
}
