package tests_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// helperTimeout bounds one run of the helper, which signs a JWT and makes a few requests.
const helperTimeout = 2 * time.Minute

// helper is the executable under test: foreman, whose token and repositories verbs are the helper.
func helper() string {
	return foremanBinary
}

// result is what one run of the helper did.
type result struct {
	code           int
	stdout, stderr string
}

// run executes the helper with the arguments, against the fake API and the credentials in conf.
func run(api, conf string, args ...string) result {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(context.Background(), helperTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper(), args...)
	cmd.Env = append(os.Environ(), "GITHUB_API_URL="+api, "GITHUB_APP_DIR="+conf)
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	Expect(ctx.Err()).NotTo(HaveOccurred(), "helper outlived %s", helperTimeout)
	code := 0
	if err != nil {
		var exit *exec.ExitError
		Expect(errors.As(err, &exit)).To(BeTrue(), "helper did not run: %v", err)
		code = exit.ExitCode()
	}
	return result{code: code, stdout: strings.TrimRight(stdout.String(), "\n"), stderr: stderr.String()}
}

// writeApps gives each App its own id and a private key under conf. "implementer" is App 1 and
// "reviewer" is App 2; "researcher" has no credentials. "paged", "wide" and "crowded" stand past a
// ceiling of the listing the helper reads.
func writeApps(conf string) {
	GinkgoHelper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	Expect(err).NotTo(HaveOccurred())
	der, err := x509.MarshalPKCS8PrivateKey(key)
	Expect(err).NotTo(HaveOccurred())
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for name, id := range map[string]string{
		"implementer": "1", "reviewer": "2", "paged": "3", "wide": "4",
		"crowded": "5", "planner": "6", "curator": "7",
	} {
		dir := filepath.Join(conf, name)
		Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "app-id"), []byte(id+"\n"), 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "private-key.pem"), pemKey, 0o600)).To(Succeed())
	}
}

var _ = Describe("the foreman token helper", Ordered, func() {
	var api, conf string

	BeforeAll(func() {
		server := fakeGitHub()
		DeferCleanup(server.Close)
		api = server.URL
		conf = GinkgoT().TempDir()
		writeApps(conf)
	})

	expect := func(code int, out string, args ...string) {
		GinkgoHelper()
		got := run(api, conf, args...)
		Expect(got.stdout).To(Equal(out))
		if code == 0 {
			Expect(got.code).To(BeZero(), "stderr: %s", got.stderr)
		} else {
			Expect(got.code).NotTo(BeZero())
		}
	}

	DescribeTable("exit code and stdout",
		expect,
		Entry("implementer mints its own repo-scoped token", 0, "tok-1-7-r", "token", "--app", "implementer", "--repo", "o/r"),
		Entry("reviewer mints its own repo-scoped token", 0, "tok-2-7-r", "token", "--app", "reviewer", "--repo", "o/r"),
		Entry("a read-only token is restricted to read permissions", 0,
			"tok-1-7-r-ro-contents+issues+metadata+pull_requests", "token", "--app", "implementer", "--repo", "o/r", "--read-only"),
		Entry("a read-only token is still one repository", 0,
			"tok-2-7-r-ro-contents+issues+metadata+pull_requests", "token", "--app", "reviewer", "--repo", "o/r", "--read-only"),
		Entry("a Planner read-only token asks for no Pull requests, which its App lacks", 0,
			"tok-6-7-r-ro-contents+issues+metadata", "token", "--app", "planner", "--repo", "o/r", "--read-only"),
		Entry("a Curator read-only token asks for no Contents or Pull requests, which its App lacks", 0,
			"tok-7-7-r-ro-issues+metadata", "token", "--app", "curator", "--repo", "o/r", "--read-only"),
		Entry("a read-only token for an unknown agent role is refused", 1, "",
			"token", "--app", "paged", "--repo", "o/paged", "--read-only"),
		Entry("an App not installed on the repo mints nothing", 1, "", "token", "--app", "implementer", "--repo", "o/q"),
		Entry("an App without credentials mints nothing, and takes no other App's", 1, "",
			"token", "--app", "researcher", "--repo", "o/r"),
		Entry("an app name cannot climb out of the config dir", 1, "", "token", "--app", "../implementer", "--repo", "o/r"),
		Entry("a repo must be owner/name", 1, "", "token", "--app", "implementer", "--repo", "r"),
		Entry("a repo cannot traverse", 1, "", "token", "--app", "implementer", "--repo", "../r"),
		Entry("token needs --app", 1, "", "token", "--repo", "o/r"),
		Entry("implementer lists its repositories", 0, "o/r", "repositories", "--app", "implementer"),
		Entry("reviewer lists its repositories across installations", 0, "o/r\no/q", "repositories", "--app", "reviewer"),
		Entry("an App without credentials lists nothing", 1, "", "repositories", "--app", "researcher"),
		Entry("unknown command", 1, "", "frobnicate", "--app", "implementer"),
	)

	DescribeTable("help",
		func(args ...string) {
			got := run(api, conf, args...)
			Expect(got.code).To(BeZero(), "stderr: %s", got.stderr)
			Expect(got.stdout).To(ContainSubstring("usage:"))
		},
		Entry("the top level has --help", "--help"),
		Entry("each command has --help (token)", "token", "--help"),
		Entry("each command has --help (repositories)", "repositories", "--help"),
	)

	DescribeTable("a refusal says why on stderr and prints nothing on stdout",
		func(want string, args ...string) {
			got := run(api, conf, args...)
			Expect(got.code).NotTo(BeZero())
			Expect(got.stdout).To(BeEmpty())
			Expect(got.stderr).To(ContainSubstring(want))
		},
		Entry("token needs --app, and says so", "usage:", "token", "--repo", "o/r"),
		Entry("repositories needs --app, and says so", "usage:", "repositories"),
		Entry("a repeated value flag is refused", "--app", "token", "--app", "implementer", "--app", "reviewer", "--repo", "o/r"),
		Entry("a repeated switch is refused", "--read-only",
			"token", "--app", "implementer", "--repo", "o/r", "--read-only", "--read-only"),
		Entry("a flag the command does not take is refused", "--repo", "repositories", "--app", "implementer", "--repo", "o/r"),
		Entry("a conflicting --read-only on repositories is refused", "unrecognized arguments: --read-only",
			"repositories", "--app", "implementer", "--read-only"),
		Entry("a conflicting --read-only value on token is refused", "ignored explicit argument 'x'",
			"token", "--app", "implementer", "--repo", "o/r", "--read-only=x"),
		Entry("a flag with no value is refused", "--repo", "token", "--app", "implementer", "--repo"),
		// A listing that reaches its ceiling is a failure, never a truncated answer.
		// A full page of installations is already over the installation cap, so it stops on page one.
		Entry("an App whose installation listing is full on its last page lists nothing", "over the 50",
			"repositories", "--app", "paged"),
		Entry("an App whose repository listing is full on its last page lists nothing", "longer than 10 pages",
			"repositories", "--app", "wide"),
		Entry("an App installed past the installation cap lists nothing", "over the 50",
			"repositories", "--app", "crowded"),
	)
})
