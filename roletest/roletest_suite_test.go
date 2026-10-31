package roletest_test

import (
	"os"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

// echoFake is the fake the helper's specs build: it prints its name and arguments, then exits 7.
func echoFake(name string, args []string) int {
	_, _ = os.Stdout.WriteString(name + " " + strings.Join(args, " ") + "\n")
	return 7
}

// TestMain lets the test binary answer as a fake when it is re-executed under a fake's name.
func TestMain(m *testing.M) {
	roletest.RunFake(map[string]roletest.FakeMain{"fake-echo": echoFake})
	os.Exit(m.Run())
}

func TestRoletest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Roletest Suite")
}
