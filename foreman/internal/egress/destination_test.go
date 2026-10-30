package egress

import (
	"net/netip"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("publiclyRoutable", func() {
	DescribeTable("refuses an address that carries a zone",
		func(raw string) {
			Expect(publiclyRoutable(netip.MustParseAddr(raw))).To(BeFalse())
		},
		// The unspecified address passes IsGlobalUnicast once it has a zone, and a dial reaches the local host.
		Entry("the unspecified address", "::%0"),
		Entry("loopback", "::1%lo"),
		Entry("link-local", "fe80::1%eth0"),
		Entry("an otherwise public address", "2001:4860:4860::8888%eth0"),
	)

	It("admits a public address without a zone", func() {
		Expect(publiclyRoutable(netip.MustParseAddr("2001:4860:4860::8888"))).To(BeTrue())
	})
})
