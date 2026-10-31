package tests_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"

	"github.com/nateinaction/factory/roletest"
)

// foremanPkg is the package that builds the executable under test, which the suite runs as a black box.
const foremanPkg = "github.com/nateinaction/factory/foreman/cmd/foreman"

// drainCheck is the built foreman the specs run; BeforeSuite sets it.
var drainCheck string

const drainTimeout = 30 * time.Second

// runDrainCheck runs the drain check with the fake dagu's state in dir and returns its exit code.
func runDrainCheck(dir string, args ...string) int {
	GinkgoHelper()
	script, err := filepath.Abs(drainCheck)
	Expect(err).NotTo(HaveOccurred())
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, script, drainArgs(args)...)
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

// drainArgs turns the specs' arguments, the dagu binary and then the DAG names, into foreman's command
// line. With no arguments at all it is the bare verb, a usage error.
func drainArgs(args []string) []string {
	if len(args) == 0 {
		return []string{"drain-check"}
	}
	return append([]string{"drain-check", "--dagu", args[0]}, args[1:]...)
}

var _ = BeforeSuite(func() {
	dir, err := os.MkdirTemp("", "foreman-")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(os.RemoveAll, dir)
	drainCheck = filepath.Join(dir, "foreman")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "build", "-o", drainCheck, foremanPkg).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(out))
})

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
		state("g", "Status: retrying\n", "")
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
	// Decided in the run model (dagu.StatusRetrying): a run about to restart can still change state.
	It("retrying run blocks", func() {
		Expect(runDrainCheck(dir, dagu, "g")).To(Equal(1))
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

var _ = Describe("the cutover guard task", func() {
	var tasks []any

	BeforeEach(func() {
		var doc any
		Expect(yaml.Unmarshal([]byte(readText("../tasks/drain.yml")), &doc)).To(Succeed())
		var ok bool
		tasks, ok = doc.([]any)
		Expect(ok).To(BeTrue(), "drain.yml is not a task list")
	})

	It("installs foreman before it asks, and never skips the check for want of foreman", func() {
		var guard tree
		install, ask := -1, -1
		for i, t := range tasks {
			task, ok := t.(tree)
			Expect(ok).To(BeTrue())
			if has(task, "ansible.builtin.command") {
				guard, ask = task, i
			}
			if has(task, "ansible.builtin.include_tasks") {
				install = i
			}
		}
		Expect(guard).NotTo(BeNil(), "no task runs foreman")
		Expect(at(guard, "ansible.builtin.command", "argv")).To(ContainSubstring("/usr/local/bin/foreman', 'drain-check'"))
		Expect(install).To(BeNumerically(">=", 0), "foreman is not installed before the check")
		Expect(install).To(BeNumerically("<", ask))
		Expect(strings.Join(strs(listAt(guard, "when")), " ")).NotTo(ContainSubstring("foreman"),
			"a host without foreman would skip the check, which fails open")
	})
})
