package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/NotRllyRn/codex-broker/internal/cli"
	"github.com/NotRllyRn/codex-broker/internal/platform"
)

func main() {
	if os.Getenv("WINDOWKEEPER_DATA_DIR") == "/data" {
		if err := platform.PrepareVolumes("/data", "/run/windowkeeper"); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := cli.Run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
