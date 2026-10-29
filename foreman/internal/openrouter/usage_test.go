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

	It("sums what separate responses reported, keeping a field nobody reported unknown", func() {
		first := Usage{Input: new(10), Output: new(2)}
		second := Usage{Cost: new(0.25)}

		total := first.Add(second)
		Expect(total.Input).To(HaveValue(BeEquivalentTo(10)))
		Expect(total.Output).To(HaveValue(BeEquivalentTo(2)))
		Expect(total.Cached).To(BeNil())
		Expect(total.Cost).To(HaveValue(BeEquivalentTo(0.25)))
	})
})
