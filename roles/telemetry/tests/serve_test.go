package tests_test

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

// serve runs the role's serve tasks against a fake tailscale that reports status, and returns the
// serve calls the role made.
func serve(status string) []string {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	bin := filepath.Join(dir, "bin")
	Expect(os.Mkdir(bin, 0o700)).To(Succeed())
	_, err := roletest.BuildFake(bin, "tailscale")
	Expect(err).NotTo(HaveOccurred())
	statusFile := filepath.Join(dir, "status")
	Expect(os.WriteFile(statusFile, []byte(status), 0o600)).To(Succeed())
	callsFile := filepath.Join(dir, "calls")

	roletest.Render("serve.yml", roletest.Options{Env: map[string]string{
		"PATH":                  bin + ":" + os.Getenv("PATH"),
		"TAILSCALE_STATUS_FILE": statusFile,
		"TAILSCALE_CALLS_FILE":  callsFile,
	}})

	calls, err := os.ReadFile(callsFile)
	if os.IsNotExist(err) {
		return nil // the role made no call, so the fake never created the file
	}
	Expect(err).NotTo(HaveOccurred())
	return strings.Split(strings.TrimSuffix(string(calls), "\n"), "\n")
}

// The serve check names the exact listener, not a port number inside a longer one.
var _ = Describe("the telemetry role's tailscale serve", func() {
	// Every listener here ends in an extra digit, so the old substring check saw each port as served.
	const unrelatedStatus = `# Tailscale Serve is forwarding

|-- tcp://factory-1.example.ts.net:143170 (tailnet only)
|--> tcp://127.0.0.1:143170
|-- tcp://factory-1.example.ts.net:19090 (tailnet only)
|--> tcp://127.0.0.1:19090
|-- tcp://factory-1.example.ts.net:31000 (tailnet only)
|--> tcp://127.0.0.1:31000
https://factory-1.example.ts.net:30000 (tailnet only)
|-- / proxy http://127.0.0.1:30000
`

	const exactStatus = `# Tailscale Serve is forwarding

|-- tcp://factory-1.example.ts.net:14317 (tailnet only)
|--> tcp://127.0.0.1:14317
|-- tcp://factory-1.example.ts.net:9090 (tailnet only)
|--> tcp://127.0.0.1:9090
|-- tcp://factory-1.example.ts.net:3100 (tailnet only)
|--> tcp://127.0.0.1:3100
https://factory-1.example.ts.net:3000 (tailnet only)
|-- / proxy http://127.0.0.1:3000
`

	It("every ingest port is served past an unrelated longer port", func() {
		Expect(serve(unrelatedStatus)).To(ConsistOf(
			"serve --bg --tcp=14317 tcp://127.0.0.1:14317",
			"serve --bg --tcp=9090 tcp://127.0.0.1:9090",
			"serve --bg --tcp=3100 tcp://127.0.0.1:3100",
			"serve --bg --https=3000 http://127.0.0.1:3000",
		))
	})

	It("an already-set-up serve is left alone", func() {
		Expect(serve(exactStatus)).To(BeEmpty())
	})
})
