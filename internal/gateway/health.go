package gateway

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/logbook"
	"github.com/NotRllyRn/codex-broker/internal/responsesproxy"
)

const readinessCooldown = 30 * time.Second

// Ready is passive: a timeout temporarily removes this process from routing.
// The cooldown expires so a primary behind a backup can receive traffic again.
func (e *Engine) Ready() bool {
	until := e.unreadyUntil.Load()
	if until == 0 {
		return true
	}
	if time.Now().UnixNano() < until {
		return false
	}
	if e.unreadyUntil.CompareAndSwap(until, 0) {
		e.log("INFO", "proxy.ready", "Proxy readiness cooldown elapsed; accepting traffic again.")
	}
	return e.unreadyUntil.Load() == 0
}

func (e *Engine) failure(r *http.Request, requestID, accountID, clientID, stage string, started int64, err error) {
	if r.Context().Err() != nil {
		return
	}
	var timeout net.Error
	stalled := errors.Is(err, responsesproxy.ErrIdleReadTimeout) || errors.As(err, &timeout) && timeout.Timeout()
	cooldown := "none"
	if stalled {
		e.unreadyUntil.Store(time.Now().Add(readinessCooldown).UnixNano())
		cooldown = readinessCooldown.String()
	}
	// Do not log request bodies, headers, or leased credentials.
	message := fmt.Sprintf("request_id=%s account_id=%s client_id=%s path=%s stage=%s elapsed_ms=%d timeout=%t readiness_cooldown=%s error=%s", requestID, accountID, clientID, r.URL.Path, stage, time.Now().UnixMilli()-started, stalled, cooldown, logbook.Redact(err.Error()))
	e.log("ERROR", "proxy.failure", message)
}

func (e *Engine) log(level, event, message string) {
	if e.Log != nil {
		e.Log(level, event, message)
		return
	}
	if level == "ERROR" {
		slog.Error(event + " " + message)
	} else {
		slog.Info(event + " " + message)
	}
}

type observedBody struct {
	io.ReadCloser
	once    sync.Once
	onError func(error)
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		b.once.Do(func() { b.onError(err) })
	}
	return n, err
}
