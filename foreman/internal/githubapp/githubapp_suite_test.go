package githubapp_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestGithubapp runs the package's specs.
func TestGithubapp(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "githubapp suite")
}
