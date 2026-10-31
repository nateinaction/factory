package tests_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

// drainCheck is the executable under test. Run as a black box, so a rewrite changes only this path.
const drainCheck = "../files/drain-check"

const drainTimeout = 30 * time.Second

// runDrainCheck runs the drain check with the fake dagu's state in dir and returns its exit code.
func runDrainCheck(dir string, args ...string) int {
	GinkgoHelper()
	script, err := filepath.Abs(drainCheck)
	Expect(err).NotTo(HaveOccurred())
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, script, args...)
	cmd.Env = append(os.Environ(), "FAKE="+dir)
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	Expect(errors.As(err, &exit)).To(BeTrue(), "drain-check did not run: %v", err)
	return exit.ExitCode()
}

var _ = Describe("the drain check", func() {
	var (
		dir  string
		dagu string
	)

	// state sets what the fake dagu prints for a DAG, and the exit code it gives when rc is not empty.
	state := func(dag, status, rc string) {
		GinkgoHelper()
		Expect(os.WriteFile(filepath.Join(dir, dag), []byte(status), 0o600)).To(Succeed())
		if rc != "" {
			Expect(os.WriteFile(filepath.Join(dir, dag+".rc"), []byte(rc), 0o600)).To(Succeed())
		}
	}

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		var err error
		dagu, err = roletest.BuildFake(dir, "dagu")
		Expect(err).NotTo(HaveOccurred())
		state("a", "Status: succeeded\n", "")
		state("b", "Status: failed\n", "")
		state("c", "Status: running\n", "")
		state("d", "Status: Queued\n", "")
		state("e", "something unrecognised\n", "")
		state("f", "no runs found\n", "1\n")
	})

	It("idle runs pass", func() {
		Expect(runDrainCheck(dir, dagu, "a", "b")).To(Equal(0))
	})
	It("running run blocks", func() {
		Expect(runDrainCheck(dir, dagu, "a", "c")).To(Equal(1))
	})
	It("queued run blocks (case-insensitive)", func() {
		Expect(runDrainCheck(dir, dagu, "d")).To(Equal(1))
	})
	It("unrecognised status fails closed", func() {
		Expect(runDrainCheck(dir, dagu, "e")).To(Equal(1))
	})
	It("dagu error fails closed", func() {
		Expect(runDrainCheck(dir, dagu, "missing")).To(Equal(1))
	})
	It("nonzero exit blocks even with benign text", func() {
		Expect(runDrainCheck(dir, dagu, "f")).To(Equal(1))
	})
	It("no DAG names is a usage error", func() {
		Expect(runDrainCheck(dir, dagu)).To(Equal(2))
	})
	It("no arguments is a usage error", func() {
		Expect(runDrainCheck(dir)).To(Equal(2))
	})
})
