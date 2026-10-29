package dagu

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestDagu runs the dagu adapter's specs.
func TestDagu(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "dagu suite")
}
