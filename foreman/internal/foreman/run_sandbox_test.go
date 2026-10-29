package foreman

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/sandbox"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// srtSettings is the part of an srt settings file the specs read.
type srtSettings struct {
	Network struct {
		AllowedDomains []string `json:"allowedDomains"`
		HTTPProxyPort  int      `json:"httpProxyPort"`
	} `json:"network"`
	Filesystem struct {
		AllowRead  []string `json:"allowRead"`
		AllowWrite []string `json:"allowWrite"`
		DenyRead   []string `json:"denyRead"`
		DenyWrite  []string `json:"denyWrite"`
	} `json:"filesystem"`
}

// settingsPath is the settings file a sandboxed command names.
func settingsPath(c proc.Cmd) string {
	for i, arg := range c.Argv {
		if arg == "--settings" && i+1 < len(c.Argv) {
			return c.Argv[i+1]
		}
	}

	return ""
}

var _ = Describe("the sandbox", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
		h.implementerGH(nil, nil)
		h.writeToken(workflow.Implementer, "repo-token\n")
		h.roleWrites(workflow.Implementer, 5, firstRunOutput)
		h.roleWritesRetro(workflow.Implementer, validRetro)
	})

	// settingsOn reads the settings file of the run and retro commands as they start.
	settingsOn := func(calls *[]srtSettings) {
		h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
			if isModel(c) {
				raw, err := os.ReadFile(settingsPath(c))
				Expect(err).NotTo(HaveOccurred())

				var s srtSettings
				Expect(json.Unmarshal(raw, &s)).To(Succeed())

				*calls = append(*calls, s)
			}

			return base(c)
		})
	}

	It("runs the model and its retro under srt with the run's own settings", func() {
		var settings []srtSettings

		settingsOn(&settings)
		Expect(Run(GinkgoT().Context(), h.deps, workflow.Implementer, testRef(5), testAgent(), testRetro(), 3)).To(BeZero())

		calls := h.runner.find("srt --settings")
		Expect(calls).To(HaveLen(2), "want the run and its retro")
		Expect(settings).To(HaveLen(2))

		work := h.rolePaths(workflow.Implementer, 5).workdir
		runDir := h.runDir(workflow.Implementer, 5)
		scratch := h.rolePaths(workflow.Implementer, 5).scratch
		otherRun := h.runDir(workflow.Implementer, 6)
		otherScratch := h.rolePaths(workflow.Implementer, 6).scratch

		for _, c := range calls {
			Expect(c.Argv[0]).To(Equal("srt"))
			Expect(joined(c)).To(ContainSubstring("opencode run --agent implementer --format json"))
			Expect(c.Dir).To(Equal(work))
			Expect(c.ReplaceEnv).To(BeTrue(), "foreman's environment leaked into the model")
			Expect(c.Env).To(HaveKeyWithValue("HOME", filepath.Join(runDir, "home")))
			Expect(c.Env).To(HaveKeyWithValue("XDG_CACHE_HOME", filepath.Join(runDir, "home", ".cache")))
			Expect(c.Env).To(HaveKeyWithValue("XDG_CONFIG_HOME", filepath.Join(runDir, "home", ".config")))
			Expect(c.Env).To(HaveKeyWithValue("XDG_DATA_HOME", filepath.Join(runDir, "home", ".local", "share")))
			Expect(c.Env).To(HaveKeyWithValue("XDG_STATE_HOME", filepath.Join(runDir, "home", ".local", "state")))
			Expect(c.Env).To(HaveKeyWithValue("TMPDIR", filepath.Join(runDir, "tmp")))
			Expect(c.Env).To(HaveKeyWithValue("PATH", HavePrefix("/nix/store/toolchain/bin:")))
			Expect(c.Env).NotTo(HaveKey("OPENROUTER_API_KEY"), "the key reached the model")
			Expect(c.Env).NotTo(HaveKey("FACTORY_KEY_FILE"))
			Expect(c.Env["OPENCODE_CONFIG_CONTENT"]).To(ContainSubstring(`"apiKey":"`+testToken+`"`),
				"the model sends the run's token and never the OpenRouter key")

			var config struct {
				Permission struct {
					ExternalDirectory map[string]string `json:"external_directory"`
				} `json:"permission"`
			}
			Expect(json.Unmarshal([]byte(c.Env["OPENCODE_CONFIG_CONTENT"]), &config)).To(Succeed())
			Expect(config.Permission.ExternalDirectory).To(HaveKeyWithValue("*", "deny"),
				"every external path outside this run is denied")
			Expect(config.Permission.ExternalDirectory).To(HaveKeyWithValue(runDir, "allow"))
			Expect(config.Permission.ExternalDirectory).To(HaveKeyWithValue(scratch, "allow"))
			Expect(config.Permission.ExternalDirectory).To(
				HaveKeyWithValue(retroPath(h.deps.Settings.WorkRoot, 5, workflow.Implementer), "allow"))
			Expect(config.Permission.ExternalDirectory).NotTo(HaveKey(otherRun))
			Expect(config.Permission.ExternalDirectory).NotTo(HaveKey(otherScratch))
		}

		port := proxyPortOf(h.opencodeCalls()[0])

		for _, s := range settings {
			Expect(s.Network.HTTPProxyPort).To(Equal(port), "egress must cross foreman's proxy")
			Expect(s.Network.AllowedDomains).To(BeEmpty(), "the egress proxy enforces the allowlist")
			Expect(s.Filesystem.DenyRead).To(ConsistOf(append([]string{"/"},
				h.deps.Settings.Home, h.deps.Settings.WorkRoot, h.deps.Settings.DaguHome,
				h.deps.Settings.TokenDir, h.deps.Settings.KeyFile, "/nix/var/nix/daemon-socket")),
				"the host is hidden, and the named sensitive paths stay denied")
			Expect(s.Filesystem.AllowRead).To(ContainElements(work, runDir, scratch, h.deps.Settings.Skills))
			Expect(s.Filesystem.AllowRead).NotTo(ContainElement(otherRun))
			Expect(s.Filesystem.AllowRead).NotTo(ContainElement(otherScratch))
			Expect(s.Filesystem.AllowWrite).To(ConsistOf(
				work, runDir, scratch, retroPath(h.deps.Settings.WorkRoot, 5, workflow.Implementer),
			))
			Expect(s.Filesystem.DenyWrite).To(ConsistOf(filepath.Join(work, ".git")),
				"the model may edit the working tree but never commit")
		}
	})

	It("never lets an OTEL_ variable reach the model", func() {
		GinkgoT().Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
		GinkgoT().Setenv("OTEL_SERVICE_NAME", "foreman")
		GinkgoT().Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=test")

		Expect(Run(GinkgoT().Context(), h.deps, workflow.Implementer, testRef(5), testAgent(), testRetro(), 3)).To(BeZero())

		Expect(h.opencodeCalls()).NotTo(BeEmpty())

		for _, c := range h.opencodeCalls() {
			for name := range c.Env {
				Expect(name).NotTo(HavePrefix("OTEL_"),
					"the model learned where foreman exports telemetry")
			}
		}
	})

	It("deletes the run's HOME, XDG directories and toolchain afterwards", func() {
		Expect(Run(GinkgoT().Context(), h.deps, workflow.Implementer, testRef(5), testAgent(), testRetro(), 3)).To(BeZero())
		Expect(h.runDir(workflow.Implementer, 5)).NotTo(BeAnExistingFile())
		Expect(filepath.Join(h.deps.Settings.WorkRoot, toolchainDirName(workflow.Implementer, 5))).
			NotTo(BeAnExistingFile())
	})

	It("resolves the toolchain from the default branch's flake, not the working branch", func() {
		var workdir, toolchain string

		h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
			out, err := base(c)

			if len(c.Argv) > 2 && c.Argv[0] == "git" && c.Argv[1] == "clone" {
				if strings.Contains(joined(c), "--depth") {
					toolchain = c.Argv[len(c.Argv)-1]
					Expect(os.WriteFile(filepath.Join(toolchain, "flake.nix"), []byte("default"), 0o600)).To(Succeed())
				} else {
					workdir = c.Argv[len(c.Argv)-1]
					Expect(os.WriteFile(filepath.Join(workdir, "flake.nix"), []byte("working branch"), 0o600)).To(Succeed())
				}
			}

			return out, err
		})

		Expect(Run(GinkgoT().Context(), h.deps, workflow.Implementer, testRef(5), testAgent(), testRetro(), 3)).To(BeZero())
		Expect(workdir).NotTo(BeEmpty())
		Expect(toolchain).NotTo(Equal(workdir))

		clone := h.runner.find("--depth 1 --branch main")
		Expect(clone).To(HaveLen(1))

		nix := h.runner.find("nix print-dev-env --json")
		Expect(nix).To(HaveLen(1))
		Expect(joined(nix[0])).To(ContainSubstring("--option accept-flake-config false"))
		Expect(joined(nix[0])).To(ContainSubstring("--no-write-lock-file"))
		Expect(joined(nix[0])).To(ContainSubstring("--flake path:" + toolchain))
		Expect(joined(nix[0])).NotTo(ContainSubstring(workdir), "the working branch's flake was used")
		Expect(nix[0].Env).To(HaveKeyWithValue("PATH", HavePrefix("/nix/var/nix/profiles/default/bin:")))
		Expect(nix[0].Env).To(HaveKeyWithValue("HOME", filepath.Join(h.runDir(workflow.Implementer, 5), "home")))
	})

	It("fails closed when the sandbox backend is not configured", func() {
		h.deps.Settings.Srt = ""

		code, err := Run(GinkgoT().Context(), h.deps, workflow.Implementer, testRef(5), testAgent(), testRetro(), 3)
		Expect(err).To(MatchError(ContainSubstring("sandbox is not configured")))
		Expect(code).NotTo(BeZero())
		Expect(h.opencodeCalls()).To(BeEmpty(), "the model ran without its sandbox")
	})

	It("fails closed when the sandbox backend cannot start", func() {
		h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
			if c.Argv[0] == "srt" {
				return "", &proc.ExitError{Program: "srt", Code: 1}
			}

			return base(c)
		})

		code, err := Run(GinkgoT().Context(), h.deps, workflow.Implementer, testRef(5), testAgent(), testRetro(), 3)
		Expect(err).To(HaveOccurred())
		Expect(code).NotTo(BeZero())
		Expect(h.runner.find("opencode run")).To(HaveLen(1), "only the sandboxed attempt ran")
		Expect(h.runner.find("noreply.github.com push")).To(BeEmpty(), "work was published after failing closed")
	})

	It("fails closed when the egress proxy cannot bind", func() {
		p := &fakeProxy{url: "http://127.0.0.1:9/api/v1"}
		h.deps.Proxy = func(string, string, string) (Forwarder, error) { return p, nil }
		h.deps.Egress = func(context.Context, sandbox.Network, string, string) (EgressProxy, error) {
			return nil, errors.New("bind egress proxy: address already in use")
		}

		code, err := Run(GinkgoT().Context(), h.deps, workflow.Implementer, testRef(5), testAgent(), testRetro(), 3)
		Expect(err).To(MatchError(ContainSubstring("bind")))
		Expect(code).NotTo(BeZero())
		Expect(h.opencodeCalls()).To(BeEmpty(), "the model ran without an egress boundary")
		Expect(p.closed).To(BeTrue(), "the key proxy outlived the failed run")
	})

	It("fails closed when the run's proxy token is empty", func() {
		h.deps.Token = func() (string, error) { return "", nil }

		code, err := Run(GinkgoT().Context(), h.deps, workflow.Implementer, testRef(5), testAgent(), testRetro(), 3)
		Expect(err).To(MatchError(ContainSubstring("empty")))
		Expect(code).NotTo(BeZero())
		Expect(h.opencodeCalls()).To(BeEmpty(), "the model ran with a token both proxies would refuse")
	})

	It("fails closed when the egress proxy's address cannot be parsed", func() {
		p := &fakeProxy{url: "http://127.0.0.1:9/api/v1"}
		e := &fakeEgress{url: "http://127.0.0.1:9/api/v1", address: "not an address"}
		h.deps.Proxy = func(string, string, string) (Forwarder, error) { return p, nil }
		h.deps.Egress = func(context.Context, sandbox.Network, string, string) (EgressProxy, error) { return e, nil }

		code, err := Run(GinkgoT().Context(), h.deps, workflow.Implementer, testRef(5), testAgent(), testRetro(), 3)
		Expect(err).To(HaveOccurred())
		Expect(code).NotTo(BeZero())
		Expect(h.opencodeCalls()).To(BeEmpty(), "the model ran without an egress boundary")
		Expect(p.closed).To(BeTrue(), "the key proxy outlived the failed run")
		Expect(e.closed).To(BeTrue(), "the egress proxy outlived the failed run")
	})

	It("stops both proxies and removes the run's directories however the run ends", func() {
		p := &fakeProxy{url: "http://127.0.0.1:9/api/v1"}
		e := &fakeEgress{url: "http://127.0.0.1:9/api/v1", address: "127.0.0.1:9"}
		h.deps.Proxy = func(string, string, string) (Forwarder, error) { return p, nil }
		h.deps.Egress = func(context.Context, sandbox.Network, string, string) (EgressProxy, error) { return e, nil }
		h.answerOpencode(errStream("ContextOverflowError", 0))

		code, err := Run(GinkgoT().Context(), h.deps, workflow.Implementer, testRef(5), testAgent(), testRetro(), 3)
		Expect(err).To(HaveOccurred())
		Expect(code).NotTo(BeZero())
		Expect(p.closed).To(BeTrue(), "the key proxy outlived the failed run")
		Expect(e.closed).To(BeTrue(), "the egress proxy outlived the failed run")
		Expect(h.runDir(workflow.Implementer, 5)).NotTo(BeAnExistingFile(), "the run's directory outlived the run")
	})

	It("gives the proxy the role's allowlist and logs the domains it saw", func() {
		agent := testAgent()
		agent.Egress.AllowedDomains = []string{"api.example.com"}

		fake := &fakeEgress{
			url: "http://127.0.0.1:9/api/v1", address: "127.0.0.1:9", domains: []string{"api.example.com"},
		}
		h.deps.Egress = func(_ context.Context, policy sandbox.Network, _, _ string) (EgressProxy, error) {
			fake.policy = policy

			return fake, nil
		}

		Expect(Run(GinkgoT().Context(), h.deps, workflow.Implementer, testRef(5), agent, testRetro(), 3)).To(BeZero())
		Expect(fake.policy.Permits("api.example.com")).To(BeTrue())
		Expect(fake.policy.Permits("elsewhere.example")).To(BeFalse())

		lines := h.usageLines()
		Expect(lines).To(HaveLen(1))
		Expect(lines[0]).To(HaveKeyWithValue("domains", []any{"api.example.com"}))
	})

	It("refuses an egress allowlist entry that would not mean what it says", func() {
		agent := testAgent()
		agent.Egress.AllowedDomains = []string{"*"}

		code, err := Run(GinkgoT().Context(), h.deps, workflow.Implementer, testRef(5), agent, testRetro(), 3)
		Expect(err).To(MatchError(ContainSubstring("invalid egress domain")))
		Expect(code).NotTo(BeZero())
		Expect(h.opencodeCalls()).To(BeEmpty())
	})

	It("puts the Planner and its retro under srt with a read-only clone", func() {
		var settings []srtSettings

		h.installed[workflow.Planner] = []string{"o/r"}
		h.writeToken(workflow.Planner, "repo-token\n")
		h.ensureIssue(5)
		h.roleWrites(workflow.Planner, 5, plannerSingle)
		h.roleWritesRetro(workflow.Planner, validRetro)
		settingsOn(&settings)

		Expect(Run(GinkgoT().Context(), h.deps, workflow.Planner, testRef(5), testAgent(), testRetro(), 3)).To(BeZero())

		Expect(h.runner.find("srt --settings")).To(HaveLen(2), "want the run and its retro")
		Expect(settings).To(HaveLen(2))
		Expect(h.runner.find("git clone", h.rolePaths(workflow.Planner, 5).workdir)).To(HaveLen(1),
			"the Planner reads the default branch's docs from a clone")

		runDir := h.runDir(workflow.Planner, 5)
		clone := h.rolePaths(workflow.Planner, 5).workdir
		scratch := h.rolePaths(workflow.Planner, 5).scratch

		for _, s := range settings {
			Expect(s.Filesystem.AllowWrite).To(ConsistOf(
				scratch, runDir, retroPath(h.deps.Settings.WorkRoot, 5, workflow.Planner),
			))
			Expect(s.Filesystem.AllowWrite).NotTo(ContainElement(clone), "the Planner may not change its clone")
			Expect(s.Filesystem.AllowRead).To(ContainElement(clone))
			Expect(s.Filesystem.AllowRead).NotTo(ContainElement(""), "an empty path re-allowed the host")
		}
	})

	It("puts the Researcher and its retro under srt", func() {
		h.installed[workflow.Researcher] = []string{"o/r"}
		h.writeToken(workflow.Researcher, "repo-token\n")
		h.researchTicket()
		h.roleWrites(workflow.Researcher, 5, researcherRun)
		h.roleWritesRetro(workflow.Researcher, validRetro)

		Expect(Run(GinkgoT().Context(), h.deps, workflow.Researcher, testRef(5), testAgent(), testRetro(), 3)).To(BeZero())

		Expect(h.runner.find("srt --settings")).To(HaveLen(2), "want the run and its retro")
		Expect(h.opencodeCalls()).To(HaveLen(2))
	})
})

// proxyPortOf is the egress proxy port an opencode command's base URL names.
func proxyPortOf(c proc.Cmd) int {
	parsed, err := url.Parse(baseURL(c))
	Expect(err).NotTo(HaveOccurred())

	port, err := strconv.Atoi(parsed.Port())
	Expect(err).NotTo(HaveOccurred())

	return port
}
