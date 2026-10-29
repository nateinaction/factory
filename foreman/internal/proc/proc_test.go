package proc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ExecRunner", func() {
	It("returns a non-zero exit as an ExitError naming the code", func() {
		shell := shellPath()

		out, err := ExecRunner{}.Run(context.Background(), Cmd{
			Argv:    []string{shell, "-c", "echo before; exit 3"},
			Capture: true,
		})

		var ee *ExitError
		if !errors.As(err, &ee) {
			Fail(fmt.Sprintf("a non-zero exit is an *ExitError, got %v", err))
		}

		Expect(ee.Code).To(Equal(3))
		Expect(strings.TrimSpace(out)).To(Equal("before"))
	})

	It("reaches a child with a replaced environment empty but for what it was given", func() {
		GinkgoT().Setenv("FACTORY_PARENT_ONLY", "leaked")

		env := lookPath("env")

		out, err := ExecRunner{}.Run(context.Background(), Cmd{
			Argv:       []string{env},
			Env:        map[string]string{"FACTORY_GIVEN": "1"},
			ReplaceEnv: true,
			Capture:    true,
		})

		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Fields(out)).To(Equal([]string{"FACTORY_GIVEN=1"}))
	})
})

// lookPath finds a program the suite runs, failing the spec when the host lacks it.
func lookPath(program string) string {
	GinkgoHelper()

	path, err := exec.LookPath(program)
	Expect(err).NotTo(HaveOccurred(), program)

	return path
}

// shellPath finds the POSIX shell the suite uses to start children and grandchildren.
func shellPath() string {
	GinkgoHelper()

	return lookPath("sh")
}
