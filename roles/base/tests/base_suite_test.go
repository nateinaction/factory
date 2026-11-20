package tests_test

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

// TestMain answers for the fake systemctl: the banner specs link the test binary as `systemctl`.
func TestMain(m *testing.M) {
	roletest.RunFake(map[string]roletest.FakeMain{"systemctl": fakeSystemctl})
	os.Exit(m.Run())
}

func TestBaseRole(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "base role Suite")
}

// fakeSystemctl stands in for the three queries the banner makes. FAKE_DAGU_STATE is dagu's state
// (default active; is-active exits 3 for any other, as systemctl does), FAKE_DAGU_SINCE its start
// time, FAKE_FAILED_UNITS the number of failed units, and FAKE_SYSTEMCTL_FAIL makes every query fail.
func fakeSystemctl(_ string, args []string) int {
	if os.Getenv("FAKE_SYSTEMCTL_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "fake systemctl: failing as asked")
		return 1
	}
	switch strings.Join(args, " ") {
	case "is-active dagu":
		state := os.Getenv("FAKE_DAGU_STATE")
		if state == "" {
			state = "active"
		}
		fmt.Println(state)
		if state != "active" {
			return 3
		}
	case "show dagu -p ActiveEnterTimestamp --value":
		fmt.Println(os.Getenv("FAKE_DAGU_SINCE"))
	case "--failed --no-legend":
		for i := 0; i < failedUnits(); i++ {
			fmt.Printf("unit%d.service loaded failed failed A unit\n", i)
		}
	default:
		fmt.Fprintln(os.Stderr, "fake systemctl: unexpected arguments:", args)
		return 2
	}
	return 0
}

// failedUnits is FAKE_FAILED_UNITS as a count; unset or malformed means none.
func failedUnits() int {
	n, err := strconv.Atoi(os.Getenv("FAKE_FAILED_UNITS"))
	if err != nil {
		return 0
	}
	return n
}
