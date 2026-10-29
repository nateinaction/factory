package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/foreman"
	"github.com/nateinaction/factory/foreman/internal/proc"
)

// recordingRunner records the processes the command would start, answering every one with no output.
type recordingRunner struct {
	mu   sync.Mutex
	cmds []proc.Cmd
}

// Run records c.
func (r *recordingRunner) Run(_ context.Context, c proc.Cmd) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.cmds = append(r.cmds, c)

	return "", nil
}

// lines are the recorded command lines.
func (r *recordingRunner) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, 0, len(r.cmds))
	for _, c := range r.cmds {
		out = append(out, strings.Join(c.Argv, " "))
	}

	return out
}

// cliHarness runs the CLI against a recording runner and captured streams.
type cliHarness struct {
	deps   *foreman.Deps
	runner *recordingRunner
	logger *slog.Logger
	out    bytes.Buffer
	errOut bytes.Buffer
	logs   bytes.Buffer
	home   string
	dir    string
}

// newCLIHarness builds a command whose role config lives in a temp directory.
func newCLIHarness() *cliHarness {
	dir := GinkgoT().TempDir()

	h := &cliHarness{runner: &recordingRunner{}, home: dir, dir: dir}
	h.logger = slog.New(slog.NewTextHandler(&h.logs, nil))
	h.deps = &foreman.Deps{Runner: h.runner, Out: &h.out, Log: h.logger}

	return h
}

// execute runs one invocation and returns its exit code. Each invocation gets a fresh settings source,
// as the real process does.
func (h *cliHarness) execute(args ...string) int {
	GinkgoHelper()

	root, err := newRootCommand(h.deps, config.SettingsViper(h.home))
	Expect(err).NotTo(HaveOccurred())

	root.SetArgs(args)
	root.SetOut(&h.out)
	root.SetErr(&h.errOut)

	cmd, err := root.ExecuteContextC(context.Background())

	return exit(h.logger, cmd, err)
}

// writeConfig writes text as the role config and returns its path.
func (h *cliHarness) writeConfig(text string) string {
	GinkgoHelper()

	path := filepath.Join(h.dir, "config.json")
	Expect(os.WriteFile(path, []byte(text), 0o600)).To(Succeed())

	return path
}

// validConfig is a config file with every agent role.
const validConfig = `{"roles":["implementer","reviewer"],"agents":{` +
	`"planner":{"model":"prov/model","context_cap":200000},"researcher":{"model":"prov/model","context_cap":200000},` +
	`"implementer":{"model":"prov/model","context_cap":200000},"reviewer":{"model":"prov/model","context_cap":200000},` +
	`"curator":{"model":"prov/model","context_cap":200000}}}`

var _ = Describe("the foreman command", func() {
	var h *cliHarness

	BeforeEach(func() {
		h = newCLIHarness()
	})

	Context("with a valid config", func() {
		var configPath string

		BeforeEach(func() {
			configPath = h.writeConfig(validConfig)
		})

		DescribeTable("rejects a bad invocation with the usage exit, doing nothing",
			func(args ...string) {
				Expect(h.execute(args...)).To(Equal(ExitUsage))
				Expect(h.errOut.String()).To(ContainSubstring("Usage:"))
				Expect(h.logs.String()).To(ContainSubstring("bad invocation"))
				Expect(h.runner.cmds).To(BeEmpty())
			},
			Entry("no verb"),
			Entry("unknown verb", "frobnicate"),
			Entry("poll with arguments", "poll", "extra"),
			Entry("start alone", "start"),
			Entry("start without an issue", "start", "implementer", "o/r"),
			Entry("start with an extra argument", "start", "implementer", "o/r", "5", "extra"),
			Entry("run without an issue", "run", "implementer", "o/r"),
			Entry("a malformed repository", "start", "implementer", "not-a-repo", "5"),
			Entry("a word for an issue", "start", "implementer", "o/r", "five"),
			Entry("an injected issue", "run", "implementer", "o/r", "5; rm -rf /"),
		)

		DescribeTable("fails closed on an unknown agent role",
			func(verb string) {
				Expect(h.execute(verb, "janitor", "o/r", "5", "--config", configPath)).To(Equal(ExitUsage))
				Expect(h.logs.String()).To(ContainSubstring("unknown agent role"))
				Expect(h.runner.cmds).To(BeEmpty())
			},
			Entry(nil, "start"), Entry(nil, "run"),
		)

		It("polls the agent roles the config names", func() {
			Expect(h.execute("poll", "--config", configPath)).To(BeZero())
			Expect(h.out.String()).To(ContainSubstring("nothing ready"))
			Expect(h.runner.lines()).To(ConsistOf(
				ContainSubstring("repositories --app implementer"),
				ContainSubstring("repositories --app reviewer"),
			))
		})
	})

	DescribeTable("fails closed on a bad config",
		func(text string) {
			path := h.writeConfig(text)

			Expect(h.execute("poll", "--config", path)).To(Equal(1))
			Expect(h.logs.String()).To(ContainSubstring("foreman failed"))
		},
		Entry("empty", ``),
		Entry("no fields", `{}`),
		Entry("unknown role", `{"roles":["janitor"],"agents":{}}`),
		Entry("unknown agent key", `{"roles":["implementer"],"agents":{"janitor":`+
			`{"model":"m","context_cap":1}}}`),
		Entry("unknown top-level key", `{"roles":["implementer"],"agents":{},"extra":true}`),
		Entry("no agents", `{"roles":["implementer"],"agents":{}}`),
		Entry("no roles", `{"roles":[],"agents":{}}`),
		Entry("roles missing", `{"agents":{}}`),
		Entry("not JSON", `[`),
	)

	It("fails closed on a missing config", func() {
		path := filepath.Join(h.dir, "missing.json")

		Expect(h.execute("poll", "--config", path)).To(Equal(1))
	})

	Context("settings precedence", func() {
		It("uses the host layout as the defaults", func() {
			GinkgoT().Setenv("FACTORY_API", "")
			GinkgoT().Setenv("FACTORY_WORK_ROOT", "")
			GinkgoT().Setenv("FACTORY_DAG_PREFIX", "")

			Expect(h.execute("poll", "--config", h.writeConfig(validConfig))).To(BeZero())
			Expect(h.deps.Settings.API).To(Equal("https://api.github.com"))
			Expect(h.deps.Settings.WorkRoot).To(Equal(filepath.Join(h.home, "work")))
			Expect(h.deps.Settings.DAGPrefix).To(Equal("factory-"))
		})

		It("lets the environment override a default", func() {
			GinkgoT().Setenv("FACTORY_DAG_PREFIX", "from-env-")

			Expect(h.execute("poll", "--config", h.writeConfig(validConfig))).To(BeZero())
			Expect(h.deps.Settings.DAGPrefix).To(Equal("from-env-"))
		})

		It("lets a flag override the environment", func() {
			GinkgoT().Setenv("FACTORY_WORK_ROOT", "/from/env")

			Expect(h.execute("poll", "--config", h.writeConfig(validConfig), "--work-root", "/from/flag")).To(BeZero())
			Expect(h.deps.Settings.WorkRoot).To(Equal("/from/flag"))
		})
	})

	It("documents every command in --help", func() {
		Expect(h.execute("--help")).To(BeZero())

		help := h.out.String()
		Expect(help).To(ContainSubstring("poll"))
		Expect(help).To(ContainSubstring("start"))
		Expect(help).To(ContainSubstring("run"))
		Expect(help).NotTo(ContainSubstring("completion"))
	})

	DescribeTable("documents a command's arguments in its help",
		func(verb, usage string) {
			Expect(h.execute(verb, "--help")).To(BeZero())
			Expect(h.out.String()).To(ContainSubstring(usage))
		},
		Entry("poll", "poll", "foreman poll [flags]"),
		Entry("start", "start", "foreman start <agent-role> <owner/repo> <issue> [flags]"),
		Entry("run", "run", "foreman run <agent-role> <owner/repo> <issue> [flags]"),
	)
})
