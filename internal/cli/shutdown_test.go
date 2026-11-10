package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestRunServerGracefulDrain(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		name := "complete"
		if timeout {
			name = "timeout"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			defer close(release)
			shuttingDown := make(chan struct{})
			address := make(chan string, 1)
			server := &http.Server{
				Addr: "127.0.0.1:0",
				BaseContext: func(listener net.Listener) context.Context {
					address <- listener.Addr().String()
					return context.Background()
				},
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer close(finished)
					close(started)
					select {
					case <-release:
					case <-shuttingDown:
						if timeout {
							<-release
						} else {
							// Shutdown must leave the request context and dependencies usable.
							if err := r.Context().Err(); err != nil {
								t.Errorf("request canceled during drain: %v", err)
							}
							_, _ = w.Write([]byte("complete"))
						}
					}
				}),
			}
			server.RegisterOnShutdown(func() { close(shuttingDown) })
			defer server.Close()
			result := make(chan error, 1)
			cleaned := make(chan struct{})
			go func() {
				// Model serve's deferred handler/application cleanup: only after drain.
				defer close(cleaned)
				result <- runServer(ctx, server, "", "", 200*time.Millisecond)
			}()
			var addr string
			select {
			case addr = <-address:
			case <-time.After(5 * time.Second):
				t.Fatal("server did not start")
			}
			requestDone := make(chan error, 1)
			go func() {
				client := &http.Client{Timeout: 5 * time.Second}
				response, err := client.Get("http://" + addr)
				if err == nil {
					defer response.Body.Close()
					var body []byte
					body, err = io.ReadAll(response.Body)
					if !timeout && string(body) != "complete" {
						err = errors.New("incomplete response")
					}
				}
				requestDone <- err
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not start")
			}
			select {
			case <-cleaned:
				t.Fatal("cleanup ran with active request")
			default:
			}
			cancel()
			select {
			case err := <-result:
				if timeout {
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("drain error = %v", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					select {
					case <-finished:
					default:
						t.Fatal("returned before request finished")
					}
					if err := <-requestDone; err != nil {
						t.Fatal(err)
					}
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown did not finish")
			}
		})
	}
}
