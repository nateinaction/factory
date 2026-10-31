package foreman

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// Ceilings on what `foreman history` reads.
const (
	// HistoryIssueLimit bounds how many issues, newest first, a history reads.
	HistoryIssueLimit = 50
	// HistoryListPages bounds the pages of the repository's issue listing: pull requests share it, so it
	// holds more than HistoryIssueLimit issues.
	HistoryListPages = 3
	// HistoryThreadPages bounds the pages read of one issue's comments, and of its timeline.
	HistoryThreadPages = 2
)

// handoffPrefix starts the name of every factory label.
const handoffPrefix = "factory:"

// History is `foreman history`: a read-only report, from GitHub alone, of each issue's lifecycle in an
// assigned repository: the time in each handoff state, the wait at the human gate, the rework rounds
// and the outcome. The handoff markers and label events GitHub already holds are the whole record, so
// it covers issues the factory handled before any other telemetry existed. It never writes, and it
// says when it stopped at a bound.
func History(ctx context.Context, d Deps, cfg config.Config, repo workflow.Repo) (int, error) {
	role, ok := pollingRole(Assigned(ctx, d, cfg.Roles)[repo])
	if !ok {
		return 1, fmt.Errorf("%s is not an assigned repository: no factory App is installed on it", repo)
	}

	token, err := d.mint(ctx, role, repo, readOnlyToken)
	if err != nil {
		return 1, err
	}

	read, err := readHistories(ctx, d, d.client(token), repo)
	if err != nil {
		return 1, err
	}

	if _, err := io.WriteString(d.Out, read.render()); err != nil {
		return 1, fmt.Errorf("write history: %w", err)
	}

	return 0, nil
}

// historyRead is what a history read found, and where it stopped short.
type historyRead struct {
	// histories are the issues the factory handled, oldest first.
	histories []workflow.History
	// issuesCut is true when the repository has older issues than the ones read.
	issuesCut bool
	// partial are the issues whose comments or timeline ran past the page bound, so their figures
	// are incomplete.
	partial []int
	// unreadable are the issues with a factory marker that does not parse.
	unreadable []int
}

// readHistories reads the newest issues of a repository and builds each one's history.
func readHistories(ctx context.Context, d Deps, c github.Client, repo workflow.Repo) (historyRead, error) {
	issues, listCut, err := c.ListIssuesUpTo(ctx, repo, github.StateAll, github.Newest, HistoryListPages)
	if err != nil {
		return historyRead{}, err
	}

	issues = slices.DeleteFunc(issues, func(i workflow.Issue) bool { return i.IsPullRequest })

	read := historyRead{issuesCut: listCut || len(issues) > HistoryIssueLimit}
	if len(issues) > HistoryIssueLimit {
		issues = issues[:HistoryIssueLimit]
	}

	slices.SortFunc(issues, func(a, b workflow.Issue) int { return a.Number - b.Number })

	for _, issue := range issues {
		if err := read.add(ctx, d, c, workflow.IssueRef{Repo: repo, Number: issue.Number}); err != nil {
			return historyRead{}, err
		}
	}

	return read, nil
}

// add reads one issue's comments and timeline and records its history, when the factory handled it.
func (r *historyRead) add(ctx context.Context, d Deps, c github.Client, ref workflow.IssueRef) error {
	comments, commentsCut, err := c.CommentsUpTo(ctx, ref, HistoryThreadPages)
	if err != nil {
		return err
	}

	events, eventsCut, err := c.TimelineUpTo(ctx, ref, HistoryThreadPages)
	if err != nil {
		return err
	}

	if commentsCut || eventsCut {
		r.partial = append(r.partial, ref.Number)
	}

	markers, err := workflow.Markers(comments, ref.Repo.Owner())
	if err != nil {
		d.log().Warn("history: a factory marker does not parse", "repo", ref.Repo, "issue", ref.Number, "error", err)

		r.unreadable = append(r.unreadable, ref.Number)

		return nil
	}

	if h, handled := workflow.BuildHistory(ref.Number, markers, events, d.now()); handled {
		r.histories = append(r.histories, h)
	}

	return nil
}

// render draws the report as plain text: one row per issue, then where the read stopped short.
func (r *historyRead) render() string {
	var b strings.Builder

	rows := make([][]string, 0, 1+len(r.histories))
	rows = append(rows, []string{"ISSUE", "OUTCOME", "REWORK", "GATE", "TIME IN STATE"})

	for _, h := range r.histories {
		rows = append(rows, []string{
			"#" + strconv.Itoa(h.Number), outcomeLabel(h.Outcome), strconv.Itoa(h.Rework),
			duration(h.Gate), stateTimesLabel(h.Times),
		})
	}

	if len(rows) == 1 {
		b.WriteString(noRows)
	} else {
		b.WriteString(table(rows))
	}

	if r.issuesCut {
		fmt.Fprintf(&b, "Stopped at the bound: read the newest %d issues; older ones were not read.\n",
			HistoryIssueLimit)
	}

	if len(r.partial) > 0 {
		fmt.Fprintf(&b, "Stopped at the bound: %s has more than %d comments or events; its figures are partial.\n",
			issueList(r.partial), HistoryThreadPages*github.PageSize)
	}

	if len(r.unreadable) > 0 {
		fmt.Fprintf(&b, "Not reported: %s carries a factory marker that does not parse.\n", issueList(r.unreadable))
	}

	return b.String()
}

// outcomeLabel is the state an issue's latest handoff left it in, or a dash when there is none.
func outcomeLabel(s workflow.State) string {
	if s == "" {
		return noValue
	}

	return strings.TrimPrefix(string(s), handoffPrefix)
}

// stateTimesLabel is each state with its time, or a dash when the issue carried none.
func stateTimesLabel(times []workflow.StateTime) string {
	if len(times) == 0 {
		return noValue
	}

	parts := make([]string, 0, len(times))
	for _, t := range times {
		parts = append(parts, outcomeLabel(t.State)+" "+duration(t.Duration))
	}

	return strings.Join(parts, ", ")
}

// duration is d to the second.
func duration(d time.Duration) string {
	return d.Round(time.Second).String()
}

// issueList is issue numbers as "#1, #2".
func issueList(numbers []int) string {
	parts := make([]string, 0, len(numbers))
	for _, n := range numbers {
		parts = append(parts, "#"+strconv.Itoa(n))
	}

	return strings.Join(parts, ", ")
}
