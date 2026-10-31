package tests_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// buildTimeout bounds compiling foreman for the suite.
const buildTimeout = 5 * time.Minute

// foremanBinary is the path of the foreman executable the specs run, built once before they start.
var foremanBinary string

func TestGithubAppRole(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "github_app role Suite")
}

var _ = BeforeSuite(func() {
	dir, err := os.MkdirTemp("", "foreman-build-")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(os.RemoveAll, dir)
	foremanBinary = filepath.Join(dir, "foreman")

	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	// Foreman is its own module; go.work lets the root module build it by import path.
	build := exec.CommandContext(ctx, "go", "build", "-o", foremanBinary, "github.com/nateinaction/factory/foreman/cmd/foreman")
	out, err := build.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "go build foreman: %s", out)
})
