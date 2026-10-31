package tests_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

// TestMain answers for the fake dagu before any spec runs: the drain check shells out to dagu, and
// the fake is this test binary re-executed under that name.
func TestMain(m *testing.M) {
	roletest.RunFake(map[string]roletest.FakeMain{"dagu": fakeDagu})
	os.Exit(m.Run())
}

// fakeDagu stands in for `dagu status <dag>`. It prints the file named for the DAG under $FAKE and
// exits with the number in that name plus ".rc", 0 when there is none. A DAG with no file is an error.
func fakeDagu(_ string, args []string) int {
	if len(args) != 2 || args[0] != "status" {
		return 64
	}
	dir := os.Getenv("FAKE")
	out, err := os.ReadFile(filepath.Join(dir, args[1]))
	if err != nil {
		return 1
	}
	if _, err := os.Stdout.Write(out); err != nil {
		return 1
	}
	rc := 0
	if raw, err := os.ReadFile(filepath.Join(dir, args[1]+".rc")); err == nil {
		if _, err := fmt.Sscanf(string(raw), "%d", &rc); err != nil {
			return 1
		}
	}
	return rc
}

func TestAgentRole(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "agent role Suite")
}
