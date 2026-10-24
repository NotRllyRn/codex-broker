package main

import (
	"context"
	"fmt"
	"github.com/NotRllyRn/codex-broker/internal/usagesync"
	"os"
)

func main() {
	if err := usagesync.Run(context.Background(), os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Usage sync:", err)
		os.Exit(1)
	}
}
