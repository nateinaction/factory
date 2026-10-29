package foreman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/sandbox"
)

// agent runs the model, retrying transient failures up to twice within r.Timeout.
func (s session) agent(ctx context.Context, r opencode.Run) (opencode.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	// A budget with nothing left never starts the model: the deadline has already passed.
	if err := ctx.Err(); err != nil {
		return opencode.Result{}, fmt.Errorf("%s: %w", r.Role, err)
	}

	deadline, _ := ctx.Deadline() // WithTimeout always sets one

	var res opencode.Result

	for attempt := 1; attempt <= opencode.MaxModelAttempts; attempt++ {
		res.Attempts = attempt

		stream, err := s.attempt(ctx, r, time.Until(deadline))

		res.Usage = res.Usage.Add(stream.Usage)

		res.Steps += stream.Steps
		if stream.Session != "" {
			res.Session = stream.Session
		}

		done, err := s.afterAttempt(ctx, r, stream, err, res.Steps, attempt)
		if err != nil {
			return res, err
		}

		if done {
			return res, nil
		}
	}
	// unreachable: the loop returns
	return res, &opencode.ModelError{Kind: opencode.FailOther, Attempts: opencode.MaxModelAttempts}
}

// afterAttempt decides what to do with one attempt: done reports that the run finished, and a
// non-nil error is the failure to return. A step cap fails the run and is never retried.
func (s session) afterAttempt(
	ctx context.Context, r opencode.Run, stream opencode.Stream, runErr error, steps, attempt int,
) (done bool, err error) {
	kind, failed := opencode.Failure(stream, runErr)
	capped := opencode.StepCapReached(steps, r.Steps)

	if !failed {
		if capped {
			return false, &opencode.ModelError{Kind: opencode.FailStepCap, Attempts: attempt}
		}

		return true, nil
	}

	var cause error
	if stream.Failure == nil {
		cause = runErr
	}

	if ctx.Err() != nil {
		return false, fmt.Errorf("%s: %w", r.Role, ctx.Err())
	}

	if !kind.Retryable() || attempt == opencode.MaxModelAttempts || capped {
		return false, &opencode.ModelError{Kind: kind, Attempts: attempt, Cause: cause}
	}

	if err := s.d.sleep(ctx, opencode.Backoff(attempt)); err != nil {
		return false, fmt.Errorf("%s: %w", r.Role, err)
	}

	return false, nil
}

// attempt runs opencode once under the run's sandbox and parses what it printed. The stream goes to
// the log at debug level, not info, with every secret the run holds removed: the transcript carries
// private repository code and issue text, so it stays out of the default diagnostics. A session
// without a backend or settings refuses to start the model: there is no unsandboxed fallback.
func (s session) attempt(ctx context.Context, r opencode.Run, timeout time.Duration) (opencode.Stream, error) {
	argv := sandbox.Argv(s.srt, s.settings, opencode.Argv(r))
	if argv == nil {
		return opencode.Stream{}, errors.New("the sandbox is not configured")
	}

	// Every byte the model process writes reaches a log through here: stdout through the
	// "agent output" record, stderr through this writer. There is no other path out.
	stderr := newRedactWriter(s.d.errOut(), s.secrets)

	out, err := s.d.Runner.Run(ctx, proc.Cmd{
		Argv: argv, Dir: s.workdir, Env: s.agentEnv, ReplaceEnv: true, Timeout: timeout, Capture: true,
		Stderr: stderr,
	})
	err = errors.Join(err, stderr.flush())

	if out != "" {
		s.d.log().Debug("agent output", "text", redact(out, s.secrets))
	}

	stream, parseErr := opencode.ParseStream(out)
	// A stream foreman could not read to its end is a failure of the attempt in its own right: the
	// steps, the usage and any failure event after the unreadable line are missing, so the run is
	// never read as a success.
	return stream, errors.Join(err, parseErr)
}

// logUsage writes the run's single usage line to stdout, which dagu captures in the run log.
func (d Deps) logUsage(r opencode.UsageReport) {
	line, err := json.Marshal(r)
	if err != nil {
		d.log().Error("usage report failed", "error", err)
		return
	}

	if err := writeLine(d.Out, string(line)); err != nil {
		d.log().Error("usage report failed", "error", err)
	}
}
