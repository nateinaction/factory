package workflow_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestWorkflow runs the workflow suite.
func TestWorkflow(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "workflow")
}
