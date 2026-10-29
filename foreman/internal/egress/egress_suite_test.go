package egress

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestEgress runs the egress suite.
func TestEgress(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "egress")
}
