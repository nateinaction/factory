package foreman

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/ghtest"
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

// logEntry is one captured diagnostic record.
type logEntry struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// logCapture is a slog.Handler that keeps records for assertions instead of formatting them.
type logCapture struct {
	mu      sync.Mutex
	entries []logEntry
}

// Enabled accepts every level, so a spec sees whatever foreman emits.
func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

// Handle records r and its attributes.
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	e := logEntry{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		e.attrs[a.Key] = a.Value.String()

		return true
	})

	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = append(c.entries, e)

	return nil
}

// WithAttrs returns the same handler: the harness never logs through a derived logger.
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }

// WithGroup returns the same handler: the harness never logs through a derived logger.
func (c *logCapture) WithGroup(string) slog.Handler { return c }

// String renders the captured records as "LEVEL message key=value ..." lines, for matching.
func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	var b strings.Builder

	for _, e := range c.entries {
		fmt.Fprintf(&b, "%s %s", e.level, e.msg)

		for _, key := range slices.Sorted(maps.Keys(e.attrs)) {
			fmt.Fprintf(&b, " %s=%s", key, e.attrs[key])
		}

		b.WriteString("\n")
	}

	return b.String()
}

// harness wires Deps to a recording proxy in front of the emulator, a fake runner and a temp directory.
type harness struct {
	gh     *ghtest.Proxy
	runner *fakeRunner
	deps   Deps
	out    *bytes.Buffer
	logs   *logCapture
	dir    string
	// installed maps an agent role to the repositories its App is installed on.
	installed map[workflow.Role][]string
	sleeps    []time.Duration
	// pr and prURL are the open pull request the setup created or the run opened.
	pr    int
	prURL string
}

// testAgent() is the agent role settings tests run with.
func testAgent() config.AgentConfig {
	return config.AgentConfig{
		Model: "prov/model", ContextCap: 200_000, Compaction: true, Steps: 200, DeadlineMinutes: 90,
	}
}

// testRetro is the retro budget tests run with.
func testRetro() config.Budget { return config.Budget{Steps: 20, DeadlineMinutes: 15} }

// testConfig is a config file's worth of agent roles.
func testConfig() config.Config {
	agents := map[string]config.AgentConfig{}
	for _, r := range workflow.KnownRoles() {
		agents[string(r)] = testAgent()
	}

	return config.Config{Roles: workflow.KnownRoles(), Agents: agents, Retro: testRetro()}
}

// stepsStream is a run that finished after n steps, each reporting no error.
func stepsStream(n int) string {
	var b strings.Builder

	for range n {
		b.WriteString(`{"type":"step_start","sessionID":"ses_1"}` + "\n")
		b.WriteString(`{"type":"step_finish","sessionID":"ses_1","part":{"tokens":{"input":1,"output":1}}}` + "\n")
	}

	return b.String()
}

// newHarness builds a harness whose runner answers the helpers every flow needs. Each spec gets a
// fresh repository on the shared emulator; setup calls bypass the proxy, so it records only foreman's
// calls, and any request the proxy did not expect fails the spec.
func newHarness() *harness {
	dir := GinkgoT().TempDir()
	ctx := context.Background()

	Expect(emulator).NotTo(BeNil(), "the suite must start the GitHub emulator first")
	Expect(emulator.ResetRepo(ctx, "o/r")).To(Succeed())

	gh := ghtest.NewProxy(emulator.URL())

	DeferCleanup(func() {
		Expect(gh.Unexpected()).To(BeEmpty(), "requests nothing answered")
		gh.Close()
	})
	DeferCleanup(func() {
		Expect(emulator.DeleteRepo(ctx, "o/r")).To(Succeed())
	})

	key := filepath.Join(dir, "openrouter-key")
	Expect(os.WriteFile(key, []byte("or-key\n"), 0o600)).To(Succeed())

	h := &harness{
		gh: gh, runner: &fakeRunner{}, out: &bytes.Buffer{}, logs: &logCapture{}, dir: dir,
		installed: map[workflow.Role][]string{workflow.Implementer: {"o/r"}, workflow.Reviewer: {"o/r"}},
	}
	h.runner.fn = h.defaultRun
	h.deps = Deps{
		Settings: config.Settings{
			API: gh.URL(), TokenDir: dir, KeyFile: key,
			WorkRoot: filepath.Join(dir, "work"), Self: "/usr/local/bin/foreman",
			TokenHelper: "/usr/local/bin/github-app-token", Dagu: "/usr/local/bin/dagu", DAGPrefix: "factory-",
		},
		Runner: h.runner, HTTP: gh.Client(), Out: h.out, Log: slog.New(h.logs),
		Sleep: func(_ context.Context, d time.Duration) error { h.sleeps = append(h.sleeps, d); return nil },
	}

	return h
}

// defaultRun answers token minting and git queries, and a clone makes its directory, as git does;
// everything else succeeds silently.
func (h *harness) defaultRun(c proc.Cmd) (string, error) {
	line := joined(c)
	switch {
	case len(c.Argv) > 3 && c.Argv[0] == "git" && c.Argv[1] == "clone":
		Expect(os.MkdirAll(c.Argv[3], 0o700)).To(Succeed())
		return "", nil
	case c.Argv[0] == "opencode":
		return okStream("ses_1"), nil
	case strings.Contains(line, "github-app-token"):
		return h.tokenHelper(c)
	case strings.Contains(line, "rev-parse HEAD"):
		return "sha-head\n", nil
	case strings.Contains(line, "rev-list --count"):
		return "1\n", nil
	}

	return "", nil
}

// tokenHelper answers github-app-token: an App lists and mints only where it is installed.
func (h *harness) tokenHelper(c proc.Cmd) (string, error) {
	installed := h.installed[workflow.Role(appOf(c))]

	switch {
	case strings.Contains(joined(c), " repositories "):
		return strings.Join(installed, "\n") + "\n", nil
	case !slices.Contains(installed, repoOf(c)):
		return "", &proc.ExitError{Program: "github-app-token", Code: 1}
	}

	return "minted-token-" + appOf(c) + "\n", nil
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
	`"planner":{"model":"prov/model","context_cap":200000,"steps":40,"deadline_minutes":20},` +
	`"researcher":{"model":"prov/model","context_cap":200000,"steps":80,"deadline_minutes":45},` +
	`"implementer":{"model":"prov/model","context_cap":200000,"steps":200,"deadline_minutes":90},` +
	`"reviewer":{"model":"prov/model","context_cap":200000,"steps":60,"deadline_minutes":30},` +
	`"curator":{"model":"prov/model","context_cap":200000,"steps":60,"deadline_minutes":30}},` +
	`"retro":{"steps":20,"deadline_minutes":15}}`

// writeToken leaves a token for the factory half of role.
func (h *harness) writeToken(role workflow.Role, token string) {
	Expect(os.WriteFile(h.deps.Settings.TokenPath(role), []byte(token), 0o640)).To(Succeed())
}

// queue creates the issues a poll of o/r should find: each label maps to the issues carrying it.
func (h *harness) queue(byLabel map[string][]map[string]any) {
	h.queueRepo("o/r", byLabel)
}

// queueRepo creates the issues a poll of one repository should find.
func (h *harness) queueRepo(repo string, byLabel map[string][]map[string]any) {
	GinkgoHelper()

	Expect(emulator.EnsureRepo(context.Background(), repo)).To(Succeed())

	labels := map[int][]string{}
	highest := 0

	for label, issues := range byLabel {
		for _, fields := range issues {
			number, ok := fields["number"].(int)
			if !ok {
				Fail(fmt.Sprintf("queue entry for %s has no issue number", label))
			}

			labels[number] = append(labels[number], label)
			highest = maxInt(highest, number)
		}
	}

	for number := 1; number <= highest; number++ {
		_, err := emulator.CreateIssue(context.Background(), repo, labels[number], "Add x", "Do x")
		Expect(err).NotTo(HaveOccurred())
	}
}

// acceptWrites makes sure issue n exists. Label and comment writes then succeed against the emulator.
func (h *harness) acceptWrites(n string) {
	GinkgoHelper()

	number, err := strconv.Atoi(n)
	Expect(err).NotTo(HaveOccurred(), "issue number %q", n)

	h.ensureIssue("o/r", number)
}

// ensureIssue creates the filler issues below number and then number itself.
func (h *harness) ensureIssue(repo string, number int) {
	GinkgoHelper()

	Expect(emulator.EnsureRepo(context.Background(), repo)).To(Succeed())

	for attempt := 0; attempt <= number; attempt++ {
		exists, err := emulator.HasIssue(context.Background(), repo, number)
		Expect(err).NotTo(HaveOccurred())

		if exists {
			return
		}

		_, err = emulator.CreateIssue(context.Background(), repo, nil, "Add x", "Do x")
		Expect(err).NotTo(HaveOccurred())
	}

	Fail(fmt.Sprintf("could not create issue %s#%d", repo, number))
}

// maxInt is the larger of a and b.
func maxInt(a, b int) int {
	if a > b {
		return a
	}

	return b
}

// outcome returns the labels added and comments posted on issue 5, in order.
func (h *harness) outcome() (labels, comments []string) {
	return h.gh.Bodies("POST", "/repos/o/r/issues/5/labels"), h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")
}

// pullRequest is the open pull request's number and advertised web URL.
func (h *harness) pullRequest() (int, string) {
	GinkgoHelper()

	if h.pr != 0 {
		return h.pr, h.prURL
	}

	prs, err := emulator.PullRequests(context.Background(), "o/r")
	Expect(err).NotTo(HaveOccurred())
	Expect(prs).To(HaveLen(1))

	h.pr, h.prURL = prs[0].Number, prs[0].HTMLURL

	return h.pr, h.prURL
}

// reviewPath is the pull request's reviews endpoint.
func (h *harness) reviewPath() string {
	GinkgoHelper()

	pr, _ := h.pullRequest()

	return fmt.Sprintf("/repos/o/r/pulls/%d/reviews", pr)
}

// retroNumber is the open retro ticket's number, or 0.
func (h *harness) retroNumber() int {
	GinkgoHelper()

	number, err := emulator.FindIssueByLabel(context.Background(), "o/r", workflow.LabelRetro)
	Expect(err).NotTo(HaveOccurred())

	return number
}

// retroPath is the retro ticket's comments endpoint.
func (h *harness) retroPath() string {
	GinkgoHelper()

	number := h.retroNumber()
	Expect(number).NotTo(BeZero(), "no retro ticket")

	return fmt.Sprintf("/repos/o/r/issues/%d/comments", number)
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

// implementerGH creates the GitHub side of an Implementer run on issue 5; pr is the existing PR or nil.
func (h *harness) implementerGH(pr map[string]any, reviews []workflow.Item) {
	GinkgoHelper()

	h.ensureIssue("o/r", 5)

	h.pr, h.prURL = 0, ""

	if pr == nil {
		return
	}

	created, err := emulator.CreatePR(context.Background(), "o/r", workflow.BranchName(5), "Add x", "Do x")
	Expect(err).NotTo(HaveOccurred())

	h.pr, h.prURL = created.Number, created.HTMLURL

	for _, review := range reviews {
		Expect(emulator.CreateReview(context.Background(), "o/r", h.pr, review.Body)).To(Succeed())
	}
}

// reviewerGH creates the GitHub side of a Reviewer run on issue 5, with the given earlier reviews,
// and leaves the Reviewer's token.
func (h *harness) reviewerGH(reviews []workflow.Item) {
	GinkgoHelper()

	h.implementerGH(map[string]any{}, reviews)
	h.writeToken(workflow.Reviewer, "repo-token")
}

// reviewOut is where a Reviewer run on issue 5 writes its verdict.
func (h *harness) reviewOut() string {
	return filepath.Join(h.deps.Settings.WorkRoot, "review-5.json")
}

// cleanReview is a review with no findings.
const cleanReview = `{"summary":"fine","findings":[]}`

// blockingReview is a review with one high finding.
const blockingReview = `{"summary":"bad","findings":[` +
	`{"severity":"high","title":"bug","detail":"d","location":"a.go:1"}]}`

// opencodeWrites makes opencode runs write text to the file at path, for the runs whose argv contains
// marker.
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
		`{"type":"step_finish","sessionID":%q,"part":{"cost":0.002,`+
		`"tokens":{"input":1000,"output":50,"cache":{"read":400}}}}`+"\n",
		session, session)
}

// errStream is a run that ended with an API error of the given status.
func errStream(name string, status int) string {
	return fmt.Sprintf(`{"type":"error","sessionID":"ses_1",`+
		`"error":{"name":%q,"data":{"statusCode":%d,"message":"boom"}}}`+"\n", name, status)
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

// runImpl runs the Implementer on issue 5 against the emulator.
func (h *harness) runImpl(agent config.AgentConfig) (int, error) {
	h.implementerGH(nil, nil)
	h.writeToken(workflow.Implementer, "repo-token\n")

	return Run(context.Background(), h.deps, workflow.Implementer, "o/r", agent, testRetro(), 5)
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
