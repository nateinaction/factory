package foreman

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeRunner records every process and answers through fn.
type fakeRunner struct {
	mu   sync.Mutex
	cmds []Cmd
	fn   func(Cmd) (string, error)
}

// Run records c and answers it.
func (r *fakeRunner) Run(_ context.Context, c Cmd) (string, error) {
	r.mu.Lock()
	r.cmds = append(r.cmds, c)
	r.mu.Unlock()
	if r.fn == nil {
		return "", nil
	}
	return r.fn(c)
}

// joined is the command line, for matching.
func joined(c Cmd) string { return strings.Join(c.Argv, " ") }

// find returns the recorded commands whose command line contains all the words.
func (r *fakeRunner) find(words ...string) []Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Cmd
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
	gh     *fakeGH
	runner *fakeRunner
	deps   Deps
	out    *bytes.Buffer
	errb   *bytes.Buffer
	dir    string
}

// newHarness builds a harness whose runner answers the helpers every flow needs.
func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	gh := newFakeGH(t)
	key := filepath.Join(dir, "openrouter-key")
	if err := os.WriteFile(key, []byte("or-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, gh: gh, runner: &fakeRunner{}, out: &bytes.Buffer{}, errb: &bytes.Buffer{}, dir: dir}
	h.runner.fn = h.defaultRun
	h.deps = Deps{
		Settings: Settings{
			API: gh.srv.URL, TokenFile: filepath.Join(dir, "gh-token"), KeyFile: key,
			WorkRoot: filepath.Join(dir, "work"), Self: "/usr/local/bin/foreman",
			TokenHelper: "/usr/local/bin/github-app-token", Dagu: "/usr/local/bin/dagu", WorkDAG: "finances-agent-work",
		},
		Runner: h.runner, HTTP: gh.srv.Client(), Out: h.out, Err: h.errb,
	}
	return h
}

// defaultRun answers token minting and git queries; everything else succeeds silently.
func (h *harness) defaultRun(c Cmd) (string, error) {
	line := joined(c)
	switch {
	case strings.Contains(line, "github-app-token"):
		return "minted-token\n", nil
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
