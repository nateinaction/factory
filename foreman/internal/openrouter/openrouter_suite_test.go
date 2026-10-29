package openrouter

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestOpenrouter runs the openrouter suite.
func TestOpenrouter(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "openrouter")
}
