package foreman

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/fakegh"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// fakeRunner records every process and answers through fn.
type fakeRunner struct {
	mu   sync.Mutex
	cmds []proc.Cmd
	fn   func(proc.Cmd) (string, error)
}

// Run records c and answers it.
func (r *fakeRunner) Run(_ context.Context, c proc.Cmd) (string, error) {
	r.mu.Lock()
	r.cmds = append(r.cmds, c)
	r.mu.Unlock()
	if r.fn == nil {
		return "", nil
	}
	return r.fn(c)
}

// joined is the command line, for matching.
func joined(c proc.Cmd) string { return strings.Join(c.Argv, " ") }

// find returns the recorded commands whose command line contains all the words.
func (r *fakeRunner) find(words ...string) []proc.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []proc.Cmd
	for _, c := range r.cmds {
		line := joined(c)
		ok := true
		for _, w := range words {
			ok = ok && strings.Contains(line, w)
		}
		if ok {
			out = append(out, c)
		}
	}
	return out
}

// harness wires Deps to a fake GitHub, a fake runner and a temp directory.
type harness struct {
	t      *testing.T
	gh     *fakegh.Server
	runner *fakeRunner
	deps   Deps
	out    *bytes.Buffer
	errb   *bytes.Buffer
	dir    string
	// installed maps an agent role to the repositories its App is installed on.
	installed map[workflow.Role][]string
	sleeps    []time.Duration
}

// testAgent is the agent role settings tests run with.
var testAgent = config.AgentConfig{Model: "prov/model", ContextCap: 200_000, Compaction: true}

// newHarness builds a harness whose runner answers the helpers every flow needs.
func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	gh := fakegh.New(t)
	key := filepath.Join(dir, "openrouter-key")
	if err := os.WriteFile(key, []byte("or-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, gh: gh, runner: &fakeRunner{}, out: &bytes.Buffer{}, errb: &bytes.Buffer{}, dir: dir,
		installed: map[workflow.Role][]string{workflow.Implementer: {"o/r"}, workflow.Reviewer: {"o/r"}}}
	h.runner.fn = h.defaultRun
	h.deps = Deps{
		Settings: config.Settings{
			API: gh.URL(), TokenDir: dir, KeyFile: key,
			WorkRoot: filepath.Join(dir, "work"), Self: "/usr/local/bin/foreman",
			TokenHelper: "/usr/local/bin/github-app-token", Dagu: "/usr/local/bin/dagu", DAGPrefix: "factory-",
		},
		Runner: h.runner, HTTP: gh.Client(), Out: h.out, Err: h.errb,
		Sleep: func(_ context.Context, d time.Duration) error { h.sleeps = append(h.sleeps, d); return nil },
	}
	return h
}

// defaultRun answers token minting and git queries; everything else succeeds silently.
func (h *harness) defaultRun(c proc.Cmd) (string, error) {
	line := joined(c)
	switch {
	case c.Argv[0] == "opencode":
		return okStream("ses_1"), nil
	case strings.Contains(line, "github-app-token") && strings.Contains(line, " repositories "):
		return strings.Join(h.installed[workflow.Role(appOf(c))], "\n") + "\n", nil
	case strings.Contains(line, "github-app-token") && !slices.Contains(h.installed[workflow.Role(appOf(c))], repoOf(c)):
		return "", &proc.ExitError{Program: "github-app-token", Code: 1} // an App mints only where it is installed
	case strings.Contains(line, "github-app-token"):
		return "minted-token-" + appOf(c) + "\n", nil
	case strings.Contains(line, "rev-parse HEAD"):
		return "sha-head\n", nil
	case strings.Contains(line, "rev-list --count"):
		return "1\n", nil
	}
	return "", nil
}

// leakFree fails the test when a secret appears in any argv.
func (h *harness) leakFree(secrets ...string) {
	h.t.Helper()
	for _, c := range h.runner.cmds {
		for _, s := range secrets {
			if strings.Contains(joined(c), s) {
				h.t.Errorf("secret %q in argv: %v", s, c.Argv)
			}
		}
	}
}

// repoOf is the value of --repo in a github-app-token command line.
func repoOf(c proc.Cmd) string {
	for i, a := range c.Argv {
		if a == "--repo" && i+1 < len(c.Argv) {
			return c.Argv[i+1]
		}
	}
	return ""
}

// appOf is the value of --app in a github-app-token command line.
func appOf(c proc.Cmd) string {
	for i, a := range c.Argv {
		if a == "--app" && i+1 < len(c.Argv) {
			return c.Argv[i+1]
		}
	}
	return ""
}
