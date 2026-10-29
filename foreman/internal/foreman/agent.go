package foreman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// agent runs the model, retrying transient failures up to twice within r.Timeout.
func (s session) agent(ctx context.Context, r opencode.Run) (opencode.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	deadline, _ := ctx.Deadline() // WithTimeout always sets one

	var res opencode.Result

	for attempt := 1; attempt <= opencode.MaxModelAttempts; attempt++ {
		stream, err := s.attempt(ctx, r, time.Until(deadline))

		res.Usage = res.Usage.Add(stream.Usage)
		if stream.Session != "" {
			res.Session = stream.Session
		}

		kind, failed := opencode.Failure(stream, err)
		if !failed {
			return res, nil
		}

		var cause error
		if stream.Failure == nil {
			cause = err
		}

		if ctx.Err() != nil {
			return res, fmt.Errorf("%s: %w", r.Role, ctx.Err())
		}

		if !kind.Retryable() || attempt == opencode.MaxModelAttempts {
			return res, &opencode.ModelError{Kind: kind, Attempts: attempt, Cause: cause}
		}

		if err := s.d.sleep(ctx, opencode.Backoff(attempt)); err != nil {
			return res, fmt.Errorf("%s: %w", r.Role, err)
		}
	}
	// unreachable: the loop returns
	return res, &opencode.ModelError{Kind: opencode.FailOther, Attempts: opencode.MaxModelAttempts}
}

// attempt runs opencode once and parses what it printed. The stream goes to the dagu log with the
// OpenRouter key removed.
func (s session) attempt(ctx context.Context, r opencode.Run, timeout time.Duration) (opencode.Stream, error) {
	out, err := s.d.Runner.Run(ctx, proc.Cmd{
		Argv: opencode.Argv(r), Dir: s.workdir, Env: s.agentEnv, Timeout: timeout, Capture: true,
	})
	if out != "" {
		s.d.logf("%s", s.redact(out))
	}

	return opencode.ParseStream(out), err
}

// redact removes the OpenRouter key from text bound for a log.
func (s session) redact(text string) string {
	key := s.agentEnv["OPENROUTER_API_KEY"]
	if key == "" {
		return text
	}

	return strings.ReplaceAll(text, key, "[redacted]")
}

// withState gives fn a fresh state database for the role's run and deletes it afterwards. The
// Reviewer's state is in memory only. A leftover from an earlier run is removed first.
func withState(root string, role workflow.Role, number int, fn func(db string) error) (err error) {
	if role == workflow.Reviewer {
		return fn(opencode.InMemoryDB)
	}

	dir, err := freshPath(root, fmt.Sprintf("state-%s-%d", role, number))
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, privateDirMode); err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()

	return fn(dir + "/opencode.db")
}

// logUsage writes the run's single usage line to stdout, which dagu captures in the run log.
func (d Deps) logUsage(r opencode.UsageReport) {
	line, err := json.Marshal(r)
	if err != nil {
		d.logf("usage report: %v", err)
		return
	}

	if err := writeLine(d.Out, string(line)); err != nil {
		d.logf("usage report: %v", err)
	}
}
