package workflow

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ResearcherOutput is researcher-output.json.
type ResearcherOutput struct {
	// Version is OutputFileVersion.
	Version int `json:"version"`
	// Branch is the <name> in the findings branch research/<name>.
	Branch string `json:"branch"`
	// Resolution is the comment to post on the resolved ticket.
	Resolution string `json:"resolution"`
	// DecisionLine is the map's one-line gist of the decision.
	DecisionLine string `json:"decision_line"`
}

// researchNameRE matches one branch-safe segment: no slash, no leading dot or dash.
var researchNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ResearchBranch validates the output's name and returns the findings branch research/<name>.
func ResearchBranch(name string) (string, error) {
	switch {
	case !researchNameRE.MatchString(name):
		return "", fmt.Errorf("research branch name %q is not one safe branch segment", name)
	case strings.Contains(name, ".."), strings.HasSuffix(name, "."):
		return "", fmt.Errorf("research branch name %q is not one safe branch segment", name)
	}

	return "research/" + name, nil
}

// ParseResearcherOutput reads researcher-output.json strictly.
func ParseResearcherOutput(text string) (ResearcherOutput, error) {
	return parseOutput(text, ResearcherOutputFile, func(out ResearcherOutput) (ResearcherOutput, []string) {
		return out, researcherProblems(out)
	})
}

// researcherProblems checks the fields a Researcher output carries beyond the strict decode.
func researcherProblems(out ResearcherOutput) []string {
	var problems []string

	if _, err := ResearchBranch(out.Branch); err != nil {
		problems = append(problems, err.Error())
	}

	if out.Resolution == "" {
		problems = append(problems, "resolution is required")
	}

	switch {
	case out.DecisionLine == "":
		problems = append(problems, "decision_line is required")
	case strings.ContainsAny(out.DecisionLine, "\r\n"):
		problems = append(problems, "decision_line must be one line")
	}

	return problems
}

// DecisionsHeading starts the map's index of resolved tickets.
const DecisionsHeading = "## Decisions so far"

// PartOf reads the "Part of #N" fallback convention at the top of a child ticket's body.
func PartOf(body string) (int, bool) {
	first := ""

	for line := range strings.SplitSeq(body, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			first = trimmed

			break
		}
	}

	rest, ok := strings.CutPrefix(first, "Part of #")
	if !ok {
		return 0, false
	}

	digits, _, _ := strings.Cut(rest, " ")

	number, err := ParseIssueNumber(digits)
	if err != nil {
		return 0, false
	}

	return number, true
}

// IssueURL is an issue's web URL.
func IssueURL(ref IssueRef) string {
	return fmt.Sprintf("https://github.com/%s/issues/%d", ref.Repo, ref.Number)
}

// DecisionEntry renders the map's one-line index entry for a resolved ticket.
func DecisionEntry(title, url, gist string) string {
	return fmt.Sprintf("- [%s](%s): %s", escapeBrackets(OneLine(title)), url, OneLine(gist))
}

// escapeBrackets keeps a title from breaking the link text it sits in.
func escapeBrackets(text string) string {
	return strings.NewReplacer("[", `\[`, "]", `\]`).Replace(text)
}

// OneLine collapses every run of whitespace to one space.
func OneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// AppendDecision adds entry to the map's Decisions so far section, after its last line. An entry the
// section already carries leaves the body unchanged, so applying a resolution twice is safe.
func AppendDecision(body, entry string) (string, error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", errors.New("the decision line is empty")
	}

	lines := strings.Split(body, "\n")

	start := sectionStart(lines)
	if start < 0 {
		return "", fmt.Errorf("the map has no %q section", DecisionsHeading)
	}

	end := sectionEnd(lines, start)

	last, content, found := decisionPlacement(lines, start, end, entry)
	if found {
		return body, nil
	}

	// An empty section keeps the blank line under its heading, so the first entry reads as a list.
	if !content && start+1 < end && strings.TrimSpace(lines[start+1]) == "" {
		last = start + 1
	}

	out := make([]string, 0, len(lines))
	out = append(out, lines[:last+1]...)
	out = append(out, entry)

	// An empty section's blank line became the one under the heading, so the entry needs its own
	// blank line before the next section.
	if !content && last > start {
		out = append(out, "")
	}

	out = append(out, lines[last+1:]...)

	return strings.Join(out, "\n"), nil
}

// decisionPlacement finds where an entry goes in a section: after its last line, and whether the
// section already carries it.
func decisionPlacement(lines []string, start, end int, entry string) (last int, content, found bool) {
	last = start

	for i := start + 1; i < end; i++ {
		if strings.TrimSpace(lines[i]) == entry {
			return last, content, true
		}

		if strings.TrimSpace(lines[i]) != "" {
			last, content = i, true
		}
	}

	return last, content, false
}

// sectionStart is the line of the Decisions so far heading, or -1 when the body has none.
func sectionStart(lines []string) int {
	for i, line := range lines {
		if strings.TrimSpace(line) == DecisionsHeading {
			return i
		}
	}

	return -1
}

// sectionEnd is the line of the next section heading after start, or the end of the body.
func sectionEnd(lines []string, start int) int {
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			return i
		}
	}

	return len(lines)
}

// ResearchMarker keys a resolution to its findings branch, so a re-run updates that comment.
func ResearchMarker(branch string) string {
	return MarkerPrefix + "research branch=" + branch + " -->"
}

// RenderResolution is the resolution comment: its marker, the answer, and a link to the findings branch.
// The link is kept whole: the answer is cut to leave room for it.
func RenderResolution(repo Repo, branch, resolution string) string {
	link := fmt.Sprintf("https://github.com/%s/tree/%s", repo, branch)

	head := ResearchMarker(branch) + "\n\n"
	tail := fmt.Sprintf("\n\nFindings: [%s](%s)", branch, link)

	budget := CommentLimitChars - utf8.RuneCountInString(head) - utf8.RuneCountInString(tail)

	return head + Truncate(strings.TrimSpace(resolution), budget) + tail
}
