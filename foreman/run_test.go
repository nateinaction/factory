package foreman

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// implementerGH registers the GitHub side of an Implementer run on issue 5; pr is the existing PR or nil.
func (h *harness) implementerGH(pr map[string]any, reviews []Item) {
	h.gh.json("GET /repos/o/r/issues/5", map[string]any{"number": 5, "title": "Add x", "body": "Do x"})
	if pr == nil {
		h.gh.json("GET /repos/o/r/pulls", []map[string]any{})
	} else {
		h.gh.json("GET /repos/o/r/pulls", []map[string]any{pr})
		h.gh.json("GET /repos/o/r/pulls/8/reviews", reviews)
	}
	h.gh.json("GET /repos/o/r", map[string]any{"default_branch": "main"})
	h.gh.handle("POST /repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"number": 8, "html_url": "http://github/pr/8"})
	})
	h.gh.status("POST /repos/o/r/issues/5/comments", http.StatusCreated)
	h.gh.json("GET /repos/o/r/issues", []map[string]any{{"number": 41}})
	h.gh.json("GET /repos/o/r/issues/41/comments", []Item{})
	h.gh.status("POST /repos/o/r/issues/41/comments", http.StatusCreated)
}

// opencodeWrites makes opencode runs write text to the file at path, for the runs whose argv contains marker.
func (h *harness) opencodeWrites(marker, path, text string) {
	base := h.runner.fn
	h.runner.fn = func(c Cmd) (string, error) {
		if c.Argv[0] == "opencode" && strings.Contains(joined(c), marker) {
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				h.t.Error(err)
			}
			return okStream("ses_1"), nil
		}
		return base(c)
	}
}

// TestRunImplementerFirstRunPushesAndOpensPR pins: run implementer first run pushes and opens p r.
func TestRunImplementerFirstRunPushesAndOpensPR(t *testing.T) {
	h := newHarness(t)
	h.implementerGH(nil, nil)
	if err := os.WriteFile(h.deps.Settings.TokenFile, []byte("repo-token\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	h.opencodeWrites("--session", filepath.Join(h.deps.Settings.WorkRoot, "issue-5.retro.md"), "lessons\n")
	code, err := Run(context.Background(), h.deps, Implementer, "o/r", testAgent, 5)
	if err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	work := filepath.Join(h.deps.Settings.WorkRoot, "issue-5")
	if len(h.runner.find("git clone https://github.com/o/r.git "+work)) != 1 {
		t.Errorf("clone: %v", h.runner.cmds)
	}
	if len(h.runner.find("checkout -B agent/issue-5")) != 1 || len(h.runner.find("fetch origin")) != 0 {
		t.Errorf("checkout/fetch: %v", h.runner.cmds)
	}
	oc := h.runner.find("opencode run --agent implementer --format json Use the `implement` skill")
	if len(oc) != 1 || oc[0].Dir != work || oc[0].Env["OPENROUTER_API_KEY"] != "or-key" || !near(oc[0].Timeout, AgentTimeout) {
		t.Fatalf("opencode %+v", oc)
	}
	if strings.Contains(joined(oc[0]), "Review:") {
		t.Error("first run carries review feedback")
	}
	push := h.runner.find("noreply.github.com push")
	if len(push) != 1 || !strings.HasSuffix(joined(push[0]), "push --force origin agent/issue-5") {
		t.Errorf("push %v", push)
	}
	if push[0].Env["GIT_CONFIG_VALUE_0"] == "" {
		t.Error("push without credentials in env")
	}
	pr := h.gh.bodies("POST", "/repos/o/r/pulls")
	if len(pr) != 1 || !strings.Contains(pr[0], `"title":"Add x"`) || !strings.Contains(pr[0], `"head":"agent/issue-5"`) ||
		!strings.Contains(pr[0], `"base":"main"`) || !strings.Contains(pr[0], `Closes #5\n\nOpened by the factory agent.`) {
		t.Errorf("pr %v", pr)
	}
	if c := h.gh.bodies("POST", "/repos/o/r/issues/5/comments"); len(c) != 1 || !strings.Contains(c[0], "Pull request: http://github/pr/8") {
		t.Errorf("comments %v", c)
	}
	if !strings.HasPrefix(h.out.String(), "http://github/pr/8\n") {
		t.Errorf("stdout %q", h.out.String())
	}
	retro := h.gh.bodies("POST", "/repos/o/r/issues/41/comments")
	if len(retro) != 1 || !strings.Contains(retro[0], `<!-- agent-retro:5:sha-head -->\n## Retro: #5\n\nlessons`) {
		t.Errorf("retro %v", retro)
	}
	if rc := h.runner.find("--session"); len(rc) != 1 || !near(rc[0].Timeout, RetroTimeout) {
		t.Errorf("retro run %v", rc)
	}
	h.leakFree("repo-token", "or-key")
}

// TestRunImplementerReworkBuildsOnReviewedCommits pins: run implementer rework builds on reviewed commits.
func TestRunImplementerReworkBuildsOnReviewedCommits(t *testing.T) {
	h := newHarness(t)
	h.implementerGH(map[string]any{"number": 8, "html_url": "http://github/pr/8"},
		[]Item{{Body: ReviewMarker("old") + "\nFIX THE THING"}})
	if err := os.WriteFile(h.deps.Settings.TokenFile, []byte("repo-token"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), h.deps, Implementer, "o/r", testAgent, 5); err != nil {
		t.Fatal(err)
	}
	if len(h.runner.find("fetch origin agent/issue-5")) != 1 || len(h.runner.find("checkout -B agent/issue-5 origin/agent/issue-5")) != 1 {
		t.Errorf("did not build on the branch: %v", h.runner.cmds)
	}
	if oc := h.runner.find("opencode run --agent implementer"); len(oc) < 1 || !strings.Contains(joined(oc[0]), "Review:\n"+ReviewMarker("old")+"\nFIX THE THING") {
		t.Errorf("feedback missing from prompt")
	}
	if push := h.runner.find("noreply.github.com push"); len(push) != 1 || strings.Contains(joined(push[0]), "--force") {
		t.Errorf("rework must fast-forward: %v", push)
	}
	if len(h.gh.bodies("POST", "/repos/o/r/pulls")) != 0 {
		t.Error("opened a second PR")
	}
	if c := h.gh.bodies("POST", "/repos/o/r/issues/5/comments"); len(c) != 0 {
		t.Errorf("commented a PR link again: %v", c)
	}
	if !strings.HasPrefix(h.out.String(), "http://github/pr/8\n") {
		t.Errorf("stdout %q", h.out.String())
	}
}

// TestRunImplementerFailsWithoutNewCommits pins: run implementer fails without new commits.
func TestRunImplementerFailsWithoutNewCommits(t *testing.T) {
	h := newHarness(t)
	h.implementerGH(nil, nil)
	if err := os.WriteFile(h.deps.Settings.TokenFile, []byte("t"), 0o640); err != nil {
		t.Fatal(err)
	}
	base := h.runner.fn
	h.runner.fn = func(c Cmd) (string, error) {
		if strings.Contains(joined(c), "rev-list --count") {
			return "0\n", nil
		}
		return base(c)
	}
	code, err := Run(context.Background(), h.deps, Implementer, "o/r", testAgent, 5)
	if err == nil || code == 0 || !strings.Contains(err.Error(), "no commits") {
		t.Fatalf("%d %v", code, err)
	}
	if len(h.runner.find("noreply.github.com push")) != 0 || len(h.gh.bodies("POST", "/repos/o/r/pulls")) != 0 {
		t.Error("published work the agent never made")
	}
}

// TestRunImplementerRetroFailureIsNotFatal pins: run implementer retro failure is not fatal.
func TestRunImplementerRetroFailureIsNotFatal(t *testing.T) {
	h := newHarness(t)
	h.implementerGH(nil, nil)
	if err := os.WriteFile(h.deps.Settings.TokenFile, []byte("t"), 0o640); err != nil {
		t.Fatal(err)
	}
	base := h.runner.fn
	h.runner.fn = func(c Cmd) (string, error) {
		if strings.Contains(joined(c), "--session") {
			return "", errors.New("retro blew up")
		}
		return base(c)
	}
	code, err := Run(context.Background(), h.deps, Implementer, "o/r", testAgent, 5)
	if err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	if !strings.Contains(h.errb.String(), "retro failed") || !strings.Contains(h.errb.String(), "retro blew up") {
		t.Errorf("stderr %q", h.errb.String())
	}
}

// TestRunImplementerEmptyRetroIsLoggedNotPosted pins: run implementer empty retro is logged not posted.
func TestRunImplementerEmptyRetroIsLoggedNotPosted(t *testing.T) {
	h := newHarness(t)
	h.implementerGH(nil, nil)
	if err := os.WriteFile(h.deps.Settings.TokenFile, []byte("t"), 0o640); err != nil {
		t.Fatal(err)
	}
	h.opencodeWrites("--session", filepath.Join(h.deps.Settings.WorkRoot, "issue-5.retro.md"), "  \n")
	if code, err := Run(context.Background(), h.deps, Implementer, "o/r", testAgent, 5); err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	if !strings.Contains(h.errb.String(), "the retro wrote nothing") || len(h.gh.bodies("POST", "/repos/o/r/issues/41/comments")) != 0 {
		t.Errorf("stderr %q", h.errb.String())
	}
}

// TestRunImplementerClearsStaleScratchFromEarlierRuns pins: run implementer clears stale scratch from earlier runs.
func TestRunImplementerClearsStaleScratchFromEarlierRuns(t *testing.T) {
	h := newHarness(t)
	h.implementerGH(nil, nil)
	if err := os.WriteFile(h.deps.Settings.TokenFile, []byte("t"), 0o640); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(h.deps.Settings.WorkRoot, "issue-5.retro.md")
	if err := os.MkdirAll(h.deps.Settings.WorkRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("stale retro"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), h.deps, Implementer, "o/r", testAgent, 5); err != nil {
		t.Fatal(err)
	}
	if len(h.gh.bodies("POST", "/repos/o/r/issues/41/comments")) != 0 {
		t.Error("posted a stale retro")
	}
}

// reviewerGH registers the GitHub side of a Reviewer run on issue 5, with the given earlier reviews.
func (h *harness) reviewerGH(reviews []Item) {
	h.gh.json("GET /repos/o/r/issues/5", map[string]any{"number": 5, "title": "Add x", "body": "Do x"})
	h.gh.json("GET /repos/o/r/pulls", []map[string]any{{"number": 8, "html_url": "http://github/pr/8"}})
	h.gh.json("GET /repos/o/r", map[string]any{"default_branch": "main"})
	h.gh.json("GET /repos/o/r/pulls/8/reviews", reviews)
	h.gh.status("POST /repos/o/r/pulls/8/reviews", http.StatusOK)
	if err := os.WriteFile(h.deps.Settings.TokenFile, []byte("repo-token"), 0o640); err != nil {
		h.t.Fatal(err)
	}
}

// cleanReview is a review with no findings.
const cleanReview = `{"summary":"fine","findings":[]}`

// blockingReview is a review with one high finding.
const blockingReview = `{"summary":"bad","findings":[{"severity":"high","title":"bug","detail":"d","location":"a.go:1"}]}`

// TestRunReviewerCleanApproves pins: run reviewer clean approves.
func TestRunReviewerCleanApproves(t *testing.T) {
	h := newHarness(t)
	h.reviewerGH(nil)
	h.opencodeWrites("code-review", filepath.Join(h.deps.Settings.WorkRoot, "review-5.json"), cleanReview)
	code, err := Run(context.Background(), h.deps, Reviewer, "o/r", testAgent, 5)
	if err != nil || code != ReviewClean {
		t.Fatalf("%d %v", code, err)
	}
	posts := h.gh.bodies("POST", "/repos/o/r/pulls/8/reviews")
	if len(posts) != 1 || !strings.Contains(posts[0], `"event":"APPROVE"`) || !strings.Contains(posts[0], `"commit_id":"sha-head"`) ||
		!strings.Contains(posts[0], "<!-- agent-review:sha-head -->") {
		t.Errorf("posts %v", posts)
	}
	work := filepath.Join(h.deps.Settings.WorkRoot, "review-5")
	if len(h.runner.find("git clone https://github.com/o/r.git "+work)) != 1 ||
		len(h.runner.find("fetch origin agent/issue-5")) != 1 ||
		len(h.runner.find("checkout -B agent/issue-5 origin/agent/issue-5")) != 1 {
		t.Errorf("checkout: %v", h.runner.cmds)
	}
	oc := h.runner.find("opencode run --agent reviewer --format json Use the `code-review` skill")
	if len(oc) != 1 || oc[0].Dir != work || !near(oc[0].Timeout, ReviewTimeout) || oc[0].Env["OPENROUTER_API_KEY"] != "or-key" {
		t.Errorf("opencode %+v", oc)
	}
	spec, err := os.ReadFile(filepath.Join(h.deps.Settings.WorkRoot, "review-5.spec.md"))
	if err != nil || string(spec) != "# Add x\n\nDo x\n" {
		t.Errorf("spec %q %v", spec, err)
	}
	h.leakFree("repo-token", "or-key")
}

// TestRunReviewerFindingsSendBack pins: run reviewer findings send back.
func TestRunReviewerFindingsSendBack(t *testing.T) {
	h := newHarness(t)
	h.reviewerGH(nil)
	h.opencodeWrites("code-review", filepath.Join(h.deps.Settings.WorkRoot, "review-5.json"), blockingReview)
	code, err := Run(context.Background(), h.deps, Reviewer, "o/r", testAgent, 5)
	if err != nil || code != ReviewFindings {
		t.Fatalf("%d %v", code, err)
	}
	posts := h.gh.bodies("POST", "/repos/o/r/pulls/8/reviews")
	if len(posts) != 1 || !strings.Contains(posts[0], `"event":"COMMENT"`) || !strings.Contains(posts[0], "### High (1)") {
		t.Errorf("posts %v", posts)
	}
}

// TestRunReviewerStopsAfterMaxRounds pins: run reviewer stops after max rounds.
func TestRunReviewerStopsAfterMaxRounds(t *testing.T) {
	h := newHarness(t)
	h.reviewerGH([]Item{{Body: ReviewMarker("s1")}, {Body: ReviewMarker("s2")}})
	h.opencodeWrites("code-review", filepath.Join(h.deps.Settings.WorkRoot, "review-5.json"), blockingReview)
	code, err := Run(context.Background(), h.deps, Reviewer, "o/r", testAgent, 5)
	if err != nil || code != ReviewExhausted {
		t.Fatalf("%d %v", code, err)
	}
}

// TestRunReviewerRepeatOfAReviewedCommitPostsNothing pins: run reviewer repeat of a reviewed commit posts nothing.
func TestRunReviewerRepeatOfAReviewedCommitPostsNothing(t *testing.T) {
	h := newHarness(t)
	h.reviewerGH([]Item{{Body: ReviewMarker("sha-head") + "\nearlier"}})
	h.opencodeWrites("code-review", filepath.Join(h.deps.Settings.WorkRoot, "review-5.json"), blockingReview)
	code, err := Run(context.Background(), h.deps, Reviewer, "o/r", testAgent, 5)
	if err != nil || code != ReviewFindings {
		t.Fatalf("%d %v", code, err)
	}
	if posts := h.gh.bodies("POST", "/repos/o/r/pulls/8/reviews"); len(posts) != 0 {
		t.Errorf("posted again: %v", posts)
	}
}

// TestRunReviewerMalformedReviewFailsClosed pins: run reviewer malformed review fails closed.
func TestRunReviewerMalformedReviewFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.reviewerGH(nil)
	h.opencodeWrites("code-review", filepath.Join(h.deps.Settings.WorkRoot, "review-5.json"), `{"summary":"ok"}`)
	code, err := Run(context.Background(), h.deps, Reviewer, "o/r", testAgent, 5)
	if err == nil || code == ReviewClean {
		t.Fatalf("%d %v", code, err)
	}
	if posts := h.gh.bodies("POST", "/repos/o/r/pulls/8/reviews"); len(posts) != 0 {
		t.Errorf("posted a review of a malformed result: %v", posts)
	}
}

// TestRunReviewerMissingReviewFileFailsClosed pins: run reviewer missing review file fails closed.
func TestRunReviewerMissingReviewFileFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.reviewerGH(nil)
	if code, err := Run(context.Background(), h.deps, Reviewer, "o/r", testAgent, 5); err == nil || code == ReviewClean {
		t.Fatalf("%d %v", code, err)
	}
}

// TestRunReviewerWithoutPRFails pins: run reviewer without p r fails.
func TestRunReviewerWithoutPRFails(t *testing.T) {
	h := newHarness(t)
	h.gh.json("GET /repos/o/r/issues/5", map[string]any{"number": 5})
	h.gh.json("GET /repos/o/r/pulls", []map[string]any{})
	if err := os.WriteFile(h.deps.Settings.TokenFile, []byte("t"), 0o640); err != nil {
		t.Fatal(err)
	}
	code, err := Run(context.Background(), h.deps, Reviewer, "o/r", testAgent, 5)
	if err == nil || code == 0 || !strings.Contains(err.Error(), "no open pull request for agent/issue-5") {
		t.Fatalf("%d %v", code, err)
	}
}

// TestRunRefusesRolesWithoutBehaviour pins: run refuses roles without behaviour.
func TestRunRefusesRolesWithoutBehaviour(t *testing.T) {
	for _, role := range []Role{Planner, Researcher, Curator} {
		h := newHarness(t)
		code, err := Run(context.Background(), h.deps, role, "o/r", testAgent, 5)
		if err == nil || code == 0 || len(h.runner.cmds) != 0 || len(h.gh.calls()) != 0 {
			t.Errorf("%s: %d %v", role, code, err)
		}
	}
}

// TestRunWithoutTokenFileFails pins: run without token file fails.
func TestRunWithoutTokenFileFails(t *testing.T) {
	h := newHarness(t)
	if code, err := Run(context.Background(), h.deps, Implementer, "o/r", testAgent, 5); err == nil || code == 0 {
		t.Errorf("%d %v", code, err)
	}
	if !slices.Equal(h.gh.summary(), nil) {
		t.Errorf("called GitHub without a token: %v", h.gh.summary())
	}
}

// near reports whether a per-attempt timeout is the deadline less a moment of elapsed time.
func near(got, deadline time.Duration) bool { return got <= deadline && got > deadline-time.Minute }
