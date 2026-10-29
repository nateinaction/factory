// Package proc runs processes: never through a shell, always under a ceiling.
package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// DefaultTimeout bounds a process whose Cmd sets no timeout.
const DefaultTimeout = 300 * time.Second

// DefaultWaitDelay bounds the wait after a cancelled process: how long a process outside the killed
// group may still hold the output pipe before the pipe is closed under it.
const DefaultWaitDelay = 5 * time.Second

// Cmd is one process to run: never through a shell, always with a ceiling.
type Cmd struct {
	// Argv is the program and its arguments.
	Argv []string
	// Dir is the working directory; empty means the current one.
	Dir string
	// Env is added to the inherited environment, or is the whole environment when ReplaceEnv is set.
	Env map[string]string
	// ReplaceEnv makes Env the whole environment. A sandboxed model process gets only what foreman
	// names, so nothing from foreman's own environment leaks in.
	ReplaceEnv bool
	// Timeout bounds the run. Zero is replaced by DefaultTimeout.
	Timeout time.Duration
	// Capture returns stdout instead of passing it through.
	Capture bool
}

// ExitError is a process that ran and exited non-zero.
type ExitError struct {
	Program string
	Code    int
}

// Error names the program and its exit code.
func (e *ExitError) Error() string {
	return fmt.Sprintf("%s exited with code %d", e.Program, e.Code)
}

// ExecRunner runs real processes, passing stdout and stderr through unless captured.
type ExecRunner struct {
	Stdout io.Writer
	Stderr io.Writer
	// WaitDelay bounds the wait when a process outside the step's own tree still holds an output
	// pipe: one that escaped the killed group, or one a child left behind when it exited. Zero is
	// replaced by DefaultWaitDelay.
	WaitDelay time.Duration
}

// Run starts the process and waits for it, under c.Timeout, killing the whole process tree it
// leads when the deadline passes. A non-zero exit is an *ExitError.
func (r ExecRunner) Run(ctx context.Context, c Cmd) (string, error) {
	if len(c.Argv) == 0 {
		return "", errors.New("empty command")
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// An argument array, never a shell: no argument is ever parsed as a command line.
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...) //nolint:gosec // see above
	cmd.Dir = c.Dir
	cmd.WaitDelay = r.waitDelay()
	isolate(cmd)

	if c.ReplaceEnv {
		cmd.Env = []string{}
	} else {
		cmd.Env = os.Environ()
	}

	for k, v := range c.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	var captured bytes.Buffer

	cmd.Stdout = r.Stdout
	if c.Capture {
		cmd.Stdout = &captured
	}

	cmd.Stderr = r.Stderr
	err := cmd.Run()

	return waitResult(c.Argv[0], captured.String(), ctx.Err(), err)
}

// waitResult classifies what ended the wait: success, the deadline, the child's own non-zero exit,
// or another failure. Only success and a non-zero exit keep the captured output.
func waitResult(program, out string, ctxErr, waitErr error) (string, error) {
	var ee *exec.ExitError

	switch {
	case waitErr == nil:
		return out, nil
	case ctxErr != nil && errors.As(waitErr, &ee):
		// The deadline killed the process: report the deadline, not the signal that delivered it.
		return "", fmt.Errorf("%s: %w", program, ctxErr)
	case errors.As(waitErr, &ee):
		return out, &ExitError{Program: program, Code: ee.ExitCode()}
	case ctxErr != nil:
		// The deadline passed and the wait failed too, as when the cancel could not kill the
		// process group or the wait delay ran out. Keep both: a leaked process tree is not a
		// plain deadline.
		return "", fmt.Errorf("%s: %w", program, errors.Join(ctxErr, waitErr))
	default:
		return "", fmt.Errorf("%s: %w", program, waitErr)
	}
}

// waitDelay is WaitDelay, or the production default when it is unset.
func (r ExecRunner) waitDelay() time.Duration {
	if r.WaitDelay > 0 {
		return r.WaitDelay
	}

	return DefaultWaitDelay
}
