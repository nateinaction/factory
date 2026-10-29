package foreman

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// The ticket's slice: the Implementer and Reviewer path works end to end on the handoff channel.
var _ = Describe("the Implementer and Reviewer path", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
	})

	It("claims, implements, reviews and leaves the issue done", func() {
		h.queue(map[string][]map[string]any{workflow.LabelImplement: {{"number": 5}}})

		// The poll routes the claim to the Implementer's run.
		Expect(Poll(context.Background(), h.deps,
			pollConfig(workflow.Implementer, workflow.Reviewer))).Error().NotTo(HaveOccurred())
		Expect(h.labels(5)).To(ContainElement(workflow.LabelImplementing))

		// The Implementer's run opens the pull request and hands off to the Reviewer.
		h.sudoExit(0)
		Expect(Start(context.Background(), h.deps, testConfig(), workflow.Implementer, "o/r", 5)).To(BeZero())
		Expect(h.labels(5)).To(ContainElement(workflow.LabelReview))
		Expect(h.labels(5)).NotTo(ContainElement(workflow.LabelImplementing))

		h.implementerGH(map[string]any{}, nil)

		// The review queue claims it, then the Reviewer's verdict closes the loop.
		h.out.Reset()
		Expect(Poll(context.Background(), h.deps,
			pollConfig(workflow.Implementer, workflow.Reviewer))).Error().NotTo(HaveOccurred())
		Expect(h.labels(5)).To(ContainElement(workflow.LabelReviewing))

		h.sudoExit(workflow.ReviewClean)
		Expect(Start(context.Background(), h.deps, testConfig(), workflow.Reviewer, "o/r", 5)).To(BeZero())
		Expect(h.labels(5)).To(ConsistOf(workflow.LabelDone))

		markers := h.markerComments(5)
		Expect(markers).To(HaveLen(4))
		Expect(markers[2]).To(ContainSubstring("from=factory:review to=factory:reviewing"))
		Expect(markers[3]).To(ContainSubstring("from=factory:reviewing to=factory:done"))
	})
})
