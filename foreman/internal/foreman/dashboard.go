package foreman

import (
	"context"
	"fmt"

	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// redrawDashboard gives a repository its retro dashboard, creating it when it is missing, and rewrites
// the body from the retro log only when the rendered body differs. It never comments on the dashboard,
// and the poll never routes it: an issue titled DashboardTitle is never routed.
func redrawDashboard(ctx context.Context, d Deps, c github.Client, repo string) error {
	issues, err := c.ListIssues(ctx, repo, github.StateAll)
	if err != nil {
		return fmt.Errorf("list the dashboard's issues: %w", err)
	}

	reports, err := retroLog(ctx, d, c, repo, issues)
	if err != nil {
		return err
	}

	number, body, found, err := findDashboardIssue(ctx, c, repo, issues)
	if err != nil {
		return err
	}

	text, err := drawnDashboard(d, reports, body, found)
	if err != nil {
		return err
	}

	switch {
	case !found:
		if _, err := c.CreateIssue(ctx, repo, workflow.DashboardTitle, text, nil); err != nil {
			return fmt.Errorf("create the dashboard: %w", err)
		}

		return nil
	case text == body:
		return nil
	}

	if err := c.UpdateIssueBody(ctx, repo, number, text); err != nil {
		return fmt.Errorf("update the dashboard: %w", err)
	}

	d.log().Info("redrew the retro dashboard", "repo", repo, "issue", number)

	return nil
}

// findDashboardIssue finds the repository's dashboard, scanning from the oldest end when the newest scan
// did not hold it. One scan holds at most MaxPages pages of issues, so a busy repository never grows a
// second dashboard.
func findDashboardIssue(
	ctx context.Context, c github.Client, repo string, issues []workflow.Issue,
) (number int, body string, found bool, err error) {
	number, body, found = findDashboard(issues)
	if found {
		return number, body, found, nil
	}

	oldest, err := c.ListOldestIssues(ctx, repo, github.StateAll)
	if err != nil {
		return 0, "", false, fmt.Errorf("list the dashboard's issues: %w", err)
	}

	number, body, found = findDashboard(oldest)

	return number, body, found, nil
}

// drawnDashboard renders the body from the log, replaying the state an existing body holds. A body
// foreman cannot read whole is never overwritten: its state would be lost.
func drawnDashboard(d Deps, reports []workflow.DashboardReport, body string, found bool) (string, error) {
	state := workflow.DashboardState{}

	if found {
		var err error
		if state, err = workflow.ParseDashboard(body); err != nil {
			return "", fmt.Errorf("read the dashboard body: %w", err)
		}
	}

	text, err := workflow.RenderDashboard(workflow.BuildDashboard(reports, state, d.now()))
	if err != nil {
		return "", fmt.Errorf("draw the dashboard: %w", err)
	}

	return text, nil
}

// retroLog reads every retro marker comment on the repository's issues. An unreadable issue fails the
// whole read, so a body is never drawn from a partial log. A comment that does not parse is logged
// and skipped: the log is append-only, and one bad comment must not freeze the dashboard.
func retroLog(
	ctx context.Context, d Deps, c github.Client, repo string, issues []workflow.Issue,
) ([]workflow.DashboardReport, error) {
	var reports []workflow.DashboardReport

	for _, issue := range issues {
		if issue.IsPullRequest || issue.Comments == 0 {
			continue
		}

		comments, err := c.ListComments(ctx, repo, issue.Number)
		if err != nil {
			return nil, fmt.Errorf("read the retro log of #%d: %w", issue.Number, err)
		}

		reports = append(reports, retroReports(d, issue.Number, comments)...)
	}

	return reports, nil
}

// retroReports reads one issue's retro comments as dashboard reports.
func retroReports(d Deps, number int, comments []workflow.Item) []workflow.DashboardReport {
	var reports []workflow.DashboardReport

	for _, comment := range comments {
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
				Claim: finding.Claim,
			})
		}
	}

	return reports
}

// findDashboard picks the repository's dashboard: the oldest issue titled DashboardTitle, open or
// closed. A dashboard a person renames is a new one on the next poll, which is the least surprising
// thing to do with a body foreman no longer owns.
func findDashboard(issues []workflow.Issue) (number int, body string, found bool) {
	for _, issue := range issues {
		if issue.Title != workflow.DashboardTitle {
			continue
		}

		if found && issue.Number > number {
			continue
		}

		number, body, found = issue.Number, issue.Body, true
	}

	return number, body, found
}
