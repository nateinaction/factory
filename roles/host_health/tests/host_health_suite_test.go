package tests_test

import (
	"fmt"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

// TestMain answers for the fake vcgencmd: the reporter specs link the test binary as `vcgencmd`.
func TestMain(m *testing.M) {
	roletest.RunFake(map[string]roletest.FakeMain{"vcgencmd": fakeVcgencmd})
	os.Exit(m.Run())
}

func TestHostHealthRole(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "host_health role Suite")
}

// fakeVcgencmd stands in for `vcgencmd get_throttled`: it prints FAKE_THROTTLED, or fails when
// FAKE_VCGENCMD_FAIL is set.
func fakeVcgencmd(_ string, args []string) int {
	if os.Getenv("FAKE_VCGENCMD_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "fake vcgencmd: failing as asked")
		return 1
	}
	if len(args) != 1 || args[0] != "get_throttled" {
		fmt.Fprintln(os.Stderr, "fake vcgencmd: unexpected arguments:", args)
		return 2
	}
	if _, err := fmt.Println("throttled=" + os.Getenv("FAKE_THROTTLED")); err != nil {
		fmt.Fprintln(os.Stderr, "fake vcgencmd:", err)
		return 1
	}
	return 0
}
