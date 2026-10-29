//go:build unix

package proc

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// isolate puts the child at the head of its own process group and cancels the step by killing that
// group. A sandboxed agent run is srt, whose grandchild is opencode: killing srt alone leaves
// opencode running, holding the captured output pipe, so the wait would outlive the deadline.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
}

// killGroup kills the process group the child leads. A group that is already gone is not an error:
// the deadline can arrive just as the child finishes on its own.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}

	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return fmt.Errorf("kill process group %d: %w", cmd.Process.Pid, err)
}
