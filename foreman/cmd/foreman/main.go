// Command foreman polls GitHub for issues and runs agent roles on them.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/foreman"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/proc"
)

// ExitUsage is the exit code of a malformed invocation, including an unknown agent role.
const ExitUsage = 2

// logLevelEnv names the environment variable that sets the diagnostic level. The level names are
// slog's: debug, info, warn and error. Anything else, including unset, is info.
const logLevelEnv = "FACTORY_LOG_LEVEL"

// main wires the real world into the command and exits with its code.
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

	d := &foreman.Deps{
		Runner: proc.ExecRunner{Stdout: os.Stdout, Stderr: os.Stderr},
		HTTP:   &http.Client{Timeout: github.HTTPTimeout},
		Out:    os.Stdout,
		Log:    logger,
		Host:   host,
		RunID:  os.Getenv("DAG_RUN_ID"),
	}

	root, err := newRootCommand(d, config.SettingsViper(home))
	if err != nil {
		logger.Error("cannot build the command", "error", err)
		return 1
	}

	cmd, err := executeCommand(ctx, root, os.Args[1:], underSudo(os.Getenv("SUDO_UID"), os.Getuid(), os.Geteuid()))

	return exit(logger, cmd, err)
}

// underSudo reports whether sudo started this process. sudo exports SUDO_UID to the command it runs,
// which catches `sudo -u factory` as well: that sets the real and effective uid to the target user, so
// comparing them sees nothing. The comparison still catches a setuid root binary, which exports no
// SUDO_UID.
func underSudo(sudoUID string, realUID, effectiveUID int) bool {
	return sudoUID != "" || realUID != effectiveUID
}

// exit maps the command's result to a process exit code, logging the failure once.
func exit(logger *slog.Logger, cmd *cobra.Command, err error) int {
	if err == nil {
		return 0
	}

	if failure, ok := errors.AsType[*exitError](err); ok {
		if failure.err != nil {
			logger.Error("foreman failed", "error", failure.err)
		}

		return failure.code
	}

	// Anything cobra did not wrap is a malformed invocation. The usage comes from the commands.
	logger.Error("bad invocation", "reason", err)
	cmd.PrintErr(cmd.UsageString())

	return ExitUsage
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
