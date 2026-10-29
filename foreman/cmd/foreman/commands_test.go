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

// recordingRunner records the processes the command would start, answering every one with no output
// unless reply answers it.
type recordingRunner struct {
	mu    sync.Mutex
	cmds  []proc.Cmd
	reply func(proc.Cmd) (string, error)
}

// Run records c.
func (r *recordingRunner) Run(_ context.Context, c proc.Cmd) (string, error) {
	r.mu.Lock()
	r.cmds = append(r.cmds, c)
	r.mu.Unlock()

	if r.reply != nil {
		return r.reply(c)
	}

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
	sudo   bool
}

// newCLIHarness builds a command whose Ansible role config lives in a temp directory.
func newCLIHarness() *cliHarness {
	dir := GinkgoT().TempDir()

	h := &cliHarness{runner: &recordingRunner{}, home: dir, dir: dir}
	// dagu answers an empty run history, so a poll sees every agent role idle.
	h.runner.reply = func(c proc.Cmd) (string, error) {
		if len(c.Argv) > 1 && c.Argv[1] == "history" {
			return "[]", nil
		}

		return "", nil
	}
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

	root.SetOut(&h.out)
	root.SetErr(&h.errOut)

	cmd, err := executeCommand(context.Background(), root, args, h.sudo)

	return exit(h.logger, cmd, err)
}

// writeConfig writes text as the Ansible role config and returns its path.
func (h *cliHarness) writeConfig(text string) string {
	GinkgoHelper()

	path := filepath.Join(h.dir, "config.json")
	Expect(os.WriteFile(path, []byte(text), 0o600)).To(Succeed())

	return path
}

// validConfig is a config file with every agent role.
const validConfig = `{"roles":["implementer","reviewer"],"agents":{` +
	`"planner":{"model":"prov/model","context_cap":200000,"steps":40,"deadline_minutes":20,` +
	`"retro":{"steps":20,"deadline_minutes":15}},` +
	`"researcher":{"model":"prov/model","context_cap":200000,"steps":80,"deadline_minutes":45,` +
	`"retro":{"steps":20,"deadline_minutes":15}},` +
	`"implementer":{"model":"prov/model","context_cap":200000,"steps":200,"deadline_minutes":90,` +
	`"retro":{"steps":20,"deadline_minutes":15}},` +
	`"reviewer":{"model":"prov/model","context_cap":200000,"steps":60,"deadline_minutes":30,` +
	`"retro":{"steps":20,"deadline_minutes":15}},` +
	`"curator":{"model":"prov/model","context_cap":200000,"steps":60,"deadline_minutes":30,` +
	`"retro":{"steps":20,"deadline_minutes":15}}},` +
	`"dead_run_margin_minutes":10,` +
	`"review_round_limit":3,"ci_pending_deadline_minutes":60}`

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
			Entry("status with an argument", "status", "extra"),
			Entry("logs alone", "logs"),
			Entry("logs without an issue", "logs", "o/r"),
			Entry("logs with an extra argument", "logs", "o/r", "5", "extra"),
			Entry("logs with a malformed repository", "logs", "not-a-repo", "5"),
			Entry("logs with a word for an issue", "logs", "o/r", "five"),
			Entry("history alone", "history"),
			Entry("history with an extra argument", "history", "o/r", "5"),
			Entry("history with a malformed repository", "history", "not-a-repo"),
		)

		DescribeTable("fails closed on an unknown agent role",
			func(verb string) {
				Expect(h.execute(verb, "janitor", "o/r", "5", "--config", configPath)).To(Equal(ExitUsage))
				Expect(h.logs.String()).To(ContainSubstring("unknown agent role"))
				Expect(h.runner.cmds).To(BeEmpty())
			},
			Entry(nil, "start"), Entry(nil, "run"),
		)

		It("polls the agent roles the config names, after reading dagu read-only", func() {
			Expect(h.execute("poll", "--config", configPath)).To(BeZero())
			Expect(h.out.String()).To(ContainSubstring("nothing ready"))
			Expect(h.runner.lines()).To(ConsistOf(
				ContainSubstring("repositories --app implementer"),
				ContainSubstring("repositories --app reviewer"),
				And(
					ContainSubstring("/usr/local/bin/dagu history"),
					ContainSubstring("--status queued,running,waiting,not_started"),
				),
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
		Entry("no dead-run margin", `{"roles":["implementer"],"agents":{},"dead_run_margin_minutes":0}`),
		Entry("no review round limit", `{"roles":["implementer"],"agents":{},"review_round_limit":0}`),
		Entry("no CI pending deadline", `{"roles":["implementer"],"agents":{},"ci_pending_deadline_minutes":0}`),
		Entry("not JSON", `[`),
	)

	It("fails closed on a missing config", func() {
		path := filepath.Join(h.dir, "missing.json")

		Expect(h.execute("poll", "--config", path)).To(Equal(1))
	})

	Context("under sudo", func() {
		BeforeEach(func() {
			h.sudo = true
		})

		DescribeTable("refuses everything but a run with exactly its three arguments",
			func(args ...string) {
				Expect(h.execute(args...)).To(Equal(ExitUsage))
				Expect(h.logs.String()).To(ContainSubstring("sudo permits only `foreman run`"))
				Expect(h.runner.cmds).To(BeEmpty())
			},
			Entry("a settings flag", "run", "--srt", "/tmp/elsewhere", "implementer", "o/r", "5"),
			Entry("a flag before the verb", "--srt", "/tmp/elsewhere", "run", "implementer", "o/r", "5"),
			Entry("a flag after the arguments", "run", "implementer", "o/r", "5", "--config", "/tmp/elsewhere"),
			Entry("an assigned flag", "run", "--srt=/tmp/elsewhere", "implementer", "o/r", "5"),
			Entry("a help flag", "run", "--help"),
			Entry("a flag on another verb", "start", "--key-file", "/tmp/elsewhere", "implementer", "o/r", "5"),
			Entry("poll", "poll"),
			Entry("status", "status"),
			Entry("logs", "logs", "o/r", "5"),
			Entry("history", "history", "o/r"),
			Entry("start", "start", "implementer", "o/r", "5"),
			Entry("run with no arguments", "run"),
			Entry("run with one argument", "run", "implementer"),
			Entry("run with two arguments", "run", "implementer", "o/r"),
			Entry("run with an extra argument", "run", "implementer", "o/r", "5", "extra"),
			Entry("an empty argument", "run", "implementer", "", "5"),
		)

		It("accepts the agent role, repository and issue", func() {
			Expect(h.execute("run", "janitor", "o/r", "5")).To(Equal(ExitUsage))
			Expect(h.logs.String()).To(ContainSubstring("unknown agent role"))
			Expect(h.logs.String()).NotTo(ContainSubstring("sudo permits"))
			Expect(h.runner.cmds).To(BeEmpty())
		})
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
		Expect(help).To(ContainSubstring("status"))
		Expect(help).To(ContainSubstring("logs"))
		Expect(help).To(ContainSubstring("history"))
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
		Entry("status", "status", "foreman status [flags]"),
		Entry("logs", "logs", "foreman logs <owner/repo> <issue> [flags]"),
		Entry("history", "history", "foreman history <owner/repo> [flags]"),
	)

	Context("with no runs in dagu", func() {
		var configPath string

		BeforeEach(func() {
			configPath = h.writeConfig(validConfig)
			h.runner.reply = func(proc.Cmd) (string, error) { return "[]", nil }
		})

		It("reports nothing running, read-only", func() {
			Expect(h.execute("status", "--config", configPath)).To(BeZero())
			Expect(h.out.String()).To(ContainSubstring("Active runs"))
			Expect(h.out.String()).To(ContainSubstring("(none)"))
			Expect(h.out.String()).To(ContainSubstring("Recent usage"))

			for _, line := range h.runner.lines() {
				Expect(line).To(HavePrefix("/usr/local/bin/dagu "), "status ran something else")
			}
		})

		It("fails clearly for an issue with no run", func() {
			Expect(h.execute("logs", "o/r", "5")).To(Equal(1))
			Expect(h.logs.String()).To(ContainSubstring("no dagu run for o/r issue 5"))
		})

		It("documents --follow", func() {
			Expect(h.execute("logs", "--help")).To(BeZero())
			Expect(h.out.String()).To(ContainSubstring("--follow"))
		})
	})
})
