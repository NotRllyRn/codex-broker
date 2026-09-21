package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/loopback"
)

func main() {
	listen := flag.String("listen", value("CODEX_BROKER_ADAPTER_LISTEN", loopback.DefaultListen), "loopback listen address")
	brokerURL := flag.String("broker-url", os.Getenv("CODEX_BROKER_URL"), "Codex Broker HTTPS origin")
	brokerCA := flag.String("broker-ca", os.Getenv("CODEX_BROKER_CA_CERT"), "optional Codex Broker CA certificate")
	flag.Parse()
	adapter, err := loopback.New(loopback.Config{Listen: *listen, BrokerURL: *brokerURL, BrokerCA: *brokerCA})
	if err != nil {
		fatal(err)
	}
	server := &http.Server{
		Addr:              *listen,
		Handler:           adapter.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("Codex Broker adapter listening on %s", *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(err)
	}
}

func value(name, fallback string) string {
	if current := os.Getenv(name); current != "" {
		return current
	}
	return fallback
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(1)
}
