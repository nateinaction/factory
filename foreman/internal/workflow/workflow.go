// Package workflow is foreman's pure core: agent roles, labels, markers and review verdicts. It does
// no I/O, so every rule here can be checked by calling a function with values.
package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Role is an agent role. Only the known set parses; anything else fails closed.
type Role string

// The agent roles named by the spec.
const (
	// Planner turns an issue into questions or a proposal.
	Planner Role = "planner"
	// Researcher answers a research ticket.
	Researcher Role = "researcher"
	// Implementer turns an issue into a pull request.
	Implementer Role = "implementer"
	// Reviewer reviews a pull request.
	Reviewer Role = "reviewer"
	// Curator proposes merges for the retro dashboard.
	Curator Role = "curator"
)

// KnownRoles is the closed set of agent roles, in spec order.
func KnownRoles() []Role {
	return []Role{Planner, Researcher, Implementer, Reviewer, Curator}
}

// Limits shared by the prompts, comments and API calls.
const (
	// BodyLimitChars caps issue and review text put into a prompt.
	BodyLimitChars = 20_000
	// CommentLimitChars caps any comment foreman posts.
	CommentLimitChars = 60_000
	// MaxReviewRounds is how many distinct commits are reviewed before a person is called.
	MaxReviewRounds = 3
)

// Each agent role's wall-clock deadline, from the spec's limits table. The scheduling slice moves
// them into group_vars; `foreman status` compares a run's elapsed time against them until then.
const (
	// PlannerDeadline bounds a Planner run.
	PlannerDeadline = 20 * time.Minute
	// ResearcherDeadline bounds a Researcher run.
	ResearcherDeadline = 45 * time.Minute
	// ImplementerDeadline bounds an Implementer run.
	ImplementerDeadline = 90 * time.Minute
	// ReviewerDeadline bounds a Reviewer run.
	ReviewerDeadline = 30 * time.Minute
	// CuratorDeadline bounds a Curator run.
	CuratorDeadline = 30 * time.Minute
)

// Deadline is the agent role's wall-clock deadline. An unknown role has none.
func Deadline(role Role) time.Duration {
	switch role {
	case Planner:
		return PlannerDeadline
	case Researcher:
		return ResearcherDeadline
	case Implementer:
		return ImplementerDeadline
	case Reviewer:
		return ReviewerDeadline
	case Curator:
		return CuratorDeadline
	}

	return 0
}

// Exit codes of `run reviewer`, read by `start reviewer`.
const (
	// ReviewClean means no high or medium findings.
	ReviewClean = 0
	// ReviewFindings means high or medium findings were posted.
	ReviewFindings = 10
	// ReviewExhausted means findings remain after MaxReviewRounds rounds.
	ReviewExhausted = 11
)

// Labels that drive today's flow. Old names are kept until the label slice.
const (
	// LabelReady queues an issue for the Implementer.
	LabelReady = "ready-for-agent"
	// LabelWorking marks an issue the Implementer has claimed.
	LabelWorking = "agent-working"
	// LabelReviewReady queues an issue for the Reviewer.
	LabelReviewReady = "ready-for-agent-review"
	// LabelReviewing marks an issue the Reviewer has claimed.
	LabelReviewing = "agent-reviewing"
	// LabelDone marks an issue whose review found nothing blocking.
	LabelDone = "agent-done"
	// LabelFailed marks an issue a person needs to look at.
	LabelFailed = "agent-failed"
	// LabelRetro labels the retro ticket.
	LabelRetro = "retro"
	// RetroTitle is the retro ticket's title.
	RetroTitle = "Agent retros"
)

// Severity is how much a review finding matters. Only the known set parses.
type Severity string

// The severities of a review finding.
const (
	// SeverityHigh is a bug, a spec miss or a security problem that must be fixed before merge.
	SeverityHigh Severity = "high"
	// SeverityMedium is a documented-standard breach or a notable defect.
	SeverityMedium Severity = "medium"
	// SeverityLow is a judgement call or a nit.
	SeverityLow Severity = "low"
)

// Severities is the closed set of severities, most severe first.
func Severities() []Severity {
	return []Severity{SeverityHigh, SeverityMedium, SeverityLow}
}

// Blocks reports whether a finding of this severity sends the pull request back.
func (s Severity) Blocks() bool {
	switch s {
	case SeverityHigh, SeverityMedium:
		return true
	case SeverityLow:
		return false
	}

	return false
}

// title is the severity as a heading word: "High".
func (s Severity) title() string {
	return strings.ToUpper(string(s[:1])) + string(s[1:])
}

var (
	// issueNumberRE matches digits only.
	issueNumberRE = regexp.MustCompile(`^[0-9]{1,9}$`)
	// repoRE matches owner/name.
	repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// ParseRole accepts only a known agent role.
func ParseRole(name string) (Role, error) {
	for _, r := range KnownRoles() {
		if string(r) == name {
			return r, nil
		}
	}

	return "", fmt.Errorf("unknown agent role %q", name)
}

// ParseIssueNumber accepts digits only: the number ends up in a path and a branch name.
func ParseIssueNumber(text string) (int, error) {
	if !issueNumberRE.MatchString(text) {
		return 0, fmt.Errorf("not an issue number: %q", text)
	}

	var n int
	if _, err := fmt.Sscanf(text, "%d", &n); err != nil {
		return 0, fmt.Errorf("not an issue number: %q: %w", text, err)
	}

	return n, nil
}

// ValidateRepo checks the owner/name shape, since a repo ends up in paths and URLs.
func ValidateRepo(repo string) error {
	if !repoRE.MatchString(repo) || strings.HasPrefix(repo, "..") || strings.Contains(repo, "/..") {
		return fmt.Errorf("repo must be owner/name, got %q", repo)
	}

	return nil
}

// Issue is the part of a GitHub issue foreman reads.
type Issue struct {
	Number        int
	Title         string
	Body          string
	IsPullRequest bool
}

// UnmarshalJSON reads an issue; the issues endpoint marks pull requests with a pull_request key.
func (i *Issue) UnmarshalJSON(data []byte) error {
	var raw struct {
		Number      int              `json:"number"`
		Title       string           `json:"title"`
		Body        string           `json:"body"`
		PullRequest *json.RawMessage `json:"pull_request"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode issue: %w", err)
	}

	*i = Issue{Number: raw.Number, Title: raw.Title, Body: raw.Body, IsPullRequest: raw.PullRequest != nil}

	return nil
}

// Item is a comment or review: an id and a body.
type Item struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// PickIssue returns the first issue that is not a pull request: oldest first, as the API returns them.
func PickIssue(issues []Issue) (Issue, bool) {
	for _, i := range issues {
		if !i.IsPullRequest {
			return i, true
		}
	}

	return Issue{}, false
}

// BranchName is the branch an issue's work lives on.
func BranchName(number int) string {
	return fmt.Sprintf("agent/issue-%d", number)
}

// Truncate keeps at most n characters; none when n is not positive.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}

	r := []rune(s)
	if len(r) <= n {
		return s
	}

	return string(r[:n])
}

// ReviewMarker keys a review to the commit it judged.
func ReviewMarker(sha string) string {
	return fmt.Sprintf("<!-- agent-review:%s -->", sha)
}

// ReviewMarkerPrefix starts every review foreman posts.
const ReviewMarkerPrefix = "<!-- agent-review:"

// RetroMarker keys a retro comment to an issue and commit.
func RetroMarker(number int, sha string) string {
	return fmt.Sprintf("<!-- agent-retro:%d:%s -->", number, sha)
}

// Finding is one review finding.
type Finding struct {
	Severity Severity
	Title    string
	Detail   string
	Location string
}

// ParseReview is strict: anything malformed is an error, so a bad review can never read as an approval.
func ParseReview(text string) (string, []Finding, error) {
	var raw struct {
		Summary  *string `json:"summary"`
		Findings *[]struct {
			Severity *string `json:"severity"`
			Title    *string `json:"title"`
			Detail   *string `json:"detail"`
			Location *string `json:"location"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return "", nil, fmt.Errorf("review is not valid: %w", err)
	}

	if raw.Summary == nil || raw.Findings == nil {
		return "", nil, errors.New("review needs a string summary and a list of findings")
	}

	findings := make([]Finding, 0, len(*raw.Findings))
	for _, f := range *raw.Findings {
		if f.Severity == nil || f.Title == nil || f.Detail == nil {
			return "", nil, errors.New("finding needs a severity, a title and a detail")
		}

		sev := Severity(*f.Severity)
		if !slices.Contains(Severities(), sev) {
			return "", nil, fmt.Errorf("unknown severity: %q", *f.Severity)
		}

		loc := ""
		if f.Location != nil {
			loc = *f.Location
		}

		findings = append(findings, Finding{Severity: sev, Title: *f.Title, Detail: *f.Detail, Location: loc})
	}

	return *raw.Summary, findings, nil
}

// Blocking keeps the findings that must be fixed before the pull request is accepted.
func Blocking(findings []Finding) []Finding {
	var out []Finding

	for _, f := range findings {
		if f.Severity.Blocks() {
			out = append(out, f)
		}
	}

	return out
}

// RenderReview is the review body posted on the pull request.
func RenderReview(sha, summary string, findings []Finding) string {
	lines := []string{ReviewMarker(sha), "## Agent review", "", summary}

	for _, sev := range Severities() {
		var group []string

		for _, f := range findings {
			if f.Severity != sev {
				continue
			}

			loc := ""
			if f.Location != "" {
				loc = " (`" + f.Location + "`)"
			}

			group = append(group, fmt.Sprintf("- **%s**%s: %s", f.Title, loc, f.Detail))
		}

		if len(group) > 0 {
			lines = append(lines, "", fmt.Sprintf("### %s (%d)", sev.title(), len(group)))
			lines = append(lines, group...)
		}
	}

	if len(findings) == 0 {
		lines = append(lines, "", "No findings.")
	}

	return Truncate(strings.Join(lines, "\n"), CommentLimitChars)
}

// StartsWith keeps the items whose body starts with prefix, in API order.
func StartsWith(items []Item, prefix string) []Item {
	var out []Item

	for _, i := range items {
		if strings.HasPrefix(i.Body, prefix) {
			out = append(out, i)
		}
	}

	return out
}

// ReviewRounds counts the distinct commits reviewed so far, counting sha whether or not it is posted yet.
func ReviewRounds(reviews []Item, sha string) int {
	marked := map[string]bool{ReviewMarker(sha): true}

	for _, r := range StartsWith(reviews, ReviewMarkerPrefix) {
		first, _, _ := strings.Cut(r.Body, "\n")
		marked[first] = true
	}

	return len(marked)
}
