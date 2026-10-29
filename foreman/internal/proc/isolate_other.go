//go:build !unix

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// isolate cancels a step by killing the child. A host without process groups has no tree to reach,
// and the sandbox refuses such hosts (sandbox.Supported), so this only keeps the build working.
func isolate(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return killProcess(cmd) }
}

// killProcess kills the child, treating one that already exited as done.
func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}

	err := cmd.Process.Kill()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill %s: %w", cmd.Path, err)
	}

	return nil
}
