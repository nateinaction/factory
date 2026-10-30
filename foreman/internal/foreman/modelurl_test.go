package foreman

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("modelBaseURL", func() {
	const host = "http://127.0.0.1:35881/api/v1"

	It("names the proxy bridge srt listens on inside a Linux sandbox, not the host's port", func() {
		Expect(modelBaseURL("linux", host)).To(Equal("http://localhost:3128/api/v1"))
	})

	It("keeps the host's port where the sandbox shares the host's loopback", func() {
		Expect(modelBaseURL("darwin", host)).To(Equal(host))
	})
})
