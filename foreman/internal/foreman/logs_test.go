package foreman

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/proc"
)

// logsRun is one run's status tree that points at path as its stdout.
func logsRun(state, path string) string {
	return state + " - 2026-10-28 10:00:00\n\ndag: factory-implementer\n" +
		"└─implementer [running]\n  └─stdout: " + path + "\n\nResult: " + state + "\n"
}

var _ = Describe("Logs", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
	})

	It("prints the latest run log for an issue, and nothing from GitHub", func() {
		dir := GinkgoT().TempDir()

		latest := filepath.Join(dir, "latest.log")
		older := filepath.Join(dir, "older.log")

		Expect(os.WriteFile(latest, []byte("latest run\n"), 0o600)).To(Succeed())
		Expect(os.WriteFile(older, []byte("older run\n"), 0o600)).To(Succeed())

		h.runner.wrap(func(c proc.Cmd, _ func(proc.Cmd) (string, error)) (string, error) {
			switch {
			case len(c.Argv) > 1 && c.Argv[1] == "history":
				return `[
				 {"name":"factory-implementer","dagRunId":"run-new","status":"succeeded",
				  "startedAt":"2026-10-28T10:00:00Z","params":"REPO=o/r ISSUE=5"},
				 {"name":"factory-implementer","dagRunId":"run-old","status":"succeeded",
				  "startedAt":"2026-10-28T09:00:00Z","params":"REPO=o/r ISSUE=5"}
				]`, nil
			case len(c.Argv) > 3 && c.Argv[3] == "run-new":
				return logsRun("Succeeded", latest), nil
			case len(c.Argv) > 3 && c.Argv[3] == "run-old":
				return logsRun("Succeeded", older), nil
			}

			return "", errors.New("unexpected command: " + joined(c))
		})

		code, err := Logs(context.Background(), h.deps, testRef(5), false)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(BeZero())
		Expect(h.out.String()).To(Equal("latest run\n"))
		Expect(h.gh.Summary()).To(BeEmpty(), "logs talked to GitHub")
	})

	It("fails clearly when nothing ran for the issue", func() {
		h.runner.wrap(func(c proc.Cmd, _ func(proc.Cmd) (string, error)) (string, error) {
			if len(c.Argv) > 1 && c.Argv[1] == "history" {
				return `[{"name":"factory-implementer","dagRunId":"run-1","status":"succeeded",
					"params":"REPO=o/r ISSUE=5"}]`, nil
			}

			return "", errors.New("unexpected command: " + joined(c))
		})

		code, err := Logs(context.Background(), h.deps, testRef(99), false)
		Expect(code).To(Equal(1))
		Expect(err).To(MatchError(ContainSubstring("no dagu run for o/r issue 99")))
		Expect(h.out.String()).To(BeEmpty())
	})

	It("fails clearly when the run has no log yet", func() {
		h.runner.wrap(func(c proc.Cmd, _ func(proc.Cmd) (string, error)) (string, error) {
			switch {
			case len(c.Argv) > 1 && c.Argv[1] == "history":
				return `[{"name":"factory-implementer","dagRunId":"run-1","status":"queued",
					"params":"REPO=o/r ISSUE=5"}]`, nil
			case len(c.Argv) > 1 && c.Argv[1] == "status":
				return "Queued - -\n\ndag: factory-implementer\n\nStatus: Queued\n", nil
			}

			return "", errors.New("unexpected command: " + joined(c))
		})

		code, err := Logs(context.Background(), h.deps, testRef(5), false)
		Expect(code).To(Equal(1))
		Expect(err).To(MatchError(ContainSubstring("run-1")))
		Expect(err).To(MatchError(ContainSubstring("no log yet")))
	})

	It("streams new output and stops when the run ends", func() {
		dir := GinkgoT().TempDir()
		log := filepath.Join(dir, "run.log")
		Expect(os.WriteFile(log, []byte("one\n"), 0o600)).To(Succeed())

		// The first check sees a live run; after the one wait the run has finished with more output.
		checks := 0

		h.runner.wrap(func(c proc.Cmd, _ func(proc.Cmd) (string, error)) (string, error) {
			if len(c.Argv) > 1 && c.Argv[1] == "history" {
				return `[{"name":"factory-implementer","dagRunId":"run-1","status":"running",
					"startedAt":"2026-10-28T10:00:00Z","params":"REPO=o/r ISSUE=5"}]`, nil
			}

			checks++
			if checks == 1 {
				return logsRun("Running", log), nil
			}

			return logsRun("Succeeded", log), nil
		})
		h.deps.Sleep = func(context.Context, time.Duration) error {
			Expect(os.WriteFile(log, []byte("one\ntwo\n"), 0o600)).To(Succeed())

			return nil
		}

		code, err := Logs(context.Background(), h.deps, testRef(5), true)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(BeZero())
		Expect(h.out.String()).To(Equal("one\ntwo\n"))
		Expect(checks).To(Equal(2), "follow did not stop at the result")
	})
})
