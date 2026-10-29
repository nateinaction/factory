package github_test

import (
	"context"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/ghtest"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("reading CI", func() {
	var (
		p   *ghtest.Proxy
		c   github.Client
		ctx context.Context
		sha = "abc123"
	)

	BeforeEach(func() {
		ctx = context.Background()
		p = ghtest.NewProxy("")

		DeferCleanup(func() {
			Expect(p.Unexpected()).To(BeEmpty())
			p.Close()
		})

		c = github.NewClient(p.URL(), "secret-token", p.Client())
	})

	It("reads a commit's check runs from the envelope GitHub wraps them in", func() {
		p.JSON("GET /repos/o/r/commits/abc123/check-runs", map[string]any{
			"total_count": 2,
			"check_runs": []map[string]any{
				{
					"id": 9, "name": "build", "status": "completed", "conclusion": "failure",
					"output": map[string]string{"title": "Build failed", "summary": "the compiler said no"},
					"app":    map[string]string{"slug": "github-actions"},
				},
				{"id": 10, "name": "lint", "status": "completed", "conclusion": "success"},
			},
		})

		checks, err := c.CheckRuns(ctx, "o/r", sha)
		Expect(err).NotTo(HaveOccurred())
		Expect(checks).To(HaveLen(2))
		Expect(checks[0]).To(And(
			HaveField("ID", int64(9)), HaveField("Name", "build"),
			HaveField("Conclusion", "failure"),
			HaveField("Output", HaveField("Summary", "the compiler said no")),
			HaveField("App.Slug", "github-actions"),
		))
	})

	It("reads a commit's statuses", func() {
		p.JSON("GET /repos/o/r/commits/abc123/statuses", []map[string]string{
			{"context": "ci/lint", "state": "failure", "description": "lint failed"},
			{"context": "ci/test", "state": "success"},
		})

		statuses, err := c.CommitStatuses(ctx, "o/r", sha)
		Expect(err).NotTo(HaveOccurred())
		Expect(statuses).To(HaveLen(2))
		Expect(statuses[0]).To(Equal(workflow.CommitStatus{
			Context: "ci/lint", State: "failure", Description: "lint failed",
		}))
	})

	It("reads an Actions job's log as plain text", func() {
		p.Handle("GET /repos/o/r/actions/jobs/9/logs", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")

			_, err := w.Write([]byte("line one\nline two\n"))
			Expect(err).NotTo(HaveOccurred())
		})

		log, err := c.JobLog(ctx, "o/r", 9)
		Expect(err).NotTo(HaveOccurred())
		Expect(log).To(Equal("line one\nline two\n"))
	})

	It("fails closed when a job log cannot be read", func() {
		p.Status("GET /repos/o/r/actions/jobs/9/logs", http.StatusNotFound)

		_, err := c.JobLog(ctx, "o/r", 9)
		Expect(err).To(MatchError(ContainSubstring("404")))
		Expect(err.Error()).NotTo(ContainSubstring("secret-token"))
	})
})
