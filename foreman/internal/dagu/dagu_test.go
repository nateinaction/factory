package dagu

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("the dagu CLI adapter", func() {
	Describe("commands", func() {
		It("asks for recent runs in JSON", func() {
			Expect(HistoryArgv("/usr/local/bin/dagu", 50)).To(Equal([]string{
				"/usr/local/bin/dagu", "history", "--format", "json", "--limit", "50",
			}))
		})

		It("asks only for the runs that keep an agent role busy", func() {
			Expect(ActiveHistoryArgv("/usr/local/bin/dagu", 1000)).To(Equal([]string{
				"/usr/local/bin/dagu", "history",
				"--status", "queued,running,waiting,not_started",
				"--format", "json", "--limit", "1000",
			}))
		})

		It("lists exactly the statuses an active run carries", func() {
			for _, status := range ActiveStatuses() {
				Expect(Run{Status: status}.Active()).To(BeTrue(), status)
			}
		})

		It("asks for one run's tree by its id", func() {
			Expect(StatusArgv("/usr/local/bin/dagu", "factory-implementer", "run-1")).To(Equal([]string{
				"/usr/local/bin/dagu", "status", "--run-id", "run-1", "factory-implementer",
			}))
		})
	})

	Describe("ParseHistory", func() {
		It("reads the fields foreman reports", func() {
			runs, err := ParseHistory([]byte(`[
				{"name":"factory-implementer","dagRunId":"run-1","status":"running",
				 "startedAt":"2026-10-28T10:00:00Z","params":"REPO=o/r ISSUE=5",
				 "workerId":"factory-2","duration":"4m0s","labels":["prod"],"unknown":true},
				{"name":"factory-reviewer","dagRunId":"run-2","status":"queued","params":"REPO=o/r ISSUE=6"}
			]`))
			Expect(err).NotTo(HaveOccurred())
			Expect(runs).To(HaveLen(2))
			Expect(runs[0]).To(Equal(Run{
				Name: "factory-implementer", RunID: "run-1", Status: StatusRunning,
				StartedAt: "2026-10-28T10:00:00Z", Params: "REPO=o/r ISSUE=5", WorkerID: "factory-2",
			}))
			Expect(runs[1].StartedAt).To(BeEmpty())
		})

		It("reads no runs as no runs", func() {
			runs, err := ParseHistory([]byte("[]"))
			Expect(err).NotTo(HaveOccurred())
			Expect(runs).To(BeEmpty())
		})

		It("fails on anything but a JSON list of runs", func() {
			_, err := ParseHistory([]byte("No DAG runs found matching the specified filters.\n"))
			Expect(err).To(MatchError(ContainSubstring("parse dagu history")))
		})
	})

	DescribeTable("a run is waiting for a worker while no worker has started it",
		func(status string, waiting bool) {
			Expect(Run{Status: status}.Waiting()).To(Equal(waiting))
		},
		Entry("queued", StatusQueued, true),
		Entry("not started", StatusNotStarted, true),
		Entry("running", StatusRunning, false),
		Entry("waiting on a human", StatusWaiting, false),
		Entry("succeeded", "succeeded", false),
		Entry("failed", "failed", false),
	)

	DescribeTable("a run is active while dagu can still move it",
		func(status string, active bool) {
			Expect(Run{Status: status}.Active()).To(Equal(active))
		},
		Entry("queued", StatusQueued, true),
		Entry("running", StatusRunning, true),
		Entry("waiting", StatusWaiting, true),
		Entry("not started", StatusNotStarted, true),
		Entry("succeeded", "succeeded", false),
		Entry("failed", "failed", false),
		Entry("aborted", "aborted", false),
		Entry("rejected", "rejected", false),
		Entry("partially succeeded", "partially_succeeded", false),
	)

	Describe("Param", func() {
		It("reads a key from the run's params", func() {
			r := Run{Params: "REPO=o/r ISSUE=5"}

			repo, ok := r.Param("REPO")
			Expect(ok).To(BeTrue())
			Expect(repo).To(Equal("o/r"))

			issue, ok := r.Param("ISSUE")
			Expect(ok).To(BeTrue())
			Expect(issue).To(Equal("5"))
		})

		It("reports a missing key", func() {
			_, ok := Run{Params: "REPO=o/r"}.Param("ISSUE")
			Expect(ok).To(BeFalse())
		})
	})

	Describe("ParseLogPaths", func() {
		const tree = `Succeeded - 2026-10-28 10:00:00

dag: factory-implementer (2m30s)
├─log: /var/lib/dagu/logs/dags/factory-implementer/run-scheduler.log
│
└─implementer (2m30s) [succeeded]
  ├─stdout: /var/lib/dagu/logs/dags/factory-implementer/run-stdout.log
  ├─stderr: /var/lib/dagu/logs/dags/factory-implementer/run-stderr.log
  └─stdout: /var/lib/dagu/logs/dags/factory-implementer/second-stdout.log

Result: Succeeded
`

		It("reads every log file the run wrote, in tree order", func() {
			Expect(ParseLogPaths(tree)).To(Equal([]LogRef{
				{Stream: StreamScheduler, Path: "/var/lib/dagu/logs/dags/factory-implementer/run-scheduler.log"},
				{Stream: StreamStdout, Path: "/var/lib/dagu/logs/dags/factory-implementer/run-stdout.log"},
				{Stream: StreamStderr, Path: "/var/lib/dagu/logs/dags/factory-implementer/run-stderr.log"},
				{Stream: StreamStdout, Path: "/var/lib/dagu/logs/dags/factory-implementer/second-stdout.log"},
			}))
		})

		It("ignores content lines that mention a stream", func() {
			Expect(ParseLogPaths("dag: x\n  the stdout: path is not a log line\n")).To(BeEmpty())
		})

		It("reads no paths from an empty status", func() {
			Expect(ParseLogPaths("")).To(BeEmpty())
		})
	})

	Describe("ParseStatus", func() {
		It("reads the result of a finished run", func() {
			status, ok := ParseStatus("Succeeded - 2026-10-28 10:00:00\n\ndag: x\n\nResult: Succeeded\n")
			Expect(ok).To(BeTrue())
			Expect(status).To(Equal("succeeded"))
		})

		It("reads the status of a running run from the header", func() {
			status, ok := ParseStatus("Running - 2026-10-28 10:00:00\n\ndag: x\n\nStatus: Running\n")
			Expect(ok).To(BeTrue())
			Expect(status).To(Equal(StatusRunning))
		})

		It("reports an unrecognisable status", func() {
			_, ok := ParseStatus("dagu: no such run\n")
			Expect(ok).To(BeFalse())
		})
	})
})
