package tests_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

// TestMain answers for the fake tailscale: the serve specs link the test binary as `tailscale`.
func TestMain(m *testing.M) {
	roletest.RunFake(map[string]roletest.FakeMain{"tailscale": fakeTailscale})
	os.Exit(m.Run())
}

func TestTelemetryRole(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "telemetry role Suite")
}

// fakeTailscale stands in for `tailscale`. `serve status` prints the file named by
// TAILSCALE_STATUS_FILE; every other call is appended to TAILSCALE_CALLS_FILE, so a spec can see
// which serves the role decided to set up for a given status.
func fakeTailscale(_ string, args []string) int {
	if len(args) >= 2 && args[0] == "serve" && args[1] == "status" {
		status, err := os.ReadFile(os.Getenv("TAILSCALE_STATUS_FILE"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake tailscale:", err)
			return 1
		}
		if _, err := os.Stdout.Write(status); err != nil {
			fmt.Fprintln(os.Stderr, "fake tailscale:", err)
			return 1
		}
		return 0
	}
	calls, err := os.OpenFile(os.Getenv("TAILSCALE_CALLS_FILE"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake tailscale:", err)
		return 1
	}
	if _, err := fmt.Fprintln(calls, strings.Join(args, " ")); err != nil {
		fmt.Fprintln(os.Stderr, "fake tailscale:", err)
		_ = calls.Close() // the write error is the one reported
		return 1
	}
	if err := calls.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "fake tailscale:", err)
		return 1
	}
	return 0
}
