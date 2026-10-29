package foreman

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/egress"
	"github.com/nateinaction/factory/foreman/internal/sandbox"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// acceptanceToken is the per-run proxy token the acceptance test's model sends.
const acceptanceToken = "acceptance-run-token"

// opencodeConfig is the inline configuration that points a real opencode run at the acceptance
// test's fake provider, with a named implementer agent.
const opencodeConfig = `{
  "provider": {"openrouter": {
    "options": {"baseURL": %q, "apiKey": %q},
    "models": {"acceptance": {"id": "acceptance/model"}}
  }},
  "agent": {"implementer": {"mode": "primary", "model": "openrouter/acceptance", "permission": "allow"}},
  "share": "disabled",
  "autoupdate": false
}`

// completion is a streamed chat completion, the shape opencode reads.
const completion = `data: {"id":"x","object":"chat.completion.chunk","model":"acceptance/model",` +
	`"choices":[{"index":0,"delta":{"role":"assistant","content":"OK"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"x","object":"chat.completion.chunk","model":"acceptance/model",` +
	`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6,"cost":0}}` + "\n\n" +
	"data: [DONE]\n\n"

// fakeProvider answers the two OpenRouter endpoints opencode calls.
func fakeProvider() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models":
			w.Header().Set("Content-Type", "application/json")
			// writing to a test ResponseWriter cannot change the assertion
			_, _ = io.WriteString(w, `{"data":[{"id":"acceptance/model","object":"model"}]}`)
		case "/api/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			// writing to a test ResponseWriter cannot change the assertion
			_, _ = io.WriteString(w, completion)

			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		default:
			http.NotFound(w, r)
		}
	}))
}

// pinnedOpenCodeVersion is the harness version the factory runs. The acceptance test exercises that
// version; another one, such as a locally installed 2.x, has a different provider and service model.
const pinnedOpenCodeVersion = "1.19.3"

// loopbackPort is the port of a loopback host:port address.
func loopbackPort(address string) int {
	_, portText, err := net.SplitHostPort(address)
	Expect(err).NotTo(HaveOccurred())

	port, err := strconv.Atoi(portText)
	Expect(err).NotTo(HaveOccurred())

	return port
}

// openNetwork is open egress, the default for every agent role.
func openNetwork() sandbox.Network {
	n, err := sandbox.ParseNetwork(nil)
	Expect(err).NotTo(HaveOccurred())

	return n
}

var _ = Describe("acceptance: srt filesystem", func() {
	It("hides a path outside the run and keeps the run directory writable", func() {
		srtBinary, err := exec.LookPath("srt")
		if err != nil {
			Skip("srt is not installed on this host; the sandbox acceptance test needs the pinned backend")
		}

		root := realTempDir()
		runDir := filepath.Join(root, "run")
		secretDir := filepath.Join(root, "real-home")

		Expect(os.MkdirAll(runDir, 0o700)).To(Succeed())
		Expect(os.MkdirAll(secretDir, 0o700)).To(Succeed())

		secret := filepath.Join(secretDir, "id_rsa")
		Expect(os.WriteFile(secret, []byte("private"), 0o600)).To(Succeed())

		settings := filepath.Join(runDir, "srt.json")
		raw, err := sandbox.Settings(sandbox.Spec{
			ReadWrite: []string{runDir}, Hidden: []string{secretDir}, Network: openNetwork(),
		}, loopbackPort(freePort()))
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(settings, raw, 0o600)).To(Succeed())

		// A read of the hidden file fails, while a write inside the run directory succeeds.
		script := fmt.Sprintf("cat %s 2>/dev/null || echo hidden; echo writable > %s; cat %s",
			secret, filepath.Join(runDir, "out"), filepath.Join(runDir, "out"))

		out, err := exec.CommandContext( //nolint:gosec // the acceptance test runs the pinned backend
			GinkgoT().Context(), srtBinary, "--settings", settings, "--", "sh", "-c", script,
		).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
		Expect(string(out)).To(ContainSubstring("hidden"))
		Expect(string(out)).NotTo(ContainSubstring("private"))
		Expect(string(out)).To(ContainSubstring("writable"))
	})
})

// realTempDir is a temp directory with every symlink resolved. srt binds an allowed path where it
// really is, and macOS's /tmp is a symlink to /private/tmp, so a raw temp path is not the path the
// sandbox can write.
func realTempDir() string {
	dir := GinkgoT().TempDir()

	resolved, err := filepath.EvalSymlinks(dir)
	Expect(err).NotTo(HaveOccurred())

	return resolved
}

// freePort is a port number no process holds, for the settings file the no-network specs write.
func freePort() string {
	ln, err := (&net.ListenConfig{}).Listen(GinkgoT().Context(), "tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())

	address := ln.Addr().String()
	Expect(ln.Close()).To(Succeed())

	return address
}

var _ = Describe("acceptance: two sandboxed runs at once", func() {
	It("runs two runs on one host without either seeing the other", func() {
		srtBinary, err := exec.LookPath("srt")
		if err != nil {
			Skip("srt is not installed on this host; the run-cap acceptance test needs the pinned backend")
		}

		root := realTempDir()
		work := filepath.Join(root, "work")
		Expect(os.MkdirAll(work, 0o700)).To(Succeed())

		type run struct{ runDir, scratch string }

		runs := []run{
			{filepath.Join(work, "run-implementer-1"), filepath.Join(work, "implementer-1")},
			{filepath.Join(work, "run-implementer-2"), filepath.Join(work, "implementer-2")},
		}

		for i, r := range runs {
			Expect(os.MkdirAll(r.runDir, 0o700)).To(Succeed())
			Expect(os.MkdirAll(r.scratch, 0o700)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(r.scratch, "secret"), []byte(fmt.Sprintf("secret-%d", i)), 0o600)).
				To(Succeed())
		}

		// Each run gets the spec foreman builds for one run: its own directory and scratch writable,
		// the work root that holds every run hidden. Two of them at once is what a run cap above 1
		// means.
		settings := func(r run) string {
			spec := sandbox.Plan(workflow.Implementer, sandbox.Layout{
				RunDir: r.runDir, Scratch: r.scratch, Hidden: []string{work},
			})
			path := filepath.Join(work, filepath.Base(r.runDir)+".srt.json")

			raw, err := sandbox.Settings(spec, loopbackPort(freePort()))
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(path, raw, 0o600)).To(Succeed())

			return path
		}

		// Each run reads the other's scratch secret (it must fail), writes its own marker into its
		// own scratch directory, and reads it back.
		script := func(i int) string {
			other := runs[(i+1)%len(runs)]

			return fmt.Sprintf("cat %s 2>/dev/null || echo hidden; echo own-%d > %s; cat %s",
				filepath.Join(other.scratch, "secret"), i, filepath.Join(runs[i].scratch, "out"),
				filepath.Join(runs[i].scratch, "out"))
		}

		type result struct {
			out []byte
			err error
		}

		type job struct {
			settings string
			script   string
		}

		jobs := make([]job, 0, len(runs))

		for i, r := range runs {
			jobs = append(jobs, job{settings: settings(r), script: script(i)})
		}

		results := make([]result, len(runs))

		var wg sync.WaitGroup

		wg.Add(len(runs))

		for i, j := range jobs {
			go func(i int, j job) {
				defer GinkgoRecover()
				defer wg.Done()

				out, err := exec.CommandContext( //nolint:gosec // the acceptance test runs the pinned backend
					GinkgoT().Context(), srtBinary,
					"--settings", j.settings, "--", "sh", "-c", j.script,
				).CombinedOutput()

				results[i] = result{out: out, err: err}
			}(i, j)
		}

		wg.Wait()

		for i, got := range results {
			other := (i + 1) % len(runs)

			Expect(got.err).NotTo(HaveOccurred(), string(got.out))
			Expect(string(got.out)).To(ContainSubstring("hidden"), "a run saw the other run's scratch directory")
			Expect(string(got.out)).NotTo(ContainSubstring(fmt.Sprintf("secret-%d", other)))
			Expect(string(got.out)).To(ContainSubstring(fmt.Sprintf("own-%d", i)),
				"a run could not write its own scratch directory")
		}
	})
})

var _ = Describe("acceptance: opencode run under srt", func() {
	It("runs the model inside the sandbox on an arm64 worker host", func() {
		srtBinary, err := exec.LookPath("srt")
		if err != nil {
			Skip("srt is not installed on this host; the sandbox acceptance test needs the pinned backend")
		}

		opencodeBinary, err := exec.LookPath("opencode")
		if err != nil {
			Skip("opencode is not installed on this host")
		}

		ctx := GinkgoT().Context()
		version, err := exec.CommandContext(ctx, opencodeBinary, "--version").Output() //nolint:gosec // the pinned binary
		Expect(err).NotTo(HaveOccurred())

		if strings.TrimSpace(string(version)) != pinnedOpenCodeVersion {
			Skip(fmt.Sprintf("opencode %s is installed, but the factory pins %s",
				strings.TrimSpace(string(version)), pinnedOpenCodeVersion))
		}

		provider := fakeProvider()
		DeferCleanup(provider.Close)

		proxy, err := egress.StartWithLogger(
			GinkgoT().Context(), openNetwork(), provider.URL+"/api/v1", acceptanceToken, egress.Loopback, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(proxy.Close)

		root := realTempDir()
		runDir := filepath.Join(root, "run")
		home := filepath.Join(runDir, "home")
		Expect(os.MkdirAll(home, 0o700)).To(Succeed())

		settings := filepath.Join(runDir, "srt.json")
		raw, err := sandbox.Settings(sandbox.Spec{
			ReadWrite: []string{runDir}, ReadOnly: []string{"/nix/store"},
			Network: openNetwork(),
		}, loopbackPort(proxy.Address()))
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(settings, raw, 0o600)).To(Succeed())

		ctx, cancel := context.WithTimeout(GinkgoT().Context(), 90*time.Second)
		defer cancel()

		cmd := exec.CommandContext(
			ctx, srtBinary, //nolint:gosec // the acceptance test runs the pinned backend
			"--settings", settings, "--",
			opencodeBinary, "run", "--agent", "implementer", "--format", "json", "Say OK",
		)
		cmd.Dir = root
		cmd.WaitDelay = 10 * time.Second
		cmd.Env = []string{
			"PATH=" + filepath.Dir(opencodeBinary) + ":/usr/local/bin:/usr/bin:/bin",
			"HOME=" + home,
			"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
			"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
			"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
			"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
			"TMPDIR=" + root,
			"LANG=C.UTF-8",
			"TERM=dumb",
			"OPENCODE_DISABLE_PROJECT_CONFIG=1",
			"OPENCODE_DISABLE_EXTERNAL_SKILLS=1",
			"OPENCODE_DB=" + filepath.Join(runDir, "opencode.db"),
			"OPENCODE_CONFIG_CONTENT=" + fmt.Sprintf(opencodeConfig, proxy.URL(), acceptanceToken),
		}

		var out bytes.Buffer

		cmd.Stdout = &out
		cmd.Stderr = &out

		err = cmd.Run()
		Expect(err).NotTo(HaveOccurred(), out.String())
		Expect(out.String()).To(ContainSubstring("sessionID"), out.String())
		Expect(proxy.Domains()).To(ContainElement(egress.APIDomain))
	})
})
