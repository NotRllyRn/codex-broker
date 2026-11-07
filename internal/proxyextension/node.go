package proxyextension

import (
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

//go:embed worker.cjs
var nodeSource string

// Each extension has one serialized, long-lived worker. Holding the token across
// an invocation preserves synchronous updateThread without an async API change.
type nodeWorker struct {
	*workerState
}
type workerState struct {
	gate       chan struct{}
	process    *nodeProcess
	generation uint64
}
type nodeProcess struct {
	cmd           *exec.Cmd
	input, output *os.File
	once          sync.Once
}

func (p *nodeProcess) close() {
	p.once.Do(func() { _ = p.cmd.Process.Kill(); _ = p.input.Close(); _ = p.output.Close(); _ = p.cmd.Wait() })
}
func newWorker() *nodeWorker {
	state := &workerState{gate: make(chan struct{}, 1)}
	w := &nodeWorker{workerState: state}
	runtime.AddCleanup(w, func(state *workerState) {
		if state.process != nil {
			state.process.close()
		}
	}, state)
	return w
}
func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > MaxPayload {
		return errors.New("hook input exceeds limit")
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(b)))
	if _, err = w.Write(size[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
func readFrame(r io.Reader) (map[string]json.RawMessage, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n > MaxPayload {
		return nil, errors.New("hook output exceeds limit")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	var result map[string]json.RawMessage
	err := json.Unmarshal(b, &result)
	return result, err
}
func startNode(ctx context.Context) (*nodeProcess, error) {
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer inputRead.Close()
	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		inputWrite.Close()
		return nil, err
	}
	defer outputWrite.Close()
	executable := os.Getenv("WINDOWKEEPER_EXTENSION_NODE")
	if executable == "" {
		executable = "node"
	}
	cmd := exec.Command(executable, "--max-old-space-size=512", "-e", nodeSource)
	cmd.ExtraFiles = []*os.File{inputRead, outputWrite}
	// stdout/stderr are intentionally not the protocol or the structured log sink.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		inputWrite.Close()
		outputRead.Close()
		return nil, fmt.Errorf("start Node extension runtime: %w", err)
	}
	p := &nodeProcess{cmd: cmd, input: inputWrite, output: outputRead}
	done := make(chan error, 1)
	go func() {
		reply, err := readFrame(outputRead)
		if err == nil && string(reply["ready"]) != "true" {
			err = errors.New("invalid Node handshake")
		}
		done <- err
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = errors.New("Node startup timeout")
	}
	if err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}
func (w *nodeWorker) call(ctx context.Context, generation uint64, command map[string]any, onMessage func(map[string]json.RawMessage) (any, error)) (map[string]json.RawMessage, uint64, error) {
	select {
	case w.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	defer func() { <-w.gate; runtime.KeepAlive(w) }()
	if err := ctx.Err(); err != nil {
		return nil, generation, err
	}
	if generation != 0 && (w.process == nil || generation != w.generation) {
		return nil, w.generation, errors.New("extension worker restarted; request instance lost")
	}
	if w.process == nil {
		p, err := startNode(ctx)
		if err != nil {
			return nil, w.generation, err
		}
		w.process = p
		w.generation++
	}
	p := w.process
	type outcome struct {
		reply map[string]json.RawMessage
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		if err := writeFrame(p.input, command); err != nil {
			done <- outcome{err: err}
			return
		}
		for {
			reply, err := readFrame(p.output)
			if err != nil {
				done <- outcome{err: err}
				return
			}
			if string(reply["kind"]) == `"result"` {
				done <- outcome{reply: reply}
				return
			}
			if onMessage == nil {
				done <- outcome{err: errors.New("unexpected Node message")}
				return
			}
			response, err := onMessage(reply)
			if err != nil {
				done <- outcome{err: err}
				return
			}
			if response != nil {
				if err = writeFrame(p.input, response); err != nil {
					done <- outcome{err: err}
					return
				}
			}
		}
	}()
	budget := HookTimeout
	if command["op"] == "schema" || command["op"] == "start" {
		budget = time.Second
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	var result outcome
	select {
	case result = <-done:
	case <-ctx.Done():
		result.err = ctx.Err()
		p.close()
		<-done
	case <-timer.C:
		result.err = errors.New("extension hook timeout")
		p.close()
		<-done
	}
	if result.err != nil {
		p.close()
		w.process = nil
		return nil, w.generation, fmt.Errorf("Node extension invocation failed: %w", result.err)
	}
	var message string
	if json.Unmarshal(result.reply["error"], &message) == nil && message != "" {
		return result.reply, w.generation, errors.New(message)
	}
	return result.reply, w.generation, nil
}
func (w *nodeWorker) close() {
	w.gate <- struct{}{}
	defer func() { <-w.gate }()
	if w.process != nil {
		w.process.close()
		w.process = nil
	}
}
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.workersMu.Lock()
	defer m.workersMu.Unlock()
	for _, w := range m.workers {
		w.close()
	}
}
func (m *Manager) worker(id string) *nodeWorker {
	m.workersMu.Lock()
	defer m.workersMu.Unlock()
	if m.workers == nil {
		m.workers = map[string]*nodeWorker{}
	}
	if m.workers[id] == nil {
		m.workers[id] = newWorker()
	}
	return m.workers[id]
}
func dataDirectory(base, id string) string {
	if base == "" {
		base = os.Getenv("WINDOWKEEPER_DATA_DIR")
	}
	if base == "" {
		base = ".windowkeeper/data"
	}
	path, _ := filepath.Abs(filepath.Join(base, "extensions", id))
	return path
}
