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
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/foreman"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/telemetry"
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

	tel := telemetry.Start(ctx, logger)

	d := &foreman.Deps{
		Tracer: tel.Tracer,
		Meter:  tel.Meter,
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

	sudo := underSudo(os.Getenv("SUDO_UID"), os.Getuid(), os.Geteuid())
	code := executeTraced(ctx, d, root, os.Args[1:], sudo)

	// Flush after the exit code is settled; the flush is bounded and never changes the code.
	tel.Shutdown(ctx)

	return code
}

// executeTraced runs one command line under the process's one root span, named for its verb, and
// returns the exit code. The span ends only after the failure is logged, so that record carries its ids.
func executeTraced(ctx context.Context, d *foreman.Deps, root *cobra.Command, args []string, sudo bool) int {
	verb := verbOf(root, args)

	logger := d.Log
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	// A run continues the trace its DAG parameter or environment carries. A missing context starts a
	// new trace; a malformed one is ignored with a diagnostic, never an error.
	ctx, ok := telemetry.ContinueTrace(ctx, traceParentOf(verb, args))
	if !ok {
		logger.WarnContext(ctx, "ignoring malformed trace context", "reason", "malformed traceparent")
	}

	attrs := []attribute.KeyValue{attribute.String(telemetry.AttrVerb, verb)}
	// The run id ties every process of one dagu run together in a trace; `status` finds the run's
	// trace from its TRACEPARENT parameter instead.
	if d.RunID != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrRunID, d.RunID))
	}

	ctx, span := telemetry.TracerOr(d.Tracer).Start(ctx, telemetry.RootSpanName(verb), trace.WithAttributes(attrs...))
	defer span.End()

	// The run's ids go on every diagnostic a command writes, not only the ones logged with a context:
	// the plain calls are most of foreman's, and security.Emit is one of them.
	d.Log = telemetry.SpanLogger(logger, ctx)

	cmd, err := executeCommand(ctx, root, args, sudo)
	code := exit(ctx, logger, cmd, err)

	span.SetAttributes(attribute.Int(telemetry.AttrExitCode, code))

	if code != 0 {
		// No error text: the span leaves the host, and the diagnostic record already has it.
		span.SetStatus(codes.Error, "non-zero exit")
	}

	return code
}

// traceParentOf is the W3C trace context an invocation carries. `start` receives it from dagu as the
// argument after the issue, the DAG's TRACEPARENT parameter. Every other verb reads the environment,
// which is how `start` hands it to the factory half across sudo.
func traceParentOf(verb string, args []string) string {
	if verb == startVerb {
		positional := positionalArgs(args)
		if len(positional) == roleArgCount+2 && positional[roleArgCount+1] != "" {
			return positional[roleArgCount+1]
		}
	}

	return os.Getenv(telemetry.TraceParentVar)
}

// positionalArgs drops flags and their values, keeping the arguments cobra would treat as positional.
// Every foreman flag takes a value, so a lone flag owns the next argument.
func positionalArgs(args []string) []string {
	out := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case arg == "--":
			return append(out, args[i+1:]...)
		case strings.HasPrefix(arg, "-"):
			if !strings.Contains(arg, "=") {
				i++
			}
		default:
			out = append(out, arg)
		}
	}

	return out
}

// unknownVerb names the span of an invocation that names no verb foreman has.
const unknownVerb = "unknown"

// verbOf is the verb args select, or unknownVerb. It reads the raw arguments only to name the span.
func verbOf(root *cobra.Command, args []string) string {
	cmd, _, err := root.Find(args)
	if err != nil || cmd == root {
		return unknownVerb
	}

	return cmd.Name()
}

// underSudo reports whether sudo started this process. sudo exports SUDO_UID to the command it runs,
// which catches `sudo -u factory` as well: that sets the real and effective uid to the target user, so
// comparing them sees nothing. The comparison still catches a setuid root binary, which exports no
// SUDO_UID.
func underSudo(sudoUID string, realUID, effectiveUID int) bool {
	return sudoUID != "" || realUID != effectiveUID
}

// exit maps the command's result to a process exit code, logging the failure once.
func exit(ctx context.Context, logger *slog.Logger, cmd *cobra.Command, err error) int {
	if err == nil {
		return 0
	}

	if failure, ok := errors.AsType[*exitError](err); ok {
		if failure.err != nil {
			logger.ErrorContext(ctx, "foreman failed", "error", failure.err)
		}

		return failure.code
	}

	// Anything cobra did not wrap is a malformed invocation. The usage comes from the commands.
	logger.ErrorContext(ctx, "bad invocation", "reason", err)
	cmd.PrintErr(cmd.UsageString())

	return ExitUsage
}

// newLogger builds the diagnostic logger: JSON records, one per line, to out, leveled by FACTORY_LOG_LEVEL.
func newLogger(out io.Writer) *slog.Logger {
	handler := slog.NewJSONHandler(out, &slog.HandlerOptions{Level: parseLevel(os.Getenv(logLevelEnv))})

	return slog.New(telemetry.LogHandler(handler))
}

// parseLevel reads a level name, falling back to info when it is missing or unknown.
func parseLevel(name string) slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(name)); err != nil {
		return slog.LevelInfo
	}

	return level
}
