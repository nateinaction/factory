package foreman

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
func (h *harness) opencodeCalls() []Cmd { return h.runner.find("opencode run") }

// answerOpencode makes the n-th (0-based) opencode process print outs[n]; later ones print the last.
func (h *harness) answerOpencode(outs ...string) {
	base := h.runner.fn
	n := 0
	h.runner.fn = func(c Cmd) (string, error) {
		if c.Argv[0] != "opencode" {
			return base(c)
		}
		out := outs[min(n, len(outs)-1)]
		n++
		return out, nil
	}
}

// runImpl runs the Implementer on issue 5 against a fake GitHub.
func (h *harness) runImpl(agent AgentConfig) (int, error) {
	h.implementerGH(nil, nil)
	if err := os.WriteFile(h.deps.Settings.TokenPath(Implementer), []byte("repo-token\n"), 0o640); err != nil {
		h.t.Fatal(err)
	}
	return Run(context.Background(), h.deps, Implementer, "o/r", agent, 5)
}

// TestRunUsesTheNamedAgentAndDisablesRepositoryConfig pins: each run is `opencode run --agent <role>` with repository config and external skills off.
func TestRunUsesTheNamedAgentAndDisablesRepositoryConfig(t *testing.T) {
	h := newHarness(t)
	if _, err := h.runImpl(testAgent); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.opencodeCalls() {
		line := joined(c)
		if !strings.Contains(line, "opencode run --agent implementer --format json") || strings.Contains(line, "--model") {
			t.Errorf("argv %q", line)
		}
		if c.Env["OPENCODE_DISABLE_PROJECT_CONFIG"] != "1" || c.Env["OPENCODE_DISABLE_EXTERNAL_SKILLS"] != "1" {
			t.Errorf("env %v", c.Env)
		}
	}
	if len(h.opencodeCalls()) != 2 {
		t.Errorf("want the run and its retro: %v", h.runner.cmds)
	}
}

// TestRunStateIsFreshAndDeletedAndNothingIsContinued pins: a fresh state directory per run, deleted afterwards, and no --continue.
func TestRunStateIsFreshAndDeletedAndNothingIsContinued(t *testing.T) {
	h := newHarness(t)
	stale := filepath.Join(h.deps.Settings.WorkRoot, "state-implementer-5")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "opencode.db"), []byte("old session"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen []string
	base := h.runner.fn
	h.runner.fn = func(c Cmd) (string, error) {
		if c.Argv[0] == "opencode" {
			db := c.Env["OPENCODE_DB"]
			if len(seen) == 0 {
				if _, err := os.Stat(db); err == nil {
					t.Errorf("state not fresh: %s exists", db)
				}
			}
			seen = append(seen, db)
			if err := os.WriteFile(db, []byte("session"), 0o600); err != nil {
				t.Error(err)
			}
		}
		return base(c)
	}
	if _, err := h.runImpl(testAgent); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != seen[1] || !strings.HasPrefix(seen[0], h.deps.Settings.WorkRoot) {
		t.Fatalf("run and retro share one state under the work root: %v", seen)
	}
	if _, err := os.Stat(filepath.Dir(seen[0])); !os.IsNotExist(err) {
		t.Errorf("state directory survived the run: %v", err)
	}
	if len(h.runner.find("--continue")) != 0 {
		t.Error("a session was continued by recency")
	}
}

// TestRetroResumesBySessionID pins: the retro continues the run's session by explicit id.
func TestRetroResumesBySessionID(t *testing.T) {
	h := newHarness(t)
	h.answerOpencode(okStream("ses_run"))
	if _, err := h.runImpl(testAgent); err != nil {
		t.Fatal(err)
	}
	calls := h.opencodeCalls()
	if len(calls) != 2 || !strings.Contains(joined(calls[1]), "--session ses_run") || strings.Contains(joined(calls[0]), "--session") {
		t.Errorf("calls %v", h.runner.cmds)
	}
}

// TestReviewerStateIsInMemory pins: the Reviewer's state never touches the disk.
func TestReviewerStateIsInMemory(t *testing.T) {
	h := newHarness(t)
	h.reviewerGH(nil)
	h.opencodeWrites("code-review", filepath.Join(h.deps.Settings.WorkRoot, "review-5.json"), cleanReview)
	if _, err := Run(context.Background(), h.deps, Reviewer, "o/r", testAgent, 5); err != nil {
		t.Fatal(err)
	}
	if oc := h.opencodeCalls(); len(oc) != 1 || oc[0].Env["OPENCODE_DB"] != ":memory:" {
		t.Errorf("calls %v", h.runner.cmds)
	}
}

// TestCompactionIsSetPerAgentRole pins: compaction on or off reaches opencode as inline config.
func TestCompactionIsSetPerAgentRole(t *testing.T) {
	for _, on := range []bool{true, false} {
		h := newHarness(t)
		agent := testAgent
		agent.Compaction = on
		if _, err := h.runImpl(agent); err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Compaction struct {
				Auto bool `json:"auto"`
			} `json:"compaction"`
		}
		if err := json.Unmarshal([]byte(h.opencodeCalls()[0].Env["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil || cfg.Compaction.Auto != on {
			t.Errorf("compaction %v: %v %+v", on, err, cfg)
		}
	}
}

// TestOverflowFailsTheRunWithoutRetry pins: a context overflow fails at once.
func TestOverflowFailsTheRunWithoutRetry(t *testing.T) {
	h := newHarness(t)
	h.answerOpencode(errStream("ContextOverflowError", 0))
	agent := testAgent
	agent.Compaction = false
	_, err := h.runImpl(agent)
	if err == nil || !strings.Contains(err.Error(), "context overflow") || len(h.opencodeCalls()) != 1 {
		t.Fatalf("%v %v", err, h.runner.cmds)
	}
}

// TestModelFailuresFollowTheRetryRules pins: rate limit, 5xx and timeout retry up to twice; 402, 404 and anything else do not.
func TestModelFailuresFollowTheRetryRules(t *testing.T) {
	cases := []struct {
		name  string
		out   string
		calls int
		kind  string
	}{
		{"rate limit", errStream("APIError", 429), 3, "rate limit"},
		{"server error", errStream("APIError", 503), 3, "server error"},
		{"timeout", errStream("TimeoutError", 0), 3, "timeout"},
		{"out of credits", errStream("APIError", 402), 1, "out of credits"},
		{"unknown model", errStream("APIError", 404), 1, "unknown model"},
		{"anything else", errStream("APIError", 400), 1, "model error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.answerOpencode(tc.out)
			_, err := h.runImpl(testAgent)
			if err == nil || !strings.Contains(err.Error(), tc.kind) || len(h.opencodeCalls()) != tc.calls {
				t.Fatalf("err %v, calls %d", err, len(h.opencodeCalls()))
			}
			if strings.Contains(err.Error(), "boom") || strings.Contains(err.Error(), "or-key") {
				t.Errorf("failure text leaks: %v", err)
			}
		})
	}
}

// TestARetryThatSucceedsIsNotAFailure pins: two rate limits then success finishes the run, after backing off twice.
func TestARetryThatSucceedsIsNotAFailure(t *testing.T) {
	h := newHarness(t)
	h.answerOpencode(errStream("APIError", 429), errStream("APIError", 429), okStream("ses_1"))
	if code, err := h.runImpl(testAgent); err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	if len(h.sleeps) != 2 {
		t.Errorf("backoffs %v", h.sleeps)
	}
}

// TestFailureOutputNeverCarriesTheKey pins: what is echoed to the log has the key removed.
func TestFailureOutputNeverCarriesTheKey(t *testing.T) {
	h := newHarness(t)
	h.answerOpencode(`{"type":"error","error":{"name":"APIError","data":{"statusCode":400,"message":"bad key or-key"}}}` + "\n")
	if _, err := h.runImpl(testAgent); err == nil {
		t.Fatal("want failure")
	}
	if strings.Contains(h.errb.String(), "or-key") || strings.Contains(h.out.String(), "or-key") {
		t.Errorf("key leaked: %q %q", h.errb.String(), h.out.String())
	}
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

// TestEachRunLogsOneUsageLine pins: one JSON usage line, summed over the run and its retro, with the stated keys.
func TestEachRunLogsOneUsageLine(t *testing.T) {
	h := newHarness(t)
	h.deps.Host = "pi-1"
	if _, err := h.runImpl(testAgent); err != nil {
		t.Fatal(err)
	}
	lines := h.usageLines()
	if len(lines) != 1 {
		t.Fatalf("usage lines: %q", h.out.String())
	}
	want := map[string]any{
		"repository": "o/r", "issue": 5.0, "agent_role": "implementer", "model": "prov/model", "worker_host": "pi-1",
		"input_tokens": 2000.0, "output_tokens": 100.0, "cached_tokens": 800.0, "context_cap": 200000.0,
		"peak_context": 1400.0, "cost": 0.004,
	}
	for k, v := range want {
		if lines[0][k] != v {
			t.Errorf("%s = %v, want %v", k, lines[0][k], v)
		}
	}
	if m, _ := lines[0]["missing"].([]any); len(m) != 1 || m[0] != "domains" {
		t.Errorf("missing %v: domains are not reported yet", lines[0]["missing"])
	}
}

// TestAFieldTheAdapterCannotReportIsMissingNotZero pins: no step event means no token keys, not zeros.
func TestAFieldTheAdapterCannotReportIsMissingNotZero(t *testing.T) {
	h := newHarness(t)
	h.answerOpencode(`{"type":"text","sessionID":"ses_1"}` + "\n")
	if _, err := h.runImpl(testAgent); err != nil {
		t.Fatal(err)
	}
	lines := h.usageLines()
	if len(lines) != 1 {
		t.Fatalf("usage lines: %q", h.out.String())
	}
	for _, k := range []string{"input_tokens", "output_tokens", "cached_tokens", "peak_context", "cost", "worker_host"} {
		if _, ok := lines[0][k]; ok {
			t.Errorf("%s reported as %v", k, lines[0][k])
		}
	}
	m, _ := lines[0]["missing"].([]any)
	if len(m) != 7 {
		t.Errorf("missing %v", m)
	}
}

// TestAFailedRunStillLogsItsUsage pins: money spent on a failed run is reported.
func TestAFailedRunStillLogsItsUsage(t *testing.T) {
	h := newHarness(t)
	h.answerOpencode(okStream("ses_1") + errStream("APIError", 400))
	if _, err := h.runImpl(testAgent); err == nil {
		t.Fatal("want failure")
	}
	if lines := h.usageLines(); len(lines) != 1 || lines[0]["input_tokens"] != 1000.0 {
		t.Errorf("usage %v", lines)
	}
}

// cloneLeavesDocs makes a clone leave an AGENTS.md behind, like a real repository.
func (h *harness) cloneLeavesDocs() {
	base := h.runner.fn
	h.runner.fn = func(c Cmd) (string, error) {
		if len(c.Argv) > 3 && c.Argv[0] == "git" && c.Argv[1] == "clone" {
			dir := c.Argv[3]
			if err := os.MkdirAll(dir, 0o700); err != nil {
				h.t.Error(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("AGENTS-MD-CONTENT"), 0o600); err != nil {
				h.t.Error(err)
			}
		}
		return base(c)
	}
}

// TestPromptPrefixIsByteIdenticalAcrossRuns pins: nothing variable sits before the issue material, and repository docs come first.
func TestPromptPrefixIsByteIdenticalAcrossRuns(t *testing.T) {
	var prompts []string
	for _, root := range []string{"a", "b"} {
		h := newHarness(t)
		h.deps.Settings.WorkRoot = filepath.Join(h.dir, root, "work")
		h.cloneLeavesDocs()
		if _, err := h.runImpl(testAgent); err != nil {
			t.Fatal(err)
		}
		argv := h.opencodeCalls()[0].Argv
		prompts = append(prompts, argv[len(argv)-1])
	}
	prefix := func(p string) string { before, _, _ := strings.Cut(p, IssueMaterialHeading); return before }
	if prefix(prompts[0]) != prefix(prompts[1]) || prefix(prompts[0]) == prompts[0] {
		t.Fatalf("prefixes differ or no material: %q / %q", prompts[0], prompts[1])
	}
	p := prefix(prompts[0])
	if !strings.Contains(p, "AGENTS-MD-CONTENT") || strings.Index(p, "implement") > strings.Index(p, "AGENTS-MD-CONTENT") {
		t.Errorf("role prompt must precede repository docs: %q", p)
	}
	for _, varying := range []string{"issue-5", "#5", "work"} {
		if strings.Contains(p, varying) {
			t.Errorf("prefix contains %q", varying)
		}
	}
}

// TestReadRepoDocsIsFixedOrderAndBounded pins: docs are read in a fixed order, missing ones skipped, and capped.
func TestReadRepoDocsIsFixedOrderAndBounded(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("docs/agents/b.md", "B")
	write("docs/agents/a.md", "A")
	write("GLOSSARY.md", strings.Repeat("g", DocsLimitChars+10))
	write("AGENTS.md", "root")
	docs, err := ReadRepoDocs(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	total := 0
	for _, d := range docs {
		names = append(names, d.Path)
		total += len(d.Text)
	}
	if strings.Join(names, ",") != "AGENTS.md,GLOSSARY.md,docs/agents/a.md,docs/agents/b.md" || total > DocsLimitChars {
		t.Errorf("%v total %d", names, total)
	}
}

// TestLoadConfigRequiresEveryAgentRole pins: each of the five agent roles has a model and a cap.
func TestLoadConfigRequiresEveryAgentRole(t *testing.T) {
	good := func() map[string]AgentConfig {
		m := map[string]AgentConfig{}
		for _, r := range knownRoles {
			m[string(r)] = testAgent
		}
		return m
	}
	load := func(agents map[string]AgentConfig) error {
		raw, err := json.Marshal(map[string]any{"roles": []Role{Implementer}, "agents": agents})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "c.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = LoadConfig(path)
		return err
	}
	if err := load(good()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string]AgentConfig){
		"missing role": func(m map[string]AgentConfig) { delete(m, "curator") },
		"no model":     func(m map[string]AgentConfig) { m["planner"] = AgentConfig{ContextCap: 1} },
		"no cap":       func(m map[string]AgentConfig) { m["planner"] = AgentConfig{Model: "m"} },
	} {
		m := good()
		mutate(m)
		if load(m) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
