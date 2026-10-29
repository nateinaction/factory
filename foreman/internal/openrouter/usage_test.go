package openrouter

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("reading usage from a response", func() {
	It("reads tokens, cached tokens and cost from OpenRouter's usage object", func() {
		raw := []byte(`{"id":"gen-1","usage":{"prompt_tokens":194,"completion_tokens":2,` +
			`"total_tokens":196,"cost":0.95,` +
			`"prompt_tokens_details":{"cached_tokens":100,"cache_write_tokens":5}}}`)

		u, ok := parseUsage(raw)
		Expect(ok).To(BeTrue())
		Expect(u.Input).To(HaveValue(BeEquivalentTo(194)))
		Expect(u.Output).To(HaveValue(BeEquivalentTo(2)))
		Expect(u.Cached).To(HaveValue(BeEquivalentTo(100)))
		Expect(u.Cost).To(HaveValue(BeEquivalentTo(0.95)))
	})

	It("reports nothing for a body without usage, never zero", func() {
		_, ok := parseUsage([]byte(`{"id":"gen-1"}`))
		Expect(ok).To(BeFalse())

		_, ok = parseUsage([]byte(`not json`))
		Expect(ok).To(BeFalse())
	})

	It("keeps the last usage a streamed response reported, not a running total", func() {
		var s usageStream

		_, err := s.Write([]byte("data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1}}\n\n"))
		Expect(err).NotTo(HaveOccurred())

		n, err := s.Write(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())

		_, err = s.Write([]byte("data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":9," +
			"\"cost\":0.5,\"prompt_tokens_details\":{\"cached_tokens\":4}}}\n\ndata: [DONE]\n\n"))
		Expect(err).NotTo(HaveOccurred())
		s.Finish()

		u, ok := s.Total()
		Expect(ok).To(BeTrue())
		Expect(u.Input).To(HaveValue(BeEquivalentTo(10)))
		Expect(u.Output).To(HaveValue(BeEquivalentTo(9)))
		Expect(u.Cached).To(HaveValue(BeEquivalentTo(4)))
		Expect(u.Cost).To(HaveValue(BeEquivalentTo(0.5)))
	})

	It("reads a non-streamed body that ends without a newline", func() {
		var s usageStream

		_, err := s.Write([]byte(`{"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
		Expect(err).NotTo(HaveOccurred())
		s.Finish()

		u, ok := s.Total()
		Expect(ok).To(BeTrue())
		Expect(u.Input).To(HaveValue(BeEquivalentTo(7)))
		Expect(u.Output).To(HaveValue(BeEquivalentTo(3)))
	})

	DescribeTable("sums what separate responses reported, keeping a field nobody reported unknown",
		func(first, second, want Usage) {
			Expect(first.Add(second)).To(Equal(want))
		},
		Entry("neither response reported anything", Usage{}, Usage{}, Usage{}),
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
})
