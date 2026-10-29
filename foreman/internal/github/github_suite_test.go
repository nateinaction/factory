package github_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/ghtest"
)

// emulator is the one GitHub emulator the package's specs share; each spec gets a fresh repository.
var emulator *ghtest.Emulate //nolint:gochecknoglobals // the package's specs share one emulator process

var _ = BeforeSuite(func() {
	var err error

	emulator, err = ghtest.StartEmulate(context.Background(), "../../emulate.config.yaml")
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	if emulator != nil {
		emulator.Close()
	}
})

// TestGitHub runs the github suite.
func TestGitHub(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "github")
}
