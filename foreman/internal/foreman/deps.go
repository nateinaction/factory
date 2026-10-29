// Package foreman is the factory's program for agent roles: the imperative shell that reads the
// world through Deps, calls the pure packages, and writes the outcome back to GitHub.
package foreman

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/openrouter"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/sandbox"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// Ceilings on every process foreman starts. A model run's deadline is its agent role's budget
// (config.AgentConfig), not a constant here.
const (
	// GitTimeout bounds git and the small helpers.
	GitTimeout = proc.DefaultTimeout
	// ToolchainTimeout bounds cloning the default branch and resolving its dev environment.
	ToolchainTimeout = 600 * time.Second
	// MintTimeout bounds minting a token.
	MintTimeout = 30 * time.Second
)

// File modes for what foreman writes.
const (
	// privateDirMode is a scratch or state directory only its owner may enter.
	privateDirMode = 0o700
	// privateFileMode is a scratch file only its owner may read.
	privateFileMode = 0o600
	// tokenFileMode lets the factory group read the token left for the factory half.
	tokenFileMode = 0o640
)

// Runner runs processes. The real one is proc.ExecRunner; tests substitute a fake.
type Runner interface {
	// Run starts the process and waits for it. A non-zero exit is a *proc.ExitError.
	Run(ctx context.Context, c proc.Cmd) (string, error)
}

// Forwarder is the run's OpenRouter proxy, as the run lifecycle sees it.
type Forwarder interface {
	// URL is the OpenRouter base URL the model is told to call.
	URL() string
	// Usage is what the forwarded responses reported.
	Usage() openrouter.Usage
	// Close stops the proxy.
	Close() error
}

// EgressProxy is the run's egress proxy, as the run lifecycle sees it.
type EgressProxy interface {
	// URL is the model's OpenRouter base URL: the proxy's API path.
	URL() string
	// Address is the loopback host:port srt settings point at.
	Address() string
	// Domains is every host the proxy allowed.
	Domains() []string
	// Refused is every host the proxy refused, listed apart from the allowed ones.
	Refused() []string
	// Close stops the proxy.
	Close() error
}

// Deps is everything at the edge: settings, processes, HTTP and the output streams.
type Deps struct {
	// Settings are the paths and endpoints.
	Settings config.Settings
	// Runner starts processes.
	Runner Runner
	// HTTP talks to GitHub.
	HTTP *http.Client
	// Out receives normal output.
	Out io.Writer
	// Err receives the model process's standard error, redacted; nil discards it.
	Err io.Writer
	// Log receives diagnostics as leveled, structured records. The edge builds it; nil logs nowhere.
	Log *slog.Logger
	// Tracer opens spans; nil opens no-op spans, as a spec that ignores telemetry does.
	Tracer trace.Tracer
	// Meter records instruments; nil records nothing.
	Meter metric.Meter
	// Host names this worker host in the usage report; empty is reported as missing.
	Host string
	// RunID is the dagu run id recorded in transition markers; empty is recorded as unknown.
	RunID string
	// Sleep waits between model retries; nil waits on the clock.
	Sleep func(ctx context.Context, d time.Duration) error
	// Token mints the run's random proxy token; nil reads crypto/rand.
	Token func() (string, error)
	// Now reads the clock for elapsed-time reports; nil reads the wall clock.
	Now func() time.Time
	// Proxy starts the run's OpenRouter proxy on addr; nil starts the real one.
	Proxy func(key, token, addr string) (Forwarder, error)
	// Egress starts the run's egress proxy; nil starts the real one.
	Egress func(ctx context.Context, policy sandbox.Network, apiURL, token string) (EgressProxy, error)
}

// client returns a GitHub client for token.
func (d Deps) client(token string) github.Client {
	return github.NewClient(d.Settings.API, token, d.HTTP)
}

// errOut is where the model's redacted standard error goes.
func (d Deps) errOut() io.Writer {
	if d.Err == nil {
		return io.Discard
	}

	return d.Err
}

// gitProgram is the git binary foreman runs on the host, outside every sandbox.
const gitProgram = "git"

// gitClone is git's clone subcommand.
const gitClone = "clone"

// run starts a process under a ceiling.
func (d Deps) run(ctx context.Context, c proc.Cmd) (string, error) {
	out, err := d.Runner.Run(ctx, c)
	if err != nil {
		return out, fmt.Errorf("%s: %w", c.Argv[0], err)
	}

	return out, nil
}

// log returns d's logger. A Deps built without one, as a spec that ignores diagnostics does, logs
// nowhere rather than panicking.
func (d Deps) log() *slog.Logger {
	if d.Log == nil {
		return slog.New(slog.DiscardHandler)
	}

	return d.Log
}

// tracer returns d's tracer. A Deps built without one traces nowhere.
func (d Deps) tracer() trace.Tracer {
	if d.Tracer == nil {
		return tracenoop.NewTracerProvider().Tracer("")
	}

	return d.Tracer
}

// meter returns d's meter. A Deps built without one records nowhere.
func (d Deps) meter() metric.Meter {
	if d.Meter == nil {
		return metricnoop.NewMeterProvider().Meter("")
	}

	return d.Meter
}

// runID is the id transition markers record: the dagu run's id, or a placeholder outside dagu.
func (d Deps) runID() string {
	if d.RunID == "" {
		return "unknown"
	}

	return d.RunID
}

// validRunID is what a run id may contain to be part of a file name.
var validRunID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// tokenPath is the run's own token file. A run id that is not a plain name fails, so it cannot
// reach outside the token directory; the run id is the dagu run's, not input from a repository.
func (d Deps) tokenPath(role workflow.Role) (string, error) {
	id := d.runID()
	if !validRunID.MatchString(id) {
		return "", fmt.Errorf("token file: run id %q is not a plain name", id)
	}

	return d.Settings.TokenPath(role, id), nil
}

// writeLine writes text and a newline.
func writeLine(w io.Writer, text string) error {
	if _, err := fmt.Fprintln(w, text); err != nil {
		return fmt.Errorf("write output: %w", err)
	}

	return nil
}

// sleep waits for d or until ctx ends. Tests replace it through Deps.Sleep.
func (d Deps) sleep(ctx context.Context, wait time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, wait)
	}

	t := time.NewTimer(wait)
	defer t.Stop()

	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("sleep: %w", ctx.Err())
	}
}

// now reads the clock. Tests replace it through Deps.Now, so elapsed times are deterministic.
func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}

	return time.Now()
}

// GitAuthEnv gives git its credentials through the environment, so they never appear in argv.
func GitAuthEnv(token string) map[string]string {
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))

	return map[string]string{
		"GIT_CONFIG_COUNT":    "1",
		"GIT_CONFIG_KEY_0":    "http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0":  "Authorization: Basic " + basic,
		"GIT_TERMINAL_PROMPT": "0",
	}
}
