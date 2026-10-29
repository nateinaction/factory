package foreman

import (
	"context"
	"errors"
	"fmt"

	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// curatorInputs reads the repository's retro log, the dashboard's state and the repository's
// visibility. The dashboard issue is the run's issue.
func curatorInputs(
	ctx context.Context, d Deps, c github.Client, repo workflow.Repo, dashboard workflow.Issue,
) ([]workflow.DashboardReport, workflow.DashboardState, bool, error) {
	state, err := workflow.ParseDashboard(dashboard.Body)
	if err != nil {
		return nil, workflow.DashboardState{}, false, fmt.Errorf("read the dashboard body: %w", err)
	}

	issues, err := c.ListIssues(ctx, repo, github.StateAll, github.Newest)
	if err != nil {
		return nil, workflow.DashboardState{}, false, fmt.Errorf("list the dashboard's issues: %w", err)
	}

	reports, _, err := retroLog(ctx, d, c, repo, issues)
	if err != nil {
		return nil, workflow.DashboardState{}, false, err
	}

	repository, err := c.Repository(ctx, repo)
	if err != nil {
		return nil, workflow.DashboardState{}, false, fmt.Errorf("read the repository: %w", err)
	}

	return reports, state, repository.Private, nil
}

// cloneDefault clones only the default branch. That is all the Planner reads: the repository docs
// for its prompt, and the flake the run's toolchain resolves from. No other branch is cloned. The
// Curator never calls it: its App has no Contents permission.
func (s session) cloneDefault(ctx context.Context, repo workflow.Repo, branch string) error {
	_, err := s.d.run(ctx, proc.Cmd{
		Argv: []string{
			gitProgram, gitClone, "--depth", "1", "--branch", branch,
			"https://github.com/" + repo.String() + ".git", s.workdir,
		},
		Env: s.gitEnv, Timeout: ToolchainTimeout,
	})

	return err
}

// curatedIssue is an issue a Curator draft was filed as, or the one an earlier run already filed.
type curatedIssue struct {
	// group is the draft's group.
	group workflow.GroupID
	// issue is the filed issue.
	issue workflow.Issue
}

// applyCurator records the accepted proposals, files each promoted issue once, and records the run.
// Every effect is keyed, so applying the same output twice changes nothing.
func (s session) applyCurator(
	ctx context.Context, j job, repo workflow.Repo, number int, out workflow.CuratorOutput, private bool,
) error {
	issue, err := j.c.GetIssue(ctx, workflow.IssueRef{Repo: repo, Number: number})
	if err != nil {
		return err
	}

	reports, state, _, err := curatorInputs(ctx, s.d, j.c, repo, issue)
	if err != nil {
		return err
	}

	plan := workflow.PlanCurator(out, reports, state, private)

	filed, fileErr := s.fileCuratorIssues(ctx, j, repo, plan.Issues, private)

	now := s.d.now()

	for _, f := range filed {
		plan.State = plan.State.WithAction(workflow.DashboardAction{
			Kind: workflow.ActionPromote, Groups: []workflow.GroupID{f.group},
			By: string(workflow.Curator), At: now, URL: f.issue.HTMLURL,
		})
	}

	plan.State = plan.State.WithCuratorRun(now)

	text, err := workflow.RenderDashboard(workflow.BuildDashboard(reports, plan.State, now))

	return errors.Join(fileErr, err, s.writeDashboardBody(ctx, j.c, repo, number, issue.Body, text))
}

// writeDashboardBody writes the dashboard only when the redrawn body differs.
func (s session) writeDashboardBody(
	ctx context.Context, c github.Client, repo workflow.Repo, number int, before, after string,
) error {
	if before == after {
		return nil
	}

	return c.UpdateIssueBody(ctx, workflow.IssueRef{Repo: repo, Number: number}, after)
}

// fileCuratorIssues files each draft where its group belongs, reusing the issue an earlier run filed.
// A draft that cannot be filed is reported without stopping the others.
func (s session) fileCuratorIssues(
	ctx context.Context, j job, repo workflow.Repo, drafts []workflow.CuratorIssueDraft, private bool,
) ([]curatedIssue, error) {
	filed := make([]curatedIssue, 0, len(drafts))

	var errs []error

	for _, draft := range drafts {
		issue, err := s.fileCuratorIssue(ctx, j, repo, draft, private)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		filed = append(filed, curatedIssue{group: draft.Group, issue: issue})
	}

	return filed, errors.Join(errs...)
}

// promotionRepo checks the configured factory repository at the edge and asks workflow where a group is filed. An
// unset factory repo stays the zero Repo, which PromotionRepo refuses for a group that would leave a
// public repository.
func promotionRepo(g workflow.GroupID, repo workflow.Repo, private bool, factoryRepo string) (workflow.Repo, error) {
	var factory workflow.Repo

	if factoryRepo != "" {
		var err error
		if factory, err = workflow.ParseRepo(factoryRepo); err != nil {
			return workflow.Repo{}, err
		}
	}

	return workflow.PromotionRepo(g, repo, private, factory)
}

// fileCuratorIssue files one draft in the repository its group belongs to.
func (s session) fileCuratorIssue(
	ctx context.Context, j job, repo workflow.Repo, draft workflow.CuratorIssueDraft, private bool,
) (workflow.Issue, error) {
	dest, err := promotionRepo(draft.Group, repo, private, s.d.Settings.FactoryRepo)
	if err != nil {
		return workflow.Issue{}, fmt.Errorf("file %s: %w", draft.Group, err)
	}

	c, err := s.curatorClient(ctx, j.c, repo, dest)
	if err != nil {
		return workflow.Issue{}, err
	}

	existing, ok, err := listedPromoted(ctx, c, dest, draft.Group)
	if err != nil || ok {
		return existing, err
	}

	title, body, err := workflow.RenderCuratorDraft(draft.Group, draft, private)
	if err != nil {
		return workflow.Issue{}, fmt.Errorf("draft %s: %w", draft.Group, err)
	}

	issue, err := c.CreateIssue(ctx, dest, title, body, nil)
	if err != nil {
		return workflow.Issue{}, fmt.Errorf("file %s in %s: %w", draft.Group, dest, err)
	}

	s.d.log().Info("the Curator promoted a retro group",
		"repo", repo, "group", draft.Group, "dest", dest, "issue", issue.Number)

	return issue, nil
}

// curatorClient returns the client for dest: this run's client in the repository, and the Curator
// App's token minted for another repository.
func (s session) curatorClient(
	ctx context.Context, c github.Client, repo workflow.Repo, dest workflow.Repo,
) (github.Client, error) {
	if dest == repo {
		return c, nil
	}

	token, err := s.d.mint(ctx, workflow.Curator, dest, writableToken)
	if err != nil {
		return github.Client{}, fmt.Errorf("mint a token for %s: %w", dest, err)
	}

	return s.d.client(token), nil
}
