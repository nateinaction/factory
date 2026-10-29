package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// TestImplementerPromptCarriesIssueAfterDocs pins: implementer prompt first run.
func TestImplementerPromptCarriesIssueAfterDocs(t *testing.T) {
	got := Implementer([]Doc{{Path: "AGENTS.md", Text: "RULES"}}, "o/r", workflow.Issue{Number: 4, Title: "T", Body: "B"}, "")
	want := implementerRolePrompt + "\n\n# Repository docs\n\n## AGENTS.md\n\nRULES\n" + IssueMaterialHeading +
		"Repository: o/r\nIssue: #4\nTitle: T\n\nB"
	if got != want {
		t.Errorf("got %q", got)
	}
}

// TestImplementerPromptRework pins: agent prompt rework.
func TestImplementerPromptRework(t *testing.T) {
	got := Implementer(nil, "o/r", workflow.Issue{Number: 4, Title: "T", Body: "B"}, "FEEDBACK")
	if !strings.HasSuffix(got, "Fix every high and medium finding below on top of the existing commits, then commit.\n\nReview:\nFEEDBACK") {
		t.Errorf("got %q", got)
	}
}

// TestImplementerPromptTruncatesBodyAndFeedback pins: agent prompt truncates body and feedback.
func TestImplementerPromptTruncatesBodyAndFeedback(t *testing.T) {
	long := strings.Repeat("é", workflow.BodyLimitChars+50)
	got := Implementer(nil, "o/r", workflow.Issue{Number: 1, Title: "T", Body: long}, long)
	if n := strings.Count(got, "é"); n != 2*workflow.BodyLimitChars {
		t.Errorf("kept %d runes", n)
	}
}

// TestPromptsForRetroAndReview pins: prompts for retro and review.
func TestPromptsForRetroAndReview(t *testing.T) {
	r := Retro("o/r", 9, "/w/out.md")
	if !strings.Contains(r, "#9 of o/r") || !strings.Contains(r, "to /w/out.md;") {
		t.Error(r)
	}
	p := Reviewer(nil, "main", "/w/spec.md", "/w/out.json")
	for _, want := range []string{"origin/main", "saved at /w/spec.md", "JSON to /w/out.json"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in %q", want, p)
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
