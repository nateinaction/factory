package foreman

import (
	"strings"
	"testing"
)

// TestParseRoleKnownSet pins: parse role known set.
func TestParseRoleKnownSet(t *testing.T) {
	for _, name := range []string{"planner", "researcher", "implementer", "reviewer", "curator"} {
		got, err := ParseRole(name)
		if err != nil || string(got) != name {
			t.Errorf("ParseRole(%q) = %q, %v", name, got, err)
		}
	}
}

// TestParseRoleFailsClosed pins: parse role fails closed.
func TestParseRoleFailsClosed(t *testing.T) {
	for _, name := range []string{"", "Implementer", "worker", "implementer ", "../reviewer"} {
		if _, err := ParseRole(name); err == nil {
			t.Errorf("ParseRole(%q) accepted an unknown role", name)
		}
	}
}

// TestParseIssueNumber pins: parse issue number.
func TestParseIssueNumber(t *testing.T) {
	if n, err := ParseIssueNumber("123456789"); err != nil || n != 123456789 {
		t.Errorf("got %d, %v", n, err)
	}
	for _, bad := range []string{"", "12a", "-1", "1234567890", " 1", "1\n", "../1"} {
		if _, err := ParseIssueNumber(bad); err == nil {
			t.Errorf("ParseIssueNumber(%q) accepted", bad)
		}
	}
}

// TestValidateRepo pins: validate repo.
func TestValidateRepo(t *testing.T) {
	if err := ValidateRepo("nateinaction/factory.x_y-z"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"", "a", "a/b/c", "a/b c", "a/../b?x", "/b", "a/b\n", "../etc", "a/.."} {
		if err := ValidateRepo(bad); err == nil {
			t.Errorf("ValidateRepo(%q) accepted", bad)
		}
	}
}

// TestPickIssueSkipsPullRequests pins: pick issue skips pull requests.
func TestPickIssueSkipsPullRequests(t *testing.T) {
	issues := []Issue{{Number: 1, IsPullRequest: true}, {Number: 2}, {Number: 3}}
	got, ok := PickIssue(issues)
	if !ok || got.Number != 2 {
		t.Errorf("got %v, %v", got, ok)
	}
	if _, ok := PickIssue([]Issue{{Number: 1, IsPullRequest: true}}); ok {
		t.Error("picked a pull request")
	}
}

// TestBranchName pins: branch name.
func TestBranchName(t *testing.T) {
	if got := BranchName(7); got != "agent/issue-7" {
		t.Error(got)
	}
}

// TestImplementerPromptCarriesIssueAfterDocs pins: implementer prompt first run.
func TestImplementerPromptCarriesIssueAfterDocs(t *testing.T) {
	got := ImplementerPrompt([]Doc{{Path: "AGENTS.md", Text: "RULES"}}, "o/r", Issue{Number: 4, Title: "T", Body: "B"}, "")
	want := implementerRolePrompt + "\n\n# Repository docs\n\n## AGENTS.md\n\nRULES\n" + IssueMaterialHeading +
		"Repository: o/r\nIssue: #4\nTitle: T\n\nB"
	if got != want {
		t.Errorf("got %q", got)
	}
}

// TestImplementerPromptRework pins: agent prompt rework.
func TestImplementerPromptRework(t *testing.T) {
	got := ImplementerPrompt(nil, "o/r", Issue{Number: 4, Title: "T", Body: "B"}, "FEEDBACK")
	if !strings.HasSuffix(got, "Fix every high and medium finding below on top of the existing commits, then commit.\n\nReview:\nFEEDBACK") {
		t.Errorf("got %q", got)
	}
}

// TestImplementerPromptTruncatesBodyAndFeedback pins: agent prompt truncates body and feedback.
func TestImplementerPromptTruncatesBodyAndFeedback(t *testing.T) {
	long := strings.Repeat("é", BodyLimitChars+50)
	got := ImplementerPrompt(nil, "o/r", Issue{Number: 1, Title: "T", Body: long}, long)
	if n := strings.Count(got, "é"); n != 2*BodyLimitChars {
		t.Errorf("kept %d runes", n)
	}
}

// TestMarkers pins: markers.
func TestMarkers(t *testing.T) {
	if got := ReviewMarker("abc"); got != "<!-- agent-review:abc -->" {
		t.Error(got)
	}
	if got := RetroMarker(5, "abc"); got != "<!-- agent-retro:5:abc -->" {
		t.Error(got)
	}
}

// TestParseReview pins: parse review.
func TestParseReview(t *testing.T) {
	summary, findings, err := ParseReview(`{"summary":"s","findings":[
		{"severity":"high","title":"t","detail":"d","location":"a.go:1"},
		{"severity":"low","title":"t2","detail":"d2"}]}`)
	if err != nil || summary != "s" || len(findings) != 2 {
		t.Fatalf("%q %v %v", summary, findings, err)
	}
	if findings[0].Location != "a.go:1" || findings[1].Location != "" {
		t.Errorf("locations %v", findings)
	}
}

// TestParseReviewFailsClosed pins: parse review fails closed.
func TestParseReviewFailsClosed(t *testing.T) {
	bad := []string{
		``, `not json`, `{}`, `{"summary":"s"}`, `{"findings":[]}`,
		`{"summary":1,"findings":[]}`, `{"summary":"s","findings":{}}`,
		`{"summary":"s","findings":[{"severity":"critical","title":"t","detail":"d"}]}`,
		`{"summary":"s","findings":[{"title":"t","detail":"d"}]}`,
		`{"summary":"s","findings":[{"severity":"high","detail":"d"}]}`,
		`{"summary":"s","findings":[{"severity":"high","title":"t"}]}`,
	}
	for _, text := range bad {
		if _, _, err := ParseReview(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}

// TestBlocking pins: blocking.
func TestBlocking(t *testing.T) {
	fs := []Finding{{Severity: "high"}, {Severity: "medium"}, {Severity: "low"}}
	if got := Blocking(fs); len(got) != 2 {
		t.Errorf("got %v", got)
	}
}

// TestRenderReview pins: render review.
func TestRenderReview(t *testing.T) {
	got := RenderReview("sha1", "all fine", []Finding{
		{Severity: "low", Title: "nit", Detail: "d", Location: ""},
		{Severity: "high", Title: "bug", Detail: "bad", Location: "a.go:3"},
	})
	want := "<!-- agent-review:sha1 -->\n## Agent review\n\nall fine\n\n### High (1)\n- **bug** (`a.go:3`): bad\n\n### Low (1)\n- **nit**: d"
	if got != want {
		t.Errorf("got %q", got)
	}
	if clean := RenderReview("s", "ok", nil); !strings.HasSuffix(clean, "\n\nNo findings.") {
		t.Errorf("got %q", clean)
	}
}

// TestRenderReviewTruncates pins: render review truncates.
func TestRenderReviewTruncates(t *testing.T) {
	got := RenderReview("s", strings.Repeat("x", CommentLimitChars+10), nil)
	if len([]rune(got)) != CommentLimitChars {
		t.Errorf("len %d", len([]rune(got)))
	}
}

// TestReviewRoundsCountsDistinctCommitsIncludingCurrent pins: review rounds counts distinct commits including current.
func TestReviewRoundsCountsDistinctCommitsIncludingCurrent(t *testing.T) {
	reviews := []Item{
		{Body: ReviewMarker("a") + "\nx"}, {Body: ReviewMarker("a") + "\ny"},
		{Body: ReviewMarker("b")}, {Body: "human review"},
	}
	if got := ReviewRounds(reviews, "a"); got != 2 {
		t.Errorf("repeat sha: %d", got)
	}
	if got := ReviewRounds(reviews, "c"); got != 3 {
		t.Errorf("new sha: %d", got)
	}
}

// TestGitAuthEnvKeepsTokenOutOfArgv pins: git auth env keeps token out of argv.
func TestGitAuthEnvKeepsTokenOutOfArgv(t *testing.T) {
	env := GitAuthEnv("tok")
	// base64("x-access-token:tok")
	if env["GIT_CONFIG_VALUE_0"] != "Authorization: Basic eC1hY2Nlc3MtdG9rZW46dG9r" ||
		env["GIT_CONFIG_KEY_0"] != "http.https://github.com/.extraheader" ||
		env["GIT_CONFIG_COUNT"] != "1" || env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("got %v", env)
	}
}

// TestPromptsForRetroAndReview pins: prompts for retro and review.
func TestPromptsForRetroAndReview(t *testing.T) {
	r := RetroPrompt("o/r", 9, "/w/out.md")
	if !strings.Contains(r, "#9 of o/r") || !strings.Contains(r, "to /w/out.md;") {
		t.Error(r)
	}
	p := ReviewerPrompt(nil, "main", "/w/spec.md", "/w/out.json")
	for _, want := range []string{"origin/main", "saved at /w/spec.md", "JSON to /w/out.json"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in %q", want, p)
		}
	}
}
