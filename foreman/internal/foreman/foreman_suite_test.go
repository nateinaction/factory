package foreman

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestForeman runs the foreman suite.
func TestForeman(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "foreman")
}
