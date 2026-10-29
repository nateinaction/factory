//go:build unix

package proc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The specs here kill process groups and signal pids, which only unix has.
var _ = Describe("a step's deadline", func() {
	It("kills the whole process tree the step started", func() {
		pidPath := filepath.Join(GinkgoT().TempDir(), "grandchild.pid")

		const (
			deadline  = 300 * time.Millisecond
			waitDelay = 100 * time.Millisecond
		)

		started := time.Now()

		out, err := ExecRunner{WaitDelay: waitDelay}.Run(context.Background(), Cmd{
			// The child waits on its grandchild, which holds the captured output pipe, as srt does
			// while opencode writes to it. Killing only the child would leave the pipe open.
			Argv:    []string{shellPath(), "-c", `sleep 30 & echo $! > "$1"; wait`, "sh", pidPath},
			Timeout: deadline,
			Capture: true,
		})
		elapsed := time.Since(started)

		Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "the deadline is the error: %v", err)
		Expect(out).To(BeEmpty())
		Expect(elapsed).To(BeNumerically("<", deadline+waitDelay+time.Second),
			"the wait ends at the deadline, not when the grandchild exits")
		Expect(isGone(grandchildPID(pidPath))).To(BeTrue(), "the grandchild is killed with the tree")
	})

	It("stops waiting at the wait delay for a process outside the tree holding the pipe", func() {
		pidPath := filepath.Join(GinkgoT().TempDir(), "grandchild.pid")

		const waitDelay = 100 * time.Millisecond

		started := time.Now()

		_, err := ExecRunner{WaitDelay: waitDelay}.Run(context.Background(), Cmd{
			// The child exits at once and leaves its grandchild holding the captured output pipe,
			// so only the wait delay can end the wait.
			Argv:    []string{shellPath(), "-c", `sleep 5 & echo $! > "$1"; echo holding`, "sh", pidPath},
			Timeout: 30 * time.Second,
			Capture: true,
		})
		elapsed := time.Since(started)

		DeferCleanup(func() { _ = syscall.Kill(grandchildPID(pidPath), syscall.SIGKILL) })

		Expect(errors.Is(err, exec.ErrWaitDelay)).To(BeTrue(), "the wait delay is the error: %v", err)
		Expect(elapsed).To(BeNumerically("<", waitDelay+time.Second),
			"the wait ends at the wait delay, not when the grandchild exits")
	})
})

// grandchildPID reads the pid the shell wrote for its background child before the deadline.
func grandchildPID(path string) int {
	GinkgoHelper()

	text, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())

	pid, err := strconv.Atoi(strings.TrimSpace(string(text)))
	Expect(err).NotTo(HaveOccurred())

	return pid
}

// isGone reports whether the process has stopped, polling briefly for the kill to land.
func isGone(pid int) bool {
	const (
		polls = 100
		every = 10 * time.Millisecond
	)

	for range polls {
		if err := syscall.Kill(pid, syscall.Signal(0)); errors.Is(err, syscall.ESRCH) {
			return true
		}

		time.Sleep(every)
	}

	return false
}
