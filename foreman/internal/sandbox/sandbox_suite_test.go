package sandbox

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestSandbox runs the sandbox suite.
func TestSandbox(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "sandbox")
}
