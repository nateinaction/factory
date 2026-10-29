package workflow_test

import (
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("the CI gate", func() {
	// check is a check run as GitHub reports it.
	check := func(name, status, conclusion string) workflow.CheckRun {
		return workflow.CheckRun{Name: name, Status: status, Conclusion: conclusion}
	}

	// status is a commit status named ci/lint as GitHub reports it.
	status := func(state string) workflow.CommitStatus {
		return workflow.CommitStatus{Context: "ci/lint", State: state}
	}

	DescribeTable("classifies one commit's CI",
		func(report workflow.CIReport, want workflow.CIState) {
			Expect(workflow.Classify(report)).To(Equal(want))
		},
		Entry("no checks at all", workflow.CIReport{}, workflow.CINone),
		Entry("every check passed", workflow.CIReport{Checks: []workflow.CheckRun{
			check("build", "completed", "success"),
			check("lint", "completed", "neutral"),
			check("docs", "completed", "skipped"),
		}}, workflow.CIPassed),
		Entry("a pending check", workflow.CIReport{Checks: []workflow.CheckRun{
			check("build", "in_progress", ""),
		}}, workflow.CIPending),
		Entry("a queued check", workflow.CIReport{Checks: []workflow.CheckRun{
			check("build", "queued", ""),
		}}, workflow.CIPending),
		Entry("a failed check", workflow.CIReport{Checks: []workflow.CheckRun{
			check("build", "completed", "failure"),
		}}, workflow.CIFailed),
		Entry("a timed-out check", workflow.CIReport{Checks: []workflow.CheckRun{
			check("build", "completed", "timed_out"),
		}}, workflow.CIFailed),
		Entry("a failure outranks a pending check", workflow.CIReport{Checks: []workflow.CheckRun{
			check("build", "completed", "failure"), check("lint", "in_progress", ""),
		}}, workflow.CIFailed),
		Entry("a pending status", workflow.CIReport{Statuses: []workflow.CommitStatus{
			status("pending"),
		}}, workflow.CIPending),
		Entry("a successful status", workflow.CIReport{Statuses: []workflow.CommitStatus{
			status("success"),
		}}, workflow.CIPassed),
		Entry("a failing status", workflow.CIReport{Statuses: []workflow.CommitStatus{
			status("failure"),
		}}, workflow.CIFailed),
		Entry("an errored status", workflow.CIReport{Statuses: []workflow.CommitStatus{
			status("error"),
		}}, workflow.CIFailed),
	)

	DescribeTable("decides what the review queue does",
		func(state workflow.CIState, expired bool, want workflow.CIDecision) {
			Expect(workflow.DecideCI(state, expired)).To(Equal(want))
		},
		Entry("all passed", workflow.CIPassed, false, workflow.CIClaim),
		Entry("no checks", workflow.CINone, false, workflow.CIClaim),
		Entry("pending before the deadline", workflow.CIPending, false, workflow.CIWait),
		Entry("pending past the deadline", workflow.CIPending, true, workflow.CIFail),
		Entry("failed before the deadline", workflow.CIFailed, false, workflow.CIBack),
		Entry("failed past the deadline", workflow.CIFailed, true, workflow.CIBack),
	)

	It("expires pending CI only past its deadline", func() {
		since := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

		Expect(workflow.PendingExpired(since, since.Add(59*time.Minute), time.Hour)).To(BeFalse())
		Expect(workflow.PendingExpired(since, since.Add(time.Hour), time.Hour)).To(BeFalse())
		Expect(workflow.PendingExpired(since, since.Add(61*time.Minute), time.Hour)).To(BeTrue())
	})

	It("reaches the round limit only when the count gets there", func() {
		Expect(workflow.RoundLimitReached(2, 3)).To(BeFalse())
		Expect(workflow.RoundLimitReached(3, 3)).To(BeTrue())
		Expect(workflow.RoundLimitReached(4, 3)).To(BeTrue())
	})

	It("keeps the end of a job log, by character", func() {
		Expect(workflow.Tail("0123456789", 4)).To(Equal("6789"))
		Expect(workflow.Tail("short", 10)).To(Equal("short"))
		Expect(workflow.Tail("anything", 0)).To(BeEmpty())
		Expect(workflow.Tail("éééé", 2)).To(Equal("éé"))
	})
})

var _ = Describe("the CI failure report", func() {
	report := workflow.CIReport{
		SHA: "abc123",
		Checks: []workflow.CheckRun{
			{
				Name: "build", Status: "completed", Conclusion: "failure",
				Output: workflow.CheckOutput{Title: "Build failed", Summary: "the compiler said no"},
			},
			{Name: "lint", Status: "completed", Conclusion: "success"},
		},
		Statuses: []workflow.CommitStatus{
			{Context: "ci/coverage", State: "failure", Description: "coverage fell"},
		},
		Logs: []workflow.JobLog{{Name: "build", Tail: "line one\nline two"}},
	}

	It("starts with a marker keyed to the commit and lists the failed checks", func() {
		body := workflow.RenderCIFailure(report)

		Expect(strings.SplitN(body, "\n", 2)[0]).To(Equal("<!-- factory:ci-failure sha=abc123 -->"))
		Expect(body).To(And(
			ContainSubstring("**build** (failure): the compiler said no"),
			ContainSubstring("**ci/coverage** (failure): coverage fell"),
			ContainSubstring("line one\nline two"),
		))
		Expect(body).NotTo(ContainSubstring("**lint**"), "listed a check that passed")
	})

	It("is found once posted for its commit, and never for another commit", func() {
		comments := []workflow.Item{{
			ID: 1, Body: workflow.RenderCIFailure(report), Author: factoryApp(workflow.Reviewer),
		}}

		Expect(workflow.HasCIFailure(comments, report.SHA, "o")).To(BeTrue())
		Expect(workflow.HasCIFailure(comments, "other", "o")).To(BeFalse())
	})

	It("reads back the report without its marker", func() {
		body := workflow.RenderCIFailure(report)

		text := workflow.CIFailureText(body)
		Expect(text).NotTo(ContainSubstring("<!-- factory:"))
		Expect(text).To(ContainSubstring("the compiler said no"))
	})

	It("stays within the comment limit", func() {
		body := workflow.RenderCIFailure(workflow.CIReport{
			SHA: "abc",
			Checks: []workflow.CheckRun{{
				Name: "build", Conclusion: "failure",
				Output: workflow.CheckOutput{Summary: strings.Repeat("x", workflow.CommentLimitChars)},
			}},
			Logs: []workflow.JobLog{{Name: "build", Tail: strings.Repeat("y", workflow.CommentLimitChars)}},
		})
		Expect([]rune(body)).To(HaveLen(workflow.CommentLimitChars))
	})

	It("is the report a run reads only when it comes after the handoff to review", func() {
		reviewMarker, err := workflow.RenderTransition(workflow.Transition{
			Role: workflow.Implementer, RunID: "r1",
			From: workflow.Implement, To: workflow.Review, SHA: "abc123",
		})
		Expect(err).NotTo(HaveOccurred())

		implementMarker, err := workflow.RenderTransition(workflow.Transition{
			Role: workflow.Reviewer, RunID: "r2",
			From: workflow.Review, To: workflow.Implement, SHA: "abc123",
		})
		Expect(err).NotTo(HaveOccurred())

		reportBody := workflow.RenderCIFailure(report)

		comments := []workflow.Item{
			{ID: 1, Body: reviewMarker, Author: factoryApp(workflow.Implementer)},
			{ID: 2, Body: reportBody, Author: factoryApp(workflow.Reviewer)},
			{ID: 3, Body: implementMarker, Author: factoryApp(workflow.Reviewer)},
		}

		got, ok, err := workflow.LatestCIFailure(comments, "o")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(got.ID).To(Equal(int64(2)))

		// A report from an earlier round, before the latest handoff to review, is stale.
		stale := []workflow.Item{
			{ID: 1, Body: reviewMarker, Author: factoryApp(workflow.Implementer)},
			{ID: 2, Body: reportBody, Author: factoryApp(workflow.Reviewer)},
			{ID: 3, Body: implementMarker, Author: factoryApp(workflow.Reviewer)},
			{ID: 4, Body: reviewMarker, Author: factoryApp(workflow.Implementer)},
		}
		_, ok, err = workflow.LatestCIFailure(stale, "o")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("is not read when there was no handoff to review", func() {
		_, ok, err := workflow.LatestCIFailure([]workflow.Item{
			{ID: 1, Body: workflow.RenderCIFailure(report), Author: factoryApp(workflow.Reviewer)},
		}, "o")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("ignores a CI failure report another author posted", func() {
		reviewMarker, err := workflow.RenderTransition(workflow.Transition{
			Role: workflow.Implementer, RunID: "r1",
			From: workflow.Implement, To: workflow.Review, SHA: "abc123",
		})
		Expect(err).NotTo(HaveOccurred())

		comments := []workflow.Item{
			{ID: 1, Body: reviewMarker, Author: factoryApp(workflow.Implementer)},
			{ID: 2, Body: workflow.RenderCIFailure(report), Author: "o"},
		}

		_, ok, err := workflow.LatestCIFailure(comments, "o")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
		Expect(workflow.HasCIFailure(comments, report.SHA, "o")).To(BeFalse())
	})

	It("skips a malformed handoff marker another author wrote", func() {
		reviewMarker, err := workflow.RenderTransition(workflow.Transition{
			Role: workflow.Implementer, RunID: "r1",
			From: workflow.Implement, To: workflow.Review, SHA: "abc123",
		})
		Expect(err).NotTo(HaveOccurred())

		comments := []workflow.Item{
			{ID: 1, Body: reviewMarker, Author: factoryApp(workflow.Implementer)},
			{ID: 2, Body: "<!-- factory:handoff nope -->", Author: "o"},
			{ID: 3, Body: workflow.RenderCIFailure(report), Author: factoryApp(workflow.Reviewer)},
		}

		got, ok, err := workflow.LatestCIFailure(comments, "o")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(got.ID).To(Equal(int64(3)))
	})
})

var _ = Describe("review rounds", func() {
	// The Reviewer's App posted every marked review, so each one is the factory's own record.
	reviews := []workflow.Item{
		{Body: workflow.ReviewMarker("a") + "\nx", Author: factoryApp(workflow.Reviewer)},
		{Body: workflow.ReviewMarker("a") + "\ny", Author: factoryApp(workflow.Reviewer)},
		{Body: workflow.ReviewMarker("b"), Author: factoryApp(workflow.Reviewer)},
		{Body: "human review"},
	}

	It("counts distinct reviewed commits, a repeat of one counting once", func() {
		Expect(workflow.ReviewRounds(reviews, nil, "a", "o")).To(Equal(2))
	})

	It("counts the current commit before its review is posted", func() {
		Expect(workflow.ReviewRounds(reviews, nil, "c", "o")).To(Equal(3))
	})

	It("counts a CI failure as a round on its commit", func() {
		comments := []workflow.Item{
			{Body: workflow.RenderCIFailure(workflow.CIReport{SHA: "d"}), Author: factoryApp(workflow.Reviewer)},
			{Body: "a human comment", Author: "o"},
		}

		Expect(workflow.ReviewRounds(reviews, comments, "d", "o")).To(Equal(3))
	})

	It("counts a commit once when both CI and a review named it", func() {
		comments := []workflow.Item{{
			Body: workflow.RenderCIFailure(workflow.CIReport{SHA: "a"}), Author: factoryApp(workflow.Reviewer),
		}}

		Expect(workflow.ReviewRounds(reviews, comments, "a", "o")).To(Equal(2))
	})

	It("does not count a CI failure another author posted", func() {
		comments := []workflow.Item{{
			Body: workflow.RenderCIFailure(workflow.CIReport{SHA: "d"}), Author: "o",
		}}

		Expect(workflow.ReviewRounds(reviews, comments, "a", "o")).To(Equal(2))
	})

	It("does not count a review another author posted", func() {
		forged := append(slices.Clone(reviews),
			workflow.Item{Body: workflow.ReviewMarker("d"), Author: "o"},
			workflow.Item{Body: workflow.ReviewMarker("e"), Author: "other-owner-factory-reviewer[bot]"})

		Expect(workflow.ReviewRounds(forged, nil, "a", "o")).To(Equal(2))
	})
})
