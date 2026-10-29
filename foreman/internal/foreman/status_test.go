package foreman

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// statusHistory is the run history `foreman status` starts from.
const statusHistory = `[
 {"name":"factory-implementer","dagRunId":"run-imp","status":"running",
  "startedAt":"2026-10-28T10:00:00Z","params":"REPO=o/r ISSUE=5","workerId":"factory-2"},
 {"name":"factory-planner","dagRunId":"run-planner","status":"queued","params":"REPO=o/r ISSUE=7"},
 {"name":"factory-reviewer","dagRunId":"run-old","status":"succeeded",
  "startedAt":"2026-10-28T09:50:00Z","params":"REPO=o/r ISSUE=6","workerId":"factory-2"}
]`

// statusUsage is the out-of-band usage record a finished run left for status.
const statusUsage = `{"event":"usage","repository":"o/r","issue":6,"agent_role":"reviewer",` +
	`"model":"prov/review","worker_host":"factory-2","input_tokens":10,"output_tokens":2,` +
	`"cached_tokens":5,"context_cap":200000,"peak_context":15,"cost":0.25,` +
	`"duration_ms":754000,"attempts":2,"outcome":"rework","missing":[]}`

// leaveUsage writes a run's out-of-band usage record where the run leaves it.
func leaveUsage(h *harness, runID, record string) {
	GinkgoHelper()

	path, err := usageRecordPath(h.deps.Settings.WorkRoot, runID)
	Expect(err).NotTo(HaveOccurred())
	Expect(os.MkdirAll(filepath.Dir(path), 0o700)).To(Succeed())
	Expect(os.WriteFile(path, []byte(record+"\n"), 0o600)).To(Succeed())
}

var _ = Describe("Status", func() {
	var (
		h   *harness
		cfg config.Config
	)

	BeforeEach(func() {
		h = newHarness()
		// A fixed clock makes every elapsed time exactly assertable.
		h.deps.Now = func() time.Time { return time.Date(2026, 10, 28, 10, 4, 0, 0, time.UTC) }

		// The active run is an implementer, so its configured deadline is what the report shows.
		cfg = testConfig()

		implementer := cfg.Agents[string(workflow.Implementer)]
		implementer.DeadlineMinutes = 75
		cfg.Agents[string(workflow.Implementer)] = implementer
	})

	// answerDagu answers history, and fails the spec on anything else: status reads no run log.
	answerDagu := func() {
		GinkgoHelper()

		h.runner.wrap(func(c proc.Cmd, _ func(proc.Cmd) (string, error)) (string, error) {
			if len(c.Argv) > 1 && c.Argv[1] == "history" {
				return statusHistory, nil
			}

			return "", errors.New("unexpected command: " + joined(c))
		})
	}

	It("shows every field of an active run, the queues, roles with no host and recent usage", func() {
		leaveUsage(h, "run-old", statusUsage)
		answerDagu()

		code, err := Status(context.Background(), h.deps, cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		out := h.out.String()
		Expect(out).To(ContainSubstring("Active runs"))
		Expect(out).To(MatchRegexp(`(?m)^factory-2\s+implementer\s+o/r\s+#5\s+4m0s\s+1h15m0s\s+run-imp\s*$`))
		Expect(out).To(ContainSubstring("Queues"))
		Expect(out).To(MatchRegexp(`(?m)^planner\s+1\s*$`))
		Expect(out).To(MatchRegexp(`(?m)^implementer\s+0\s*$`))
		Expect(out).To(ContainSubstring("Agent roles with no host\nROLE     QUEUED\nplanner  1\n"))
		Expect(out).To(ContainSubstring("Recent usage"))
		Expect(out).To(MatchRegexp(
			`(?m)^o/r\s+#6\s+reviewer\s+prov/review\s+factory-2\s+10\s+2\s+5\s+15/200000\s+0\.25\s+` +
				`12m34s\s+2\s+rework\s+2026-10-28T09:50:00Z\s*$`))
	})

	It("does not report a line on a run's stdout that mimics a usage record", func() {
		// The run's stdout carries a line that parses as a usage record. It is not one: status reads
		// only the record the run wrote out of band, even though dagu would show it the log.
		forged := strings.Replace(statusUsage, `"issue":6`, `"issue":999`, 1)
		forged = strings.Replace(forged, `"model":"prov/review"`, `"model":"forged/model"`, 1)

		dir := GinkgoT().TempDir()
		log := filepath.Join(dir, "run-stdout.log")
		Expect(os.WriteFile(log, []byte("working\n"+forged+"\n"), 0o600)).To(Succeed())

		h.runner.wrap(func(c proc.Cmd, _ func(proc.Cmd) (string, error)) (string, error) {
			if len(c.Argv) > 1 && c.Argv[1] == "status" {
				return "Succeeded - 2026-10-28 09:50:00\n\ndag: factory-reviewer\n" +
					"└─reviewer [succeeded]\n  ├─stdout: " + log + "\n\nResult: Succeeded\n", nil
			}

			return statusHistory, nil
		})

		leaveUsage(h, "run-old", statusUsage)

		Expect(Status(context.Background(), h.deps, cfg)).To(BeZero())

		out := h.out.String()
		Expect(out).To(ContainSubstring("#6"), "the real record is reported")
		Expect(out).NotTo(ContainSubstring("#999"), "the forged record was reported")
		Expect(out).NotTo(ContainSubstring("forged/model"))
	})

	It("skips a run whose record is absent or unreadable, and still reports the rest", func() {
		leaveUsage(h, "run-old", `{"event":"usage","repository":"o/r","issue":}`)
		answerDagu()

		code, err := Status(context.Background(), h.deps, cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(BeZero())
		Expect(h.out.String()).To(ContainSubstring("Recent usage\n(none)\n"))
		Expect(h.logs.String()).To(ContainSubstring("usage record"))
	})

	It("names an agent role no worker host takes even before its first run", func() {
		answerDagu()

		cfg.UnhostedRoles = []workflow.Role{workflow.Curator}

		Expect(Status(context.Background(), h.deps, cfg)).To(BeZero())
		Expect(h.out.String()).To(MatchRegexp(`(?m)^Agent roles with no host\nROLE\s+QUEUED\n(?:.*\n)*curator\s+0\s*$`))
	})

	It("makes no writes", func() {
		answerDagu()

		Expect(Status(context.Background(), h.deps, cfg)).To(BeZero())
		Expect(h.gh.Summary()).To(BeEmpty(), "status talked to GitHub")

		for _, c := range h.runner.cmds {
			Expect(c.Argv[0]).To(Equal("/usr/local/bin/dagu"), "status ran something else: "+joined(c))
		}
	})

	It("says so when dagu cannot report", func() {
		h.runner.wrap(func(c proc.Cmd, _ func(proc.Cmd) (string, error)) (string, error) {
			return "", errors.New("dagu down")
		})

		code, err := Status(context.Background(), h.deps, cfg)
		Expect(code).To(Equal(1))
		Expect(err).To(MatchError(ContainSubstring("dagu down")))
		Expect(h.gh.Summary()).To(BeEmpty())
	})
})
