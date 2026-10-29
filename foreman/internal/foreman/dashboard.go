package foreman

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// redrawDashboard gives a repository its retro dashboard, applies the writer commands on it and on its
// promoted issues, and rewrites the body from the retro log only when the rendered body differs. It
// never comments on the dashboard, and the poll never routes it: an issue titled DashboardTitle is never
// routed. When a retro added findings, a group became ready or a writer asked with `curate`, it queues
// one Curator run for the repository, at most one at a time and once a day.
func redrawDashboard(
	ctx context.Context, d Deps, cfg config.Config, c github.Client, perms *permissions,
	repo workflow.Repo, roles []workflow.Role, busy map[workflow.Role]bool, role workflow.Role,
) error {
	issues, err := c.ListIssues(ctx, repo, github.StateAll, github.Newest)
	if err != nil {
		return fmt.Errorf("list the dashboard's issues: %w", err)
	}

	reports, threads, err := retroLog(ctx, d, c, repo, issues)
	if err != nil {
		return err
	}

	number, body, found, err := findDashboardIssue(ctx, c, repo, issues)
	if err != nil {
		return err
	}

	state, err := dashboardState(body, found)
	if err != nil {
		return err
	}

	run := &commandRun{
		d: d, cfg: cfg, c: c, perms: perms, repo: repo, role: role,
		issues: issues, threads: threads, reports: reports,
	}
	state, settle := run.run(ctx, state, number, found)

	number, due, err := drawDashboard(ctx, d, cfg, c, repo, reports, state, roles, busy, number, body, found)
	if err != nil {
		return err
	}

	settleCommands(ctx, d, c, repo, settle)

	if !due {
		return nil
	}

	// The claim is already in the body, so a failed enqueue is retried once the claim ages out.
	if err := enqueueCurator(ctx, d, workflow.IssueRef{Repo: repo, Number: number}); err != nil {
		d.log().Warn("cannot queue the Curator run", "repo", repo, "issue", number, "error", err)
	}

	return nil
}

// drawDashboard renders the dashboard, claims a due Curator run before drawing, and writes the body
// only when it changed. due is true when the caller should queue the run.
func drawDashboard(
	ctx context.Context, d Deps, cfg config.Config, c github.Client, repo workflow.Repo,
	reports []workflow.DashboardReport, state workflow.DashboardState, roles []workflow.Role,
	busy map[workflow.Role]bool, number int, body string, found bool,
) (int, bool, error) {
	now := d.now()
	due := curatorWanted(roles, cfg, reports, state, now, busy[workflow.Curator])

	if due {
		state = state.WithCuratorClaim(now)
	}

	text, err := workflow.RenderDashboard(workflow.BuildDashboard(reports, state, now))
	if err != nil {
		return number, false, fmt.Errorf("draw the dashboard: %w", err)
	}

	number, err = writeDashboard(ctx, d, c, repo, number, body, text, found)

	return number, due, err
}

// settleCommands minimizes or deletes the commands foreman dealt with, logging one that stays visible.
func settleCommands(ctx context.Context, d Deps, c github.Client, repo workflow.Repo, settle []workflow.Item) {
	for _, comment := range settle {
		if err := settleCommand(ctx, d, c, repo, comment); err != nil {
			// The action is recorded and the comment is marked dealt with, so a command gone quiet is
			// not acted on twice; the comment only stays visible.
			d.log().Warn("cannot minimize or delete a dashboard command", "repo", repo,
				"comment", comment.ID, "error", err)
		}
	}
}

// curatorWanted reports whether a repository's dashboard should start a Curator run now. A repository
// whose Curator App is not installed, whose curator has no configured deadline, or whose Curator DAG is
// already busy starts none.
func curatorWanted(
	roles []workflow.Role, cfg config.Config, reports []workflow.DashboardReport,
	state workflow.DashboardState, now time.Time, busy bool,
) bool {
	if busy || cfg.Unhosted(workflow.Curator) || !slices.Contains(roles, workflow.Curator) {
		return false
	}

	agent, ok := cfg.Agents[string(workflow.Curator)]
	if !ok || agent.Deadline() <= 0 {
		return false
	}

	return workflow.CuratorDue(reports, state, now, agent.Deadline()+cfg.DeadRunMargin())
}

// enqueueCurator queues one Curator run for a repository's dashboard.
func enqueueCurator(ctx context.Context, d Deps, ref workflow.IssueRef) error {
	_, err := d.run(ctx, proc.Cmd{Argv: []string{
		d.Settings.Dagu, "enqueue", d.Settings.DAGPrefix + string(workflow.Curator), "--",
		"REPO=" + ref.Repo.String(), "ISSUE=" + strconv.Itoa(ref.Number),
	}})

	return err
}

// dashboardState reads the state an existing body holds. A body foreman cannot read whole is never
// overwritten: its state would be lost.
func dashboardState(body string, found bool) (workflow.DashboardState, error) {
	if !found {
		return workflow.DashboardState{}, nil
	}

	state, err := workflow.ParseDashboard(body)
	if err != nil {
		return workflow.DashboardState{}, fmt.Errorf("read the dashboard body: %w", err)
	}

	return state, nil
}

// writeDashboard creates the dashboard when it is missing, and otherwise writes only a changed body.
// It returns the dashboard's number, including the one it just created.
func writeDashboard(
	ctx context.Context, d Deps, c github.Client, repo workflow.Repo, number int, body, text string, found bool,
) (int, error) {
	switch {
	case !found:
		issue, err := c.CreateIssue(ctx, repo, workflow.DashboardTitle, text, nil)
		if err != nil {
			return 0, fmt.Errorf("create the dashboard: %w", err)
		}

		return issue.Number, nil
	case text == body:
		return number, nil
	}

	if err := c.UpdateIssueBody(ctx, workflow.IssueRef{Repo: repo, Number: number}, text); err != nil {
		return number, fmt.Errorf("update the dashboard: %w", err)
	}

	d.log().Info("redrew the retro dashboard", "repo", repo, "issue", number)

	return number, nil
}

// findDashboardIssue finds the repository's dashboard, scanning from the oldest end when the newest scan
// did not hold it. One scan holds at most MaxPages pages of issues, so a busy repository never grows a
// second dashboard.
func findDashboardIssue(
	ctx context.Context, c github.Client, repo workflow.Repo, issues []workflow.Issue,
) (number int, body string, found bool, err error) {
	number, body, found = findDashboard(issues)
	if found {
		return number, body, found, nil
	}

	oldest, err := c.ListIssues(ctx, repo, github.StateAll, github.Oldest)
	if err != nil {
		return 0, "", false, fmt.Errorf("list the dashboard's issues: %w", err)
	}

	number, body, found = findDashboard(oldest)

	return number, body, found, nil
}

// promotedThread is a promoted issue's comments and the group it was promoted as.
type promotedThread struct {
	// group is the group the issue came from.
	group workflow.GroupID
	// comments are the issue's comments, oldest first.
	comments []workflow.Item
}

// retroLog reads every retro marker comment on the repository's issues, and the threads of the issues a
// retro group was promoted as. An unreadable issue fails the whole read, so a body is never drawn from a
// partial log. A comment that does not parse is logged and skipped: the log is append-only, and one bad
// comment must not freeze the dashboard.
func retroLog(
	ctx context.Context, d Deps, c github.Client, repo workflow.Repo, issues []workflow.Issue,
) ([]workflow.DashboardReport, map[int]promotedThread, error) {
	var reports []workflow.DashboardReport

	threads := map[int]promotedThread{}

	for _, issue := range issues {
		if issue.IsPullRequest || issue.Comments == 0 {
			continue
		}

		comments, err := c.ListComments(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number})
		if err != nil {
			return nil, nil, fmt.Errorf("read the retro log of #%d: %w", issue.Number, err)
		}

		reports = append(reports, retroReports(d, issue.Number, comments)...)

		group, promoted, err := workflow.ParsePromoted(issue.Body)
		if err != nil {
			d.log().Warn("skipping a promoted issue whose marker is unreadable", "issue", issue.Number, "error", err)
		} else if promoted {
			threads[issue.Number] = promotedThread{group: group, comments: comments}
		}
	}

	return reports, threads, nil
}

// retroReports reads one issue's retro comments as dashboard reports. Only a factory App's report
// counts, so an outsider can never feed findings to the dashboard or the Curator.
func retroReports(d Deps, number int, comments []workflow.Item) []workflow.DashboardReport {
	var reports []workflow.DashboardReport

	for _, comment := range comments {
		if !workflow.FactoryAuthored(comment.Author) {
			continue
		}

		rc, ok, err := workflow.ParseRetroComment(comment.Body)
		if err != nil {
			d.log().Warn("skipping a retro comment the dashboard cannot read", "issue", number, "error", err)

			continue
		}

		if !ok {
			continue
		}

		for _, finding := range rc.Findings {
			reports = append(reports, workflow.DashboardReport{
				Issue: number, Role: rc.Role, Key: rc.Key, At: comment.UpdatedAt,
				Target: finding.Target, Group: finding.Group, Severity: finding.Severity,
				Claim: finding.Claim, Evidence: finding.Evidence,
			})
		}
	}

	return reports
}

// findDashboard picks the repository's dashboard: the oldest factory App's issue titled DashboardTitle,
// open or closed. Another author's issue is never the dashboard, so an outsider cannot file it first. A
// dashboard a person renames is a new one on the next poll, which is the least surprising thing to do
// with a body foreman no longer owns.
func findDashboard(issues []workflow.Issue) (number int, body string, found bool) {
	for _, issue := range issues {
		if issue.Title != workflow.DashboardTitle || !workflow.FactoryAuthored(issue.Author) {
			continue
		}

		if found && issue.Number > number {
			continue
		}

		number, body, found = issue.Number, issue.Body, true
	}

	return number, body, found
}
