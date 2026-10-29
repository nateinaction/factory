package prompt

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestPrompt runs the prompt suite.
func TestPrompt(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "prompt")
}
