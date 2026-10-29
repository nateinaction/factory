package opencode

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("adding usage", func() {
	DescribeTable("sums the tokens and cost of two attempts, keeping a field neither reported unknown",
		func(a, b, want Usage) {
			Expect(a.Add(b)).To(Equal(want))
		},
		Entry("neither attempt reported anything", Usage{}, Usage{}, Usage{}),
		Entry("only the first reported", Usage{Input: new(10), Cached: new(4)}, Usage{},
			Usage{Input: new(10), Cached: new(4)}),
		Entry("only the second reported", Usage{}, Usage{Output: new(2), Cost: new(0.25)},
			Usage{Output: new(2), Cost: new(0.25)}),
		Entry("each reported what the other did not", Usage{Input: new(10), Output: new(2)},
			Usage{Cost: new(0.25)}, Usage{Input: new(10), Output: new(2), Cost: new(0.25)}),
		Entry("both reported every field", Usage{Input: new(10), Output: new(2), Cached: new(4), Cost: new(0.25)},
			Usage{Input: new(5), Output: new(1), Cached: new(6), Cost: new(0.5)},
			Usage{Input: new(15), Output: new(3), Cached: new(10), Cost: new(0.75)}),
	)

	DescribeTable("keeps the largest context peak, not the sum",
		func(a, b, want Usage) {
			Expect(a.Add(b).PeakContext).To(Equal(want.PeakContext))
		},
		Entry("neither attempt reported a peak", Usage{}, Usage{}, Usage{}),
		Entry("only the first reported a peak", Usage{PeakContext: new(14)}, Usage{},
			Usage{PeakContext: new(14)}),
		Entry("only the second reported a peak", Usage{}, Usage{PeakContext: new(5)},
			Usage{PeakContext: new(5)}),
		Entry("the later attempt used less", Usage{PeakContext: new(14)}, Usage{PeakContext: new(5)},
			Usage{PeakContext: new(14)}),
		Entry("the later attempt used more", Usage{PeakContext: new(5)}, Usage{PeakContext: new(14)},
			Usage{PeakContext: new(14)}),
	)
})

var _ = Describe("ParseStream", func() {
	It("sums the tokens and cost of every step, keeping a field no step reported unknown", func() {
		out := `{"type":"step_finish","sessionID":"ses_1","part":{"cost":0.25,` +
			`"tokens":{"input":10,"output":2,"cache":{"read":4}}}}` + "\n" +
			`{"type":"step_finish","sessionID":"ses_1","part":{"tokens":{"input":5,"output":1}}}` + "\n"

		s := ParseStream(out)
		Expect(s.Steps).To(Equal(2))
		Expect(s.Usage).To(Equal(Usage{
			Input: new(15), Output: new(3), Cached: new(4), PeakContext: new(14), Cost: new(0.25),
		}))
	})

	It("reports a field no step mentioned as missing, never zero", func() {
		s := ParseStream(`{"type":"step_finish","sessionID":"ses_1","part":{"tokens":{"input":7}}}`)

		Expect(s.Steps).To(Equal(1))
		Expect(s.Usage).To(Equal(Usage{Input: new(7), PeakContext: new(7)}))
	})
})
