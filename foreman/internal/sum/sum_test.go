package sum

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("adding optional numbers", func() {
	DescribeTable("sums two optional ints, missing only when both are",
		func(a, b, want *int) {
			Expect(Add(a, b)).To(Equal(want))
		},
		Entry("neither reported", nil, nil, nil),
		Entry("only the left reported", new(7), nil, new(7)),
		Entry("only the right reported", nil, new(7), new(7)),
		Entry("both reported", new(10), new(2), new(12)),
	)

	DescribeTable("sums two optional floats, missing only when both are",
		func(a, b, want *float64) {
			Expect(Add(a, b)).To(Equal(want))
		},
		Entry("neither reported", nil, nil, nil),
		Entry("only the left reported", new(1.5), nil, new(1.5)),
		Entry("only the right reported", nil, new(1.5), new(1.5)),
		Entry("both reported", new(0.25), new(0.5), new(0.75)),
	)
})
