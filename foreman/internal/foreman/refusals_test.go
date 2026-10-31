package foreman

import (
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/egress"
	"github.com/nateinaction/factory/foreman/internal/openrouter"
	"github.com/nateinaction/factory/foreman/internal/sandbox"
	"github.com/nateinaction/factory/foreman/internal/security"
)

var _ = Describe("the run's refusal tally", func() {
	It("is one tally both proxies count into", func() {
		tally := security.NewTally(nil)
		ctx := GinkgoT().Context()

		e, err := egress.StartWithLogger(ctx, sandbox.Network{}, "", "run-token", egress.Loopback, nil, tally)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(e.Close)

		k, err := openrouter.StartWithLogger(ctx, "key", "run-token", openrouter.Loopback, nil, tally)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(k.Close)

		// Neither proxy answers a request that carries no run token, so each refuses and counts.
		for _, url := range []string{e.URL(), k.URL()} {
			resp, err := http.Get(url + "/models")
			Expect(err).NotTo(HaveOccurred())
			// test cleanup: a close error changes no assertion
			_ = resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
		}

		Expect(tally.Counts()).To(Equal(map[security.Outcome]int{security.RefusedBadToken: 2}),
			"each proxy kept a tally of its own")
	})
})
