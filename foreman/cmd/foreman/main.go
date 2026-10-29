// Command foreman polls GitHub for issues and runs agent roles on them.
package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/foreman"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/proc"
)

// logLevelEnv names the environment variable that sets the diagnostic level. The level names are
// slog's: debug, info, warn and error. Anything else, including unset, is info.
const logLevelEnv = "FACTORY_LOG_LEVEL"

// main wires the real world into foreman.Main and exits with its code.
func main() {
	os.Exit(run())
}

// run builds the edge dependencies and returns the exit code.
func run() int {
	logger := newLogger(os.Stderr)

	home, err := os.UserHomeDir()
	if err != nil {
		logger.Error("cannot find the home directory", "error", err)
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
		Log:      logger,
		Host:     host,
	}

	return foreman.Main(ctx, os.Args[1:], d)
}

// newLogger builds the diagnostic logger: text records to out, leveled by FACTORY_LOG_LEVEL.
func newLogger(out io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: parseLevel(os.Getenv(logLevelEnv))}))
}

// parseLevel reads a level name, falling back to info when it is missing or unknown.
func parseLevel(name string) slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(name)); err != nil {
		return slog.LevelInfo
	}

	return level
}
