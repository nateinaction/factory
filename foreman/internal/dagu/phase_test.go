package dagu

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("the run model's active statuses", func() {
	// A run dagu is retrying will start again, so it can still change state and must block a drain.
	// dagu itself reports such a run as running; the model still names retrying so a status report
	// that says so is never mistaken for a finished run.
	It("counts a retrying run as active", func() {
		Expect(Run{Status: StatusRetrying}.Active()).To(BeTrue())
	})

	It("leaves retrying out of dagu's history filter, since dagu reports that run as running", func() {
		Expect(ActiveStatuses()).NotTo(ContainElement(StatusRetrying))
	})

	It("counts a finished run as not active", func() {
		for _, status := range []string{
			StatusSucceeded, StatusFailed, StatusCanceled, StatusCancelled, StatusAborted,
			StatusPartiallySucceeded, StatusRejected,
		} {
			Expect(Run{Status: status}.Active()).To(BeFalse(), status)
		}
	})
})

var _ = Describe("Classify", func() {
	DescribeTable("reads a status report",
		func(report string, want Phase) {
			Expect(Classify(report)).To(Equal(want))
		},
		Entry("running", "Status: running\n", PhaseActive),
		Entry("queued, in any case", "Status: Queued\n", PhaseActive),
		Entry("waiting", "waiting", PhaseActive),
		Entry("not started with a space", "Status: not started", PhaseActive),
		Entry("not started with an underscore", "Status: not_started", PhaseActive),
		Entry("retrying", "Status: retrying", PhaseActive),
		Entry("succeeded", "Status: succeeded\n", PhaseFinished),
		Entry("failed", "Status: failed\n", PhaseFinished),
		Entry("canceled", "Status: canceled", PhaseFinished),
		Entry("cancelled", "Status: cancelled", PhaseFinished),
		Entry("aborted", "Status: aborted", PhaseFinished),
		Entry("partially succeeded", "Status: partially succeeded", PhaseFinished),
		Entry("rejected", "Status: rejected", PhaseFinished),
		Entry("an active word wins over a finished one", "failed step, run retrying", PhaseActive),
		Entry("text with no status", "something unrecognised\n", PhaseUnknown),
		Entry("nothing", "", PhaseUnknown),
	)
})
