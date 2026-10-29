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

// Cmd is one process to run: never through a shell, always with a ceiling.
type Cmd struct {
	// Argv is the program and its arguments.
	Argv []string
	// Dir is the working directory; empty means the current one.
	Dir string
	// Env is added to the inherited environment.
	Env map[string]string
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
}

// Run starts the process and waits for it, under c.Timeout. A non-zero exit is an *ExitError.
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

	cmd.Env = os.Environ()
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

	var ee *exec.ExitError
	switch {
	case err == nil:
		return captured.String(), nil
	case ctx.Err() != nil:
		return "", fmt.Errorf("%s: %w", c.Argv[0], ctx.Err())
	case errors.As(err, &ee):
		return captured.String(), &ExitError{Program: c.Argv[0], Code: ee.ExitCode()}
	default:
		return "", fmt.Errorf("%s: %w", c.Argv[0], err)
	}
}
