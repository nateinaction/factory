package opencode

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestOpencode runs the opencode suite.
func TestOpencode(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "opencode")
}
