package proc

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestProc runs the proc suite.
func TestProc(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "proc")
}
