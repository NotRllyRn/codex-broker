package httpserver

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/auth"
	"github.com/NotRllyRn/codex-broker/internal/events"
)

type blockedStreamWriter struct {
	*httptest.ResponseRecorder
	started     chan struct{}
	deadline    time.Time
	hadDeadline bool
}

func (w *blockedStreamWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func (w *blockedStreamWriter) Write([]byte) (int, error) {
	w.hadDeadline = !w.deadline.IsZero()
	close(w.started)
	if w.deadline.IsZero() {
		return 0, os.ErrDeadlineExceeded
	}
	time.Sleep(time.Until(w.deadline))
	return 0, os.ErrDeadlineExceeded
}

func TestShutdownEndsDashboardStreams(t *testing.T) {
	s, _ := proxyFixture(t, 0)
	s.App.Events = events.New(10, 10)
	s.App.Events.Publish("state", map[string]any{})
	now := time.Now().UnixMilli()
	const token = "test-session"
	_, err := s.App.Store.DB.Exec("INSERT INTO admin_sessions VALUES(?,?,?,?,?,?,?,?,?)", auth.Digest(token), auth.Digest("csrf"), now, now, now+3600000, now+3600000, nil, nil, auth.Digest("test"))
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{server: s}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.eventStream(w, r); err != nil {
			t.Errorf("eventStream: %v", err)
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	server.Config.RegisterOnShutdown(handler.BeginShutdown)
	client := server.Client()
	client.Timeout = 20 * time.Second
	request, _ := http.NewRequest("GET", server.URL, nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	request.Header.Set("Last-Event-ID", "0")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal("stream did not start")
	}
	if response.ProtoMajor != 2 {
		t.Fatal("expected HTTP/2")
	}
	reader := bufio.NewReader(response.Body)
	readEvent := func() {
		t.Helper()
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("reading event: %v", err)
			}
			if line == "\n" {
				return
			}
		}
	}
	readEvent()
	// A write deadline must not reset a healthy HTTP/2 stream while idle.
	time.Sleep(6 * time.Second)
	s.App.Events.Publish("state", map[string]any{})
	readEvent()
	// Simulate a client whose socket write remains blocked until its deadline.
	blocked := &blockedStreamWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{})}
	blockedDone := make(chan error, 1)
	go func() { blockedDone <- s.eventStream(blocked, request) }()
	select {
	case <-blocked.started:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked stream did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Config.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("stream did not end cleanly: %v", err)
	}
	select {
	case err := <-blockedDone:
		if err != nil {
			t.Fatal(err)
		}
		if !blocked.hadDeadline {
			t.Fatal("stream write has no deadline")
		}
		if !blocked.deadline.IsZero() {
			t.Fatal("write deadline was not cleared")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("blocked write prevented stream shutdown")
	}
	// Repeated shutdown and requests arriving after the signal must not hang.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handler.BeginShutdown()
			if err := s.eventStream(httptest.NewRecorder(), request); err != nil {
				t.Errorf("late stream: %v", err)
			}
		}()
	}
	wg.Wait()
}
