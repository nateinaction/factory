package foreman

import (
	"context"
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/ghtest"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("the CI gate before review", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
	})

	// reviewIssue queues issue 5 for review with a pull request open for it.
	reviewIssue := func() {
		GinkgoHelper()
		h.queue(map[string][]map[string]any{workflow.LabelReview: {{"number": 5}}})
		h.implementerGH(map[string]any{}, nil)
	}

	// ci makes the commits endpoint answer with the given check runs and statuses.
	ci := func(checks, statuses []map[string]any) {
		h.ciChecks, h.ciStatuses = checks, statuses
	}

	// ciFails makes every CI read fail.
	ciFails := func() { h.ciError = true }

	// poll polls the review queue.
	poll := func() (int, error) {
		return Poll(context.Background(), h.deps, pollConfig(workflow.Implementer, workflow.Reviewer))
	}

	It("claims the issue when every check passed", func() {
		reviewIssue()
		ci([]map[string]any{{"name": "build", "status": "completed", "conclusion": "success"}}, nil)

		Expect(poll()).Error().NotTo(HaveOccurred())

		Expect(h.labels(5)).To(ContainElement(workflow.LabelReviewing))
		Expect(h.runner.find("enqueue")).To(HaveLen(1))
	})

	It("claims the issue when there are no checks", func() {
		reviewIssue()
		ci(nil, nil)

		Expect(poll()).Error().NotTo(HaveOccurred())
		Expect(h.runner.find("enqueue")).To(HaveLen(1))
	})

	It("keeps the issue queued while CI is pending", func() {
		reviewIssue()
		ci([]map[string]any{{"name": "build", "status": "in_progress", "conclusion": nil}}, nil)

		Expect(poll()).Error().NotTo(HaveOccurred())

		Expect(h.factoryLabels(5)).To(ConsistOf(workflow.LabelReview))
		Expect(h.markerComments(5)).To(BeEmpty())
		Expect(h.runner.find("enqueue")).To(BeEmpty())
	})

	It("fails the issue when CI stayed pending past the deadline", func() {
		reviewIssue()
		ci([]map[string]any{{"name": "build", "status": "queued", "conclusion": nil}}, nil)

		old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		h.gh.JSON("GET /repos/o/r/issues/5/timeline", []map[string]any{{
			"event": "labeled", "label": map[string]string{"name": workflow.LabelReview},
			"created_at": old,
		}})

		Expect(poll()).Error().NotTo(HaveOccurred())

		Expect(h.factoryLabels(5)).To(ConsistOf(workflow.LabelFailed))
		Expect(h.runner.find("enqueue")).To(BeEmpty(), "a pending run must not start")
		Expect(h.markerComments(5)).To(ConsistOf(ContainSubstring("to=factory:failed")))
		Expect(h.logs.String()).To(ContainSubstring("pending"))
	})

	It("sends the issue back to the Implementer on failed CI, with no model run", func() {
		reviewIssue()
		ci([]map[string]any{{
			"id": 9, "name": "build", "status": "completed", "conclusion": "failure",
			"output": map[string]string{"title": "Build failed", "summary": "the compiler said no"},
			"app":    map[string]string{"slug": "github-actions"},
		}}, []map[string]any{{"context": "ci/lint", "state": "failure", "description": "lint failed"}})

		h.gh.Handle("GET /repos/o/r/actions/jobs/9/logs", func(w http.ResponseWriter, _ *http.Request) {
			_, err := w.Write([]byte("line one\nline two\n"))
			Expect(err).NotTo(HaveOccurred())
		})

		Expect(poll()).Error().NotTo(HaveOccurred())

		Expect(h.factoryLabels(5)).To(ConsistOf(workflow.LabelImplement))
		Expect(h.runner.find("enqueue")).To(BeEmpty(), "failed CI must not start a run")

		comments := h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")
		Expect(comments).To(HaveLen(2))

		report, transition := comments[0], comments[1]
		Expect(report).To(ContainSubstring("<!-- factory:ci-failure sha="))
		Expect(report).To(And(
			ContainSubstring("**build** (failure): the compiler said no"),
			ContainSubstring("**ci/lint** (failure): lint failed"),
			ContainSubstring("line one"),
			ContainSubstring("line two"),
		))
		Expect(transition).To(ContainSubstring("to=factory:implement"))

		h.expectAuthorizedAs(workflow.Reviewer)
	})

	It("fails the issue instead of sending it back at the round limit", func() {
		reviewIssue()
		ci([]map[string]any{{
			"name": "build", "status": "completed", "conclusion": "failure", "id": 9,
			"app": map[string]string{"slug": "github-actions"},
		}}, nil)
		h.gh.Status("GET /repos/o/r/actions/jobs/9/logs", http.StatusNotFound)

		// Two earlier CI failures plus this commit reach the limit of three.
		for _, sha := range []string{"old1", "old2"} {
			h.comment(5, "factory", workflow.CIFailureMarker(sha))
		}

		Expect(poll()).Error().NotTo(HaveOccurred())

		Expect(h.factoryLabels(5)).To(ConsistOf(workflow.LabelFailed))
		Expect(h.runner.find("enqueue")).To(BeEmpty())
		Expect(h.markerComments(5)).To(ContainElement(ContainSubstring("to=factory:failed")))
	})

	It("leaves the issue queued when CI cannot be read", func() {
		reviewIssue()
		ciFails()

		Expect(poll()).Error().NotTo(HaveOccurred())

		Expect(h.factoryLabels(5)).To(ConsistOf(workflow.LabelReview))
		Expect(h.runner.find("enqueue")).To(BeEmpty())
		Expect(h.logs.String()).To(ContainSubstring("CI"))
	})

	It("lets a later review issue claim while an earlier one's CI is pending", func() {
		h.queue(map[string][]map[string]any{workflow.LabelReview: {{"number": 4}, {"number": 5}}})
		h.implementerGH(map[string]any{}, nil)

		_, err := emulator.CreatePR(context.Background(), "o/r", workflow.BranchName(4), "Add x", "Do x")
		Expect(err).NotTo(HaveOccurred())

		calls := 0
		h.ciHandler = func(w http.ResponseWriter, r *http.Request) bool {
			calls++

			if calls == 1 && strings.HasSuffix(r.URL.Path, "/check-runs") {
				ghtest.WriteJSON(w, map[string]any{"check_runs": []map[string]any{
					{"name": "build", "status": "in_progress"},
				}})

				return true
			}

			return false
		}

		Expect(poll()).Error().NotTo(HaveOccurred())

		Expect(h.runner.find("enqueue")).To(HaveLen(1))
		Expect(joined(h.runner.find("enqueue")[0])).To(ContainSubstring("ISSUE=5"))
	})

	It("uses the configured round limit from the config", func() {
		reviewIssue()
		ci([]map[string]any{{"name": "build", "status": "completed", "conclusion": "failure"}}, nil)

		cfg := pollConfig(workflow.Implementer, workflow.Reviewer)
		cfg.ReviewRoundLimit = 1

		Expect(Poll(context.Background(), h.deps, cfg)).Error().NotTo(HaveOccurred())
		Expect(h.factoryLabels(5)).To(ConsistOf(workflow.LabelFailed))
	})

	It("reads pending CI with the Reviewer's token", func() {
		reviewIssue()
		ci([]map[string]any{{"name": "build", "status": "in_progress"}}, nil)

		Expect(poll()).Error().NotTo(HaveOccurred())

		var ciCalls int

		for _, call := range h.gh.Calls() {
			if !strings.Contains(call.Path, "/commits/") {
				continue
			}

			ciCalls++

			Expect(call.Header.Get("Authorization")).To(Equal("Bearer minted-token-reviewer"))
		}

		Expect(ciCalls).NotTo(BeZero())
	})

	It("does not gate the Implementer's queue", func() {
		h.queue(map[string][]map[string]any{workflow.LabelImplement: {{"number": 5}}})
		ciFails()

		Expect(poll()).Error().NotTo(HaveOccurred())
		Expect(h.runner.find("enqueue")).To(HaveLen(1))
	})
})

var _ = Describe("the Implementer's next run", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
	})

	// transition renders a handoff marker between two states.
	transition := func(role workflow.Role, run string, from, to workflow.State) string {
		GinkgoHelper()

		body, err := workflow.RenderTransition(workflow.Transition{
			Role: role, RunID: run, From: from, To: to, SHA: "abc123",
		})
		Expect(err).NotTo(HaveOccurred())

		return body
	}

	It("carries the failed CI report into the prompt", func() {
		h.implementerGH(map[string]any{}, nil)
		h.writeToken(workflow.Implementer, "repo-token\n")
		h.implementerWrites(reworkOutput)
		h.seedComment(transition(workflow.Implementer, "r1", workflow.Implement, workflow.Review))
		h.seedComment(workflow.RenderCIFailure(workflow.CIReport{
			SHA: "abc123",
			Checks: []workflow.CheckRun{{
				Name: "build", Status: "completed", Conclusion: "failure",
				Output: workflow.CheckOutput{Summary: "the compiler said no"},
			}},
			Logs: []workflow.JobLog{{Name: "build", Tail: "line one\nline two"}},
		}))
		h.seedComment(transition(workflow.Reviewer, "r2", workflow.Review, workflow.Implement))

		_, err := Run(context.Background(), h.deps, workflow.Implementer, "o/r", testAgent(), testRetro(), 3, 5)
		Expect(err).NotTo(HaveOccurred())

		Expect(h.runCalls()).To(HaveLen(1))
		prompt := joined(h.runCalls()[0])
		Expect(prompt).To(ContainSubstring("# CI failure report"))
		Expect(prompt).To(ContainSubstring("**build** (failure): the compiler said no"))
		Expect(prompt).To(ContainSubstring("line one"))
		Expect(prompt).To(ContainSubstring("line two"))
	})

	It("counts a CI failure toward the Reviewer's round limit", func() {
		h.reviewerGH([]workflow.Item{{Body: workflow.ReviewMarker("s1")}})
		h.seedComment(workflow.RenderCIFailure(workflow.CIReport{SHA: "s2"}))
		h.opencodeWrites("code-review", h.reviewOut(), blockingReview)

		code, err := Run(context.Background(), h.deps, workflow.Reviewer, "o/r", testAgent(), testRetro(), 3, 5)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(workflow.ReviewExhausted))
	})

	It("leaves an earlier round's report out", func() {
		h.implementerGH(map[string]any{}, nil)
		h.writeToken(workflow.Implementer, "repo-token\n")
		h.implementerWrites(reworkOutput)
		h.seedComment(transition(workflow.Implementer, "r1", workflow.Implement, workflow.Review))
		h.seedComment(workflow.RenderCIFailure(workflow.CIReport{SHA: "abc123"}))
		h.seedComment(transition(workflow.Reviewer, "r2", workflow.Review, workflow.Implement))
		h.seedComment(transition(workflow.Implementer, "r3", workflow.Implement, workflow.Review))

		_, err := Run(context.Background(), h.deps, workflow.Implementer, "o/r", testAgent(), testRetro(), 3, 5)
		Expect(err).NotTo(HaveOccurred())

		Expect(joined(h.runCalls()[0])).NotTo(ContainSubstring("# CI failure report"))
	})
})
