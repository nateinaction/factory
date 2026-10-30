package egress

import (
	"testing"
	"time"

	"github.com/onsi/gomega/gleak"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestEgress runs the egress suite.
func TestEgress(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "egress")
}

// leakSettle bounds how long a goroutine that is still returning gets before it counts as a leak.
const leakSettle = 5 * time.Second

// Every spec ends with the goroutines it began with: a proxy, tunnel or listener a spec starts must
// stop when the spec's cleanups have run. Eventually gives a goroutine that is still returning time to do so.
var _ = BeforeEach(func() {
	before := gleak.Goroutines()

	DeferCleanup(func(ctx SpecContext) {
		Eventually(gleak.Goroutines).WithContext(ctx).WithTimeout(leakSettle).ShouldNot(gleak.HaveLeaked(before))
	})
})
