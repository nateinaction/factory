// Package foreman is the factory's orchestrator for agent roles: the imperative shell that reads the
// world through Deps, calls the pure packages, and writes the outcome back to GitHub.
package foreman

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/proc"
)

// Ceilings on every process foreman starts.
const (
	// GitTimeout bounds git and the small helpers.
	GitTimeout = proc.DefaultTimeout
	// AgentTimeout bounds the Implementer's model run.
	AgentTimeout = 3600 * time.Second
	// RetroTimeout bounds the retro step.
	RetroTimeout = 900 * time.Second
	// ReviewTimeout bounds the Reviewer's model run.
	ReviewTimeout = 1800 * time.Second
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
	// Log receives diagnostics as leveled, structured records. The edge builds it; nil logs nowhere.
	Log *slog.Logger
	// Host names this worker host in the usage report; empty is reported as missing.
	Host string
	// Sleep waits between model retries; nil waits on the clock.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now reads the clock for elapsed-time reports; nil reads the wall clock.
	Now func() time.Time
}

// client returns a GitHub client for token.
func (d Deps) client(token string) github.Client {
	return github.NewClient(d.Settings.API, token, d.HTTP)
}

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
