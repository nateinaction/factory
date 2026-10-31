package tests_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestGithubAppRole(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "github_app role Suite")
}
