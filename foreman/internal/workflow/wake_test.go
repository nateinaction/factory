package workflow_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// marker is the comment body of a handoff marker.
func marker(t workflow.Transition) string {
	body, err := workflow.RenderTransition(t)
	Expect(err).NotTo(HaveOccurred())

	return body
}

var _ = Describe("waking on a comment", func() {
	waiting := workflow.Transition{
		Role: workflow.Planner, RunID: "run-1", From: workflow.Planning, To: workflow.Waiting,
	}
	done := workflow.Transition{
		Role: workflow.Reviewer, RunID: "run-2", From: workflow.Reviewing, To: workflow.Done,
	}
	failed := workflow.Transition{
		Role: workflow.Implementer, RunID: "run-3", From: workflow.Implementing, To: workflow.Failed,
	}

	DescribeTable("moves the issue to the role that takes it",
		func(latest workflow.Transition, writer bool, want workflow.State, ok bool) {
			got, woke := workflow.WakeState(latest, true, writer)
			Expect(woke).To(Equal(ok))

			if ok {
				Expect(got).To(Equal(want))
			}
		},
		Entry("a waiting issue goes back to the Planner", waiting, true, workflow.Plan, true),
		Entry("a done issue goes back to the Implementer", done, true, workflow.Implement, true),
		Entry("a failed Implementer issue goes back to the Implementer", failed, true, workflow.Implement, true),
		Entry("a non-writer wakes nothing", waiting, false, workflow.State(""), false),
		Entry("a done issue with a non-writer wakes nothing", done, false, workflow.State(""), false),
	)

	It("sends a failed issue back to the role that failed", func() {
		for _, role := range workflow.KnownRoles() {
			if role == workflow.Curator {
				continue // the Curator has no queue
			}

			queued, ok := workflow.Queued(role)
			Expect(ok).To(BeTrue())

			failed := workflow.Transition{Role: role, From: workflow.Failed, To: workflow.Failed}
			got, woke := workflow.WakeState(failed, true, true)
			Expect(woke).To(BeTrue())
			Expect(got).To(Equal(queued))
		}
	})

	It("wakes nothing without a marker, or from a state no writer comment moves", func() {
		_, ok := workflow.WakeState(waiting, false, true)
		Expect(ok).To(BeFalse())

		claim := workflow.Transition{
			Role: workflow.Implementer, RunID: "run", From: workflow.Implement, To: workflow.Implementing,
		}

		_, ok = workflow.WakeState(claim, true, true)
		Expect(ok).To(BeFalse())
	})

	It("gives the poll every comment posted after the latest marker, oldest first", func() {
		comments := []workflow.Item{
			{ID: 1, Body: "before", Author: "o"},
			{ID: 2, Body: marker(failed), Author: factoryApp(failed.Role)},
			{ID: 3, Body: "first after", Author: "o"},
			{ID: 4, Body: "second after", Author: "o"},
		}

		latest, found, err := workflow.LatestMarker(comments)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(latest.ID).To(BeEquivalentTo(2))
		Expect(latest.Transition).To(Equal(failed))

		after := workflow.AfterMarker(comments, latest)
		Expect(after).To(Equal([]workflow.Item{
			{ID: 3, Body: "first after", Author: "o"},
			{ID: 4, Body: "second after", Author: "o"},
		}))
	})

	It("fails closed on a malformed marker the factory wrote", func() {
		_, _, err := workflow.LatestMarker([]workflow.Item{{
			ID: 1, Body: "<!-- factory:handoff role=planner -->", Author: factoryApp(workflow.Planner),
		}})
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("the guidance of a woken run", func() {
	waiting := workflow.Transition{
		Role: workflow.Planner, RunID: "run-1", From: workflow.Planning, To: workflow.Waiting,
	}
	wake := workflow.Transition{
		Role: workflow.Planner, RunID: "poll", From: workflow.Waiting, To: workflow.Plan,
	}
	claim := workflow.Transition{
		Role: workflow.Planner, RunID: "run-2", From: workflow.Plan, To: workflow.Planning,
	}
	done := workflow.Transition{
		Role: workflow.Reviewer, RunID: "run-3", From: workflow.Reviewing, To: workflow.Done,
	}
	implement := workflow.Transition{
		Role: workflow.Implementer, RunID: "poll", From: workflow.Done, To: workflow.Implement,
	}
	implementing := workflow.Transition{
		Role: workflow.Implementer, RunID: "run-4", From: workflow.Implement, To: workflow.Implementing,
	}

	It("reads every comment since the marker the wake came from, oldest first", func() {
		comments := []workflow.Item{
			{ID: 1, Body: "old chatter", Author: "o"},
			{ID: 2, Body: marker(waiting), Author: factoryApp(workflow.Planner)},
			{ID: 3, Body: "yes, do it", Author: "o"},
			{ID: 4, Body: marker(wake), Author: factoryApp(workflow.Planner)},
			{ID: 5, Body: marker(claim), Author: factoryApp(workflow.Planner)},
		}

		Expect(workflow.Guidance(comments)).To(Equal([]workflow.Item{{ID: 3, Body: "yes, do it", Author: "o"}}))
	})

	It("includes a comment posted on the pull request, merged by comment id", func() {
		comments := []workflow.Item{
			{ID: 2, Body: marker(done), Author: factoryApp(workflow.Reviewer)},
			{ID: 3, Body: "please also handle X", Author: "o"},
			{ID: 4, Body: marker(implement), Author: factoryApp(workflow.Reviewer)},
			{ID: 5, Body: marker(implementing), Author: factoryApp(workflow.Implementer)},
		}

		Expect(workflow.Guidance(comments)).To(Equal(
			[]workflow.Item{{ID: 3, Body: "please also handle X", Author: "o"}}))
	})

	It("does not let another author's forged marker become the wake", func() {
		comments := []workflow.Item{
			{ID: 1, Body: marker(waiting), Author: factoryApp(workflow.Planner)},
			{ID: 2, Body: marker(wake), Author: "o"},
			{ID: 3, Body: "a comment", Author: "o"},
			{ID: 4, Body: marker(claim), Author: factoryApp(workflow.Planner)},
		}

		Expect(workflow.Guidance(comments)).To(BeEmpty())
	})

	It("reads nothing when the issue was not woken", func() {
		comments := []workflow.Item{
			{ID: 1, Body: marker(waiting), Author: factoryApp(workflow.Planner)},
			{ID: 2, Body: "a comment", Author: "o"},
		}

		Expect(workflow.Guidance(comments)).To(BeEmpty())
	})

	It("reads nothing without a marker", func() {
		Expect(workflow.Guidance([]workflow.Item{{ID: 1, Body: "hello", Author: "o"}})).To(BeEmpty())
	})

	It("reads nothing on a later run that no wake started", func() {
		outcome := workflow.Transition{
			Role: workflow.Planner, RunID: "run-2", From: workflow.Planning, To: workflow.Waiting,
		}
		comments := []workflow.Item{
			{ID: 1, Body: marker(waiting), Author: factoryApp(workflow.Planner)},
			{ID: 2, Body: marker(wake), Author: factoryApp(workflow.Planner)},
			{ID: 3, Body: marker(claim), Author: factoryApp(workflow.Planner)},
			{ID: 4, Body: marker(outcome), Author: factoryApp(workflow.Planner)},
			{ID: 5, Body: "another question", Author: "o"},
		}

		Expect(workflow.Guidance(comments)).To(BeEmpty())
	})
})

var _ = Describe("dead runs", func() {
	start := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	deadline := 90 * time.Minute
	margin := 10 * time.Minute

	It("dates the running label by the latest labeled event", func() {
		events := []workflow.TimelineEvent{
			{
				Event: "labeled", Label: &workflow.TimelineLabel{Name: workflow.LabelImplementing},
				CreatedAt: start,
			},
			{Event: "labeled", Label: &workflow.TimelineLabel{Name: workflow.LabelReview}, CreatedAt: start.Add(time.Hour)},
			{
				Event: "labeled", Label: &workflow.TimelineLabel{Name: workflow.LabelImplementing},
				CreatedAt: start.Add(2 * time.Hour),
			},
		}

		got, found := workflow.LabeledAt(events, workflow.LabelImplementing)
		Expect(found).To(BeTrue())
		Expect(got).To(Equal(start.Add(2 * time.Hour)))
	})

	It("finds no date without a labeled event for the label", func() {
		events := []workflow.TimelineEvent{{Event: "closed", CreatedAt: start}}

		_, found := workflow.LabeledAt(events, workflow.LabelImplementing)
		Expect(found).To(BeFalse())
	})

	DescribeTable("is dead only past the deadline plus the margin",
		func(age time.Duration, dead bool) {
			Expect(workflow.DeadRun(start, start.Add(age), deadline, margin)).To(Equal(dead))
		},
		Entry("just under the deadline", deadline-1*time.Minute, false),
		Entry("at the deadline", deadline, false),
		Entry("in the margin", deadline+5*time.Minute, false),
		Entry("exactly at the boundary", deadline+margin, false),
		Entry("past the boundary", deadline+margin+time.Second, true),
	)

	It("names the role and running label of a claimed issue", func() {
		issue := workflow.Issue{Labels: []string{workflow.LabelReviewing}}

		role, state, ok := workflow.RunningState(issue)
		Expect(ok).To(BeTrue())
		Expect(role).To(Equal(workflow.Reviewer))
		Expect(state).To(Equal(workflow.Reviewing))

		_, _, ok = workflow.RunningState(workflow.Issue{Labels: []string{workflow.LabelReview}})
		Expect(ok).To(BeFalse())
	})
})
