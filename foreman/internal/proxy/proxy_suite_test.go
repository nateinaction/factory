package proxy

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestProxy runs the proxy suite.
func TestProxy(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "proxy")
}
