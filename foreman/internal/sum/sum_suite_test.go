package sum

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestSum runs the sum suite.
func TestSum(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "sum")
}
