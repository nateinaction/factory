package workflow_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("BuildHistory", func() {
	start := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)

	event := func(kind, label string, after time.Duration) workflow.TimelineEvent {
		e := workflow.TimelineEvent{Event: kind, CreatedAt: start.Add(after)}
		if label != "" {
			e.Label = &workflow.TimelineLabel{Name: label}
		}

		return e
	}

	It("ends a label still on the issue when the issue closed", func() {
		got, ok := workflow.BuildHistory(1, nil, []workflow.TimelineEvent{
			event("labeled", workflow.LabelReview, 0),
			event("closed", "", time.Hour),
		}, start.Add(24*time.Hour))

		Expect(ok).To(BeTrue())
		Expect(got.Times).To(Equal([]workflow.StateTime{{State: workflow.Review, Duration: time.Hour}}))
	})

	It("counts a label still on an open issue up to now", func() {
		got, _ := workflow.BuildHistory(1, nil, []workflow.TimelineEvent{
			event("labeled", workflow.LabelReviewing, 0),
		}, start.Add(2*time.Hour))

		Expect(got.Times).To(Equal([]workflow.StateTime{{State: workflow.Reviewing, Duration: 2 * time.Hour}}))
	})

	It("does not take a label named closed for the closed state", func() {
		_, ok := workflow.BuildHistory(1, nil, []workflow.TimelineEvent{event("labeled", "closed", 0)}, start)

		Expect(ok).To(BeFalse())
	})

	It("counts a review sent back to the Implementer from either review state", func() {
		markers := []workflow.Marker{
			{From: workflow.Reviewing, To: workflow.Implement},
			{From: workflow.Review, To: workflow.Implement},
			{From: workflow.Reviewing, To: workflow.Done},
		}

		got, ok := workflow.BuildHistory(1, markers, nil, start)

		Expect(ok).To(BeTrue())
		Expect(got.Rework).To(Equal(2))
		Expect(got.Outcome).To(Equal(workflow.Done))
	})
})
