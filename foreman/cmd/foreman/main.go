// Command foreman polls GitHub for issues and runs agent roles on them.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/foreman"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/proc"
)

// main wires the real world into foreman.Main and exits with its code.
func main() {
	os.Exit(run())
}

// run builds the edge dependencies and returns the exit code.
func run() int {
	home, err := os.UserHomeDir()
	if err != nil {
		// A failed write to stderr has nowhere left to be reported.
		_, _ = fmt.Fprintf(os.Stderr, "foreman: %v\n", err)
		return 1
	}
	// A host name that cannot be read is reported as missing in the usage line, not guessed.
	host, hostErr := os.Hostname()
	if hostErr != nil {
		host = ""
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d := foreman.Deps{
		Settings: config.DefaultSettings(home, os.Getenv("FACTORY_AGENT_CONFIG")),
		Runner:   proc.ExecRunner{Stdout: os.Stdout, Stderr: os.Stderr},
		HTTP:     &http.Client{Timeout: github.HTTPTimeout},
		Out:      os.Stdout,
		Err:      os.Stderr,
		Host:     host,
	}

	return foreman.Main(ctx, os.Args[1:], d)
}
