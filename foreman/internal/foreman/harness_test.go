package foreman

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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

// wrap makes fn answer the commands it claims, passing every other command to the runner's current answer.
func (r *fakeRunner) wrap(fn func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error)) {
	base := r.fn
	r.fn = func(c proc.Cmd) (string, error) { return fn(c, base) }
}

// harness wires Deps to a fake GitHub, a fake runner and a temp directory.
type harness struct {
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

// newHarness builds a harness whose runner answers the helpers every flow needs. The fake GitHub
// is closed when the spec ends, and any request it did not expect fails the spec.
func newHarness() *harness {
	dir := GinkgoT().TempDir()
	gh := fakegh.New()
	DeferCleanup(func() {
		Expect(gh.Unexpected()).To(BeEmpty(), "requests no route expected")
		gh.Close()
	})
	key := filepath.Join(dir, "openrouter-key")
	Expect(os.WriteFile(key, []byte("or-key\n"), 0o600)).To(Succeed())
	h := &harness{
		gh: gh, runner: &fakeRunner{}, out: &bytes.Buffer{}, errb: &bytes.Buffer{}, dir: dir,
		installed: map[workflow.Role][]string{workflow.Implementer: {"o/r"}, workflow.Reviewer: {"o/r"}},
	}
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

// expectLeakFree fails the spec when a secret appears in any argv.
func (h *harness) expectLeakFree(secrets ...string) {
	GinkgoHelper()
	for _, c := range h.runner.cmds {
		for _, s := range secrets {
			Expect(joined(c)).NotTo(ContainSubstring(s), "secret in argv")
		}
	}
}

// expectNoWork fails the spec when anything ran or reached GitHub.
func (h *harness) expectNoWork() {
	GinkgoHelper()
	Expect(h.runner.cmds).To(BeEmpty())
	Expect(h.gh.Calls()).To(BeEmpty())
}

// expectAuthorizedAs fails the spec when any GitHub call used a token other than role's.
func (h *harness) expectAuthorizedAs(role workflow.Role) {
	GinkgoHelper()
	for _, r := range h.gh.Calls() {
		Expect(r.Header.Get("Authorization")).To(Equal("Bearer minted-token-"+string(role)), r.Method+" "+r.Path)
	}
}

// expectOnlyReads fails the spec when any GitHub call wrote.
func (h *harness) expectOnlyReads() {
	GinkgoHelper()
	Expect(h.gh.Summary()).To(HaveEach(HavePrefix("GET ")))
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

// failMint makes minting fail for the App of role, passing every other command on. With onlyToken,
// listing the App's installations still works.
func (h *harness) failMint(role workflow.Role, onlyToken bool) {
	h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
		if strings.Contains(joined(c), "app-token") && appOf(c) == string(role) &&
			(!onlyToken || strings.Contains(joined(c), " token ")) {
			return "", &proc.ExitError{Program: "github-app-token", Code: 1}
		}
		return base(c)
	})
}

// withConfig writes a valid config file and points the harness at it.
func (h *harness) withConfig() {
	h.withConfigText(validConfig)
}

// withConfigText writes text as the config file and points the harness at it.
func (h *harness) withConfigText(text string) {
	path := filepath.Join(h.dir, "config.json")
	Expect(os.WriteFile(path, []byte(text), 0o600)).To(Succeed())
	h.deps.Settings.ConfigFile = path
}

// validConfig is a config file with every agent role.
const validConfig = `{"roles":["implementer","reviewer"],"agents":{` +
	`"planner":{"model":"prov/model","context_cap":200000},"researcher":{"model":"prov/model","context_cap":200000},` +
	`"implementer":{"model":"prov/model","context_cap":200000},"reviewer":{"model":"prov/model","context_cap":200000},` +
	`"curator":{"model":"prov/model","context_cap":200000}}}`

// writeToken leaves a token for the factory half of role.
func (h *harness) writeToken(role workflow.Role, token string) {
	Expect(os.WriteFile(h.deps.Settings.TokenPath(role), []byte(token), 0o640)).To(Succeed())
}

// queue registers the issues listing of o/r: each label maps to the issues the fake returns for it.
func (h *harness) queue(byLabel map[string][]map[string]any) {
	h.queueRepo("o/r", byLabel)
}

// queueRepo registers the issues listing of one repository.
func (h *harness) queueRepo(repo string, byLabel map[string][]map[string]any) {
	h.gh.Handle("GET /repos/"+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
		list := byLabel[r.URL.Query().Get("labels")]
		if list == nil {
			list = []map[string]any{}
		}
		fakegh.WriteJSON(w, list)
	})
}

// acceptWrites lets label and comment writes on issue n succeed.
func (h *harness) acceptWrites(n string) {
	h.gh.Status("DELETE /repos/o/r/issues/"+n+"/labels/{label}", http.StatusOK)
	h.gh.Status("POST /repos/o/r/issues/"+n+"/labels", http.StatusOK)
	h.gh.Status("POST /repos/o/r/issues/"+n+"/comments", http.StatusCreated)
}

// outcome returns the labels added and comments posted on issue 5, in order.
func (h *harness) outcome() (labels, comments []string) {
	return h.gh.Bodies("POST", "/repos/o/r/issues/5/labels"), h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")
}

// factoryHalf is what the fake factory half saw: the token file's content and mode.
type factoryHalf struct {
	token string
	mode  os.FileMode
}

// sudoExit makes the factory half exit with code, recording the token file it found in place.
func (h *harness) sudoExit(code int) *factoryHalf {
	seen := &factoryHalf{}
	h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
		if c.Argv[0] != "sudo" {
			return base(c)
		}
		path := h.deps.Settings.TokenPath(workflow.Role(c.Argv[6]))
		raw, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred(), "token file not in place")
		seen.token = string(raw)
		st, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		seen.mode = st.Mode().Perm()
		if code != 0 {
			return "", &proc.ExitError{Program: "sudo", Code: code}
		}
		return "", nil
	})
	return seen
}

// implementerGH registers the GitHub side of an Implementer run on issue 5; pr is the existing PR or nil.
func (h *harness) implementerGH(pr map[string]any, reviews []workflow.Item) {
	h.gh.JSON("GET /repos/o/r/issues/5", map[string]any{"number": 5, "title": "Add x", "body": "Do x"})
	if pr == nil {
		h.gh.JSON("GET /repos/o/r/pulls", []map[string]any{})
	} else {
		h.gh.JSON("GET /repos/o/r/pulls", []map[string]any{pr})
		h.gh.JSON("GET /repos/o/r/pulls/8/reviews", reviews)
	}
	h.gh.JSON("GET /repos/o/r", map[string]any{"default_branch": "main"})
	h.gh.Handle("POST /repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fakegh.WriteJSON(w, map[string]any{"number": 8, "html_url": "http://github/pr/8"})
	})
	h.gh.Status("POST /repos/o/r/issues/5/comments", http.StatusCreated)
	h.gh.JSON("GET /repos/o/r/issues", []map[string]any{{"number": 41}})
	h.gh.JSON("GET /repos/o/r/issues/41/comments", []workflow.Item{})
	h.gh.Status("POST /repos/o/r/issues/41/comments", http.StatusCreated)
}

// reviewerGH registers the GitHub side of a Reviewer run on issue 5, with the given earlier reviews,
// and leaves the Reviewer's token.
func (h *harness) reviewerGH(reviews []workflow.Item) {
	h.gh.JSON("GET /repos/o/r/issues/5", map[string]any{"number": 5, "title": "Add x", "body": "Do x"})
	h.gh.JSON("GET /repos/o/r/pulls", []map[string]any{{"number": 8, "html_url": "http://github/pr/8"}})
	h.gh.JSON("GET /repos/o/r", map[string]any{"default_branch": "main"})
	h.gh.JSON("GET /repos/o/r/pulls/8/reviews", reviews)
	h.gh.Status("POST /repos/o/r/pulls/8/reviews", http.StatusOK)
	h.writeToken(workflow.Reviewer, "repo-token")
}

// reviewOut is where a Reviewer run on issue 5 writes its verdict.
func (h *harness) reviewOut() string {
	return filepath.Join(h.deps.Settings.WorkRoot, "review-5.json")
}

// cleanReview is a review with no findings.
const cleanReview = `{"summary":"fine","findings":[]}`

// blockingReview is a review with one high finding.
const blockingReview = `{"summary":"bad","findings":[{"severity":"high","title":"bug","detail":"d","location":"a.go:1"}]}`

// opencodeWrites makes opencode runs write text to the file at path, for the runs whose argv contains marker.
func (h *harness) opencodeWrites(marker, path, text string) {
	h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
		if c.Argv[0] == "opencode" && strings.Contains(joined(c), marker) {
			Expect(os.WriteFile(path, []byte(text), 0o600)).To(Succeed())
			return okStream("ses_1"), nil
		}
		return base(c)
	})
}

// okStream is what `opencode run --format json` prints for a run that finished: one step.
func okStream(session string) string {
	return fmt.Sprintf(`{"type":"step_start","sessionID":%q}`+"\n"+
		`{"type":"step_finish","sessionID":%q,"part":{"cost":0.002,"tokens":{"input":1000,"output":50,"cache":{"read":400}}}}`+"\n",
		session, session)
}

// errStream is a run that ended with an API error of the given status.
func errStream(name string, status int) string {
	return fmt.Sprintf(`{"type":"error","sessionID":"ses_1","error":{"name":%q,"data":{"statusCode":%d,"message":"boom"}}}`+"\n", name, status)
}

// opencodeCalls are the recorded opencode processes.
func (h *harness) opencodeCalls() []proc.Cmd { return h.runner.find("opencode run") }

// answerOpencode makes the n-th (0-based) opencode process print outs[n]; later ones print the last.
func (h *harness) answerOpencode(outs ...string) {
	n := 0
	h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
		if c.Argv[0] != "opencode" {
			return base(c)
		}
		out := outs[min(n, len(outs)-1)]
		n++
		return out, nil
	})
}

// runImpl runs the Implementer on issue 5 against a fake GitHub.
func (h *harness) runImpl(agent config.AgentConfig) (int, error) {
	h.implementerGH(nil, nil)
	h.writeToken(workflow.Implementer, "repo-token\n")
	return Run(context.Background(), h.deps, workflow.Implementer, "o/r", agent, 5)
}

// usageLines are the JSON lines on stdout that are usage reports.
func (h *harness) usageLines() []map[string]any {
	var out []map[string]any
	for line := range strings.SplitSeq(h.out.String(), "\n") {
		var m map[string]any
		if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &m) == nil && m["event"] == "usage" {
			out = append(out, m)
		}
	}
	return out
}

// beNear matches a per-attempt timeout: the deadline less a moment of elapsed time.
func beNear(deadline time.Duration) OmegaMatcher {
	return And(BeNumerically("<=", deadline), BeNumerically(">", deadline-time.Minute))
}
