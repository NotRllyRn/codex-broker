package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/core"
)

const MaxFrame = 8 * 1024 * 1024

type response struct {
	Result map[string]any `json:"result"`
	Error  any            `json:"error"`
}

type Client struct {
	command       *exec.Cmd
	stdin         io.WriteCloser
	reader        *bufio.Reader
	writeMu       sync.Mutex
	stateMu       sync.Mutex
	nextID        int64
	pending       map[int64]chan response
	notifications chan map[string]any
	done          chan struct{}
	terminal      error
	closeOnce     sync.Once
}

func Spawn(ctx context.Context, executable, cwd string, environment []string) (*Client, error) {
	command := exec.Command(executable, "app-server")
	command.Dir, command.Env = cwd, environment
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, core.NewError("CODEX_START_FAILED", "Codex app-server could not start", 503)
	}
	client := &Client{
		command: command, stdin: stdin, reader: bufio.NewReaderSize(stdout, 64*1024),
		pending: map[int64]chan response{}, notifications: make(chan map[string]any, 256), done: make(chan struct{}),
	}
	go client.readLoop()
	go io.Copy(io.Discard, io.LimitReader(stderr, 1<<30))
	initialize, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := client.Request(initialize, "initialize", map[string]any{"clientInfo": map[string]any{"name": "codex-broker", "version": "0.1.0"}, "capabilities": map[string]any{}}); err != nil {
		_ = client.Close()
		return nil, err
	}
	if err := client.Notify(initialize, "initialized", map[string]any{}); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

func (c *Client) readLoop() {
	for {
		var line []byte
		for {
			fragment, err := c.reader.ReadSlice('\n')
			if len(line)+len(fragment) > MaxFrame {
				c.fail(core.NewError("CODEX_FRAME_TOO_LARGE", "Codex returned an oversized frame", 502))
				return
			}
			line = append(line, fragment...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					c.fail(err)
				} else {
					c.fail(core.NewError("CODEX_TRANSPORT_CLOSED", "Codex app-server transport closed", 503))
				}
				return
			}
			break
		}
		var raw map[string]any
		if json.Unmarshal(line, &raw) != nil {
			c.fail(core.NewError("CODEX_INVALID_FRAME", "Codex returned an invalid frame", 502))
			return
		}
		if id, ok := raw["id"].(float64); ok {
			c.stateMu.Lock()
			waiter := c.pending[int64(id)]
			delete(c.pending, int64(id))
			c.stateMu.Unlock()
			if waiter != nil {
				var result response
				encoded, _ := json.Marshal(raw)
				_ = json.Unmarshal(encoded, &result)
				waiter <- result
			}
			continue
		}
		select {
		case c.notifications <- raw:
		default:
			select {
			case <-c.notifications:
			default:
			}
			select {
			case c.notifications <- raw:
			default:
			}
		}
	}
}

func (c *Client) fail(err error) {
	c.closeOnce.Do(func() {
		c.stateMu.Lock()
		c.terminal = err
		for id, waiter := range c.pending {
			delete(c.pending, id)
			close(waiter)
		}
		close(c.done)
		c.stateMu.Unlock()
	})
}

func (c *Client) Request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	c.stateMu.Lock()
	if c.terminal != nil {
		err := c.terminal
		c.stateMu.Unlock()
		return nil, err
	}
	c.nextID++
	id := c.nextID
	waiter := make(chan response, 1)
	c.pending[id] = waiter
	c.stateMu.Unlock()
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		c.removePending(id)
		return nil, err
	}
	select {
	case value, ok := <-waiter:
		if !ok {
			c.stateMu.Lock()
			err := c.terminal
			c.stateMu.Unlock()
			return nil, err
		}
		if value.Error != nil {
			text, _ := json.Marshal(value.Error)
			if authError(string(text)) {
				return nil, core.NewError("CODEX_AUTH_REQUIRED", "Codex authentication must be renewed", 503)
			}
			return nil, core.NewError("CODEX_RPC_REJECTED", "Codex rejected the request", 502)
		}
		if value.Result == nil {
			value.Result = map[string]any{}
		}
		return value.Result, nil
	case <-ctx.Done():
		c.removePending(id)
		return nil, ctx.Err()
	case <-c.done:
		c.stateMu.Lock()
		err := c.terminal
		c.stateMu.Unlock()
		return nil, err
	}
}

func (c *Client) removePending(id int64) {
	c.stateMu.Lock()
	delete(c.pending, id)
	c.stateMu.Unlock()
}

func (c *Client) Notify(ctx context.Context, method string, params map[string]any) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return c.write(map[string]any{"method": method, "params": params})
}

func (c *Client) write(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(payload)
	return err
}

func (c *Client) Notifications() <-chan map[string]any { return c.notifications }
func (c *Client) Done() <-chan struct{}                { return c.done }
func (c *Client) Err() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.terminal
}

func (c *Client) Close() error {
	if c.command.Process == nil {
		return nil
	}
	_ = syscall.Kill(-c.command.Process.Pid, syscall.SIGTERM)
	wait := make(chan error, 1)
	go func() { wait <- c.command.Wait() }()
	select {
	case <-wait:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-c.command.Process.Pid, syscall.SIGKILL)
		<-wait
	}
	c.fail(core.NewError("CODEX_TRANSPORT_CLOSED", "Codex app-server transport closed", 503))
	return nil
}

func authError(value string) bool {
	value = stringLower(value)
	for _, marker := range []string{"unauthorized", "authentication required", "authentication_required", "invalid_grant", "refresh_token_reused", "refresh_token_expired", "token_invalidated", "refresh token was already used", "sign in again", "log out and sign in again"} {
		if contains(value, marker) {
			return true
		}
	}
	return false
}

func stringLower(value string) string {
	b := []byte(value)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}

func (c *Client) String() string {
	return fmt.Sprintf("codex app-server pid=%d", c.command.Process.Pid)
}
