package foreman

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/prompt"
	"github.com/nateinaction/factory/foreman/internal/sandbox"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// runCurator reads a repository's dashboard and retro log in a fresh sandbox directory, and logs the
// run's one usage line even when the run failed. It runs no retro.
func runCurator(ctx context.Context, d Deps, spec runSpec) error {
	return withSandboxRun(d, spec, workflow.Curator, func(files sandboxFiles) (opencode.Usage, []string, error) {
		return curate(ctx, d, spec, files)
	})
}

// curate runs the Curator's work behind the run's proxies.
func curate(
	ctx context.Context, d Deps, spec runSpec, files sandboxFiles,
) (opencode.Usage, []string, error) {
	return withRunSecrets(ctx, d, spec, workflow.Curator,
		func(sec secrets, baseURL, proxyAddr string, network sandbox.Network) (opencode.Usage, error) {
			return curateRun(ctx, d, spec, sec, baseURL, proxyAddr, network, files)
		})
}

// curateRun clones the default branch read-only, reads the retro log and the dashboard state, runs the
// model under the sandbox, and applies its proposals.
func curateRun(
	ctx context.Context, d Deps, spec runSpec, sec secrets, baseURL, proxyAddr string,
	network sandbox.Network, files sandboxFiles,
) (opencode.Usage, error) {
	j, err := loadJob(ctx, d, sec, baseURL, spec.repo, workflow.Curator, spec.agent, spec.number,
		files.db(workflow.Curator))
	if err != nil {
		return opencode.Usage{}, err
	}

	paths, err := curatorPaths(d.Settings.WorkRoot, spec.number)
	if err != nil {
		return opencode.Usage{}, err
	}

	branch, err := j.c.DefaultBranch(ctx, spec.repo)
	if err != nil {
		return opencode.Usage{}, err
	}

	s := newSession(d, j, paths.workdir, nil)

	if err := s.cloneDefault(ctx, spec.repo, branch); err != nil {
		return opencode.Usage{}, err
	}

	docs, err := prompt.ReadRepoDocs(paths.workdir)
	if err != nil {
		return opencode.Usage{}, err
	}

	reports, state, private, err := curatorInputs(ctx, d, j.c, spec.repo, j.issue)
	if err != nil {
		return opencode.Usage{}, err
	}

	view := workflow.BuildDashboard(reports, state, d.now())

	if s, err = d.sandboxedWithToolchain(
		ctx, s, j, sec, spec.repo, branch, files, workflow.Curator, paths.workdir, paths.out,
		nil, network, proxyAddr,
	); err != nil {
		return opencode.Usage{}, err
	}

	res, err := s.curatorAgent(ctx, spec, docs, private, reports, view, paths.out)
	if err != nil {
		return res.Usage, err
	}

	out, err := readCuratorOutput(paths.out)
	if err != nil {
		// The dashboard is never commented on, so a rejected file is only logged; the claim ages out and
		// the next poll retries.
		return res.Usage, err
	}

	return res.Usage, s.applyCurator(ctx, j, spec.repo, spec.number, out, private)
}

// curatorInputs reads the repository's retro log, the dashboard's state and the repository's
// visibility. The dashboard issue is the run's issue.
func curatorInputs(
	ctx context.Context, d Deps, c github.Client, repo string, dashboard workflow.Issue,
) ([]workflow.DashboardReport, workflow.DashboardState, bool, error) {
	state, err := workflow.ParseDashboard(dashboard.Body)
	if err != nil {
		return nil, workflow.DashboardState{}, false, fmt.Errorf("read the dashboard body: %w", err)
	}

	issues, err := c.ListIssues(ctx, repo, github.StateAll)
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

// curatorPaths clears and returns a Curator run's clone and output path. The output file sits outside
// the clone, in the run's own scratch directory.
func curatorPaths(root string, number int) (runFiles, error) {
	workdir, err := freshPath(root, "curator-"+strconv.Itoa(number))
	if err != nil {
		return runFiles{}, err
	}

	scratch, err := scratchDir(root, "curator-out-"+strconv.Itoa(number))
	if err != nil {
		return runFiles{}, err
	}

	return runFiles{workdir: workdir, out: filepath.Join(scratch, workflow.CuratorOutputFile)}, nil
}

// cloneDefault clones only the default branch, which is all the Curator reads.
func (s session) cloneDefault(ctx context.Context, repo, branch string) error {
	_, err := s.d.run(ctx, proc.Cmd{
		Argv: []string{
			gitProgram, gitClone, "--depth", "1", "--branch", branch,
			"https://github.com/" + repo + ".git", s.workdir,
		},
		Env: s.gitEnv, Timeout: ToolchainTimeout,
	})

	return err
}

// readCuratorOutput reads and validates the Curator's output file.
func readCuratorOutput(path string) (workflow.CuratorOutput, error) {
	raw, err := readScratch(path)
	if err != nil {
		return workflow.CuratorOutput{}, outputProblem(workflow.CuratorOutputFile, err)
	}

	return workflow.ParseCuratorOutput(string(raw))
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
	ctx context.Context, j job, repo string, number int, out workflow.CuratorOutput, private bool,
) error {
	issue, err := j.c.GetIssue(ctx, repo, number)
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
	ctx context.Context, c github.Client, repo string, number int, before, after string,
) error {
	if before == after {
		return nil
	}

	return c.UpdateIssueBody(ctx, repo, number, after)
}

// fileCuratorIssues files each draft where its group belongs, reusing the issue an earlier run filed.
// A draft that cannot be filed is reported without stopping the others.
func (s session) fileCuratorIssues(
	ctx context.Context, j job, repo string, drafts []workflow.CuratorIssueDraft, private bool,
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

// curatorAgent runs the model on the Curator's prompt, built for the run's repository and dashboard
// number as a checked IssueRef.
func (s session) curatorAgent(
	ctx context.Context, spec runSpec, docs []prompt.Doc, private bool,
	reports []workflow.DashboardReport, view workflow.DashboardView, out string,
) (opencode.Result, error) {
	ref, err := issueRef(spec.repo, spec.number)
	if err != nil {
		return opencode.Result{}, err
	}

	return s.agent(ctx, opencode.Run{
		Role:    workflow.Curator,
		Prompt:  prompt.Curator(docs, ref, private, reports, view, out),
		Timeout: spec.agent.Deadline(), Steps: spec.agent.Steps,
	})
}

// issueRef checks a configured repository name at the edge and pairs it with an issue number.
func issueRef(repo string, number int) (workflow.IssueRef, error) {
	parsed, err := workflow.ParseRepo(repo)
	if err != nil {
		return workflow.IssueRef{}, err
	}

	return workflow.IssueRef{Repo: parsed, Number: number}, nil
}

// promotionRepo checks the configured names at the edge and asks workflow where a group is filed. An
// unset factory repo stays the zero Repo, which PromotionRepo refuses for a group that would leave a
// public repository.
func promotionRepo(g workflow.GroupID, repo string, private bool, factoryRepo string) (string, error) {
	here, err := workflow.ParseRepo(repo)
	if err != nil {
		return "", err
	}

	var factory workflow.Repo

	if factoryRepo != "" {
		if factory, err = workflow.ParseRepo(factoryRepo); err != nil {
			return "", err
		}
	}

	dest, err := workflow.PromotionRepo(g, here, private, factory)

	return dest.String(), err
}

// fileCuratorIssue files one draft in the repository its group belongs to.
func (s session) fileCuratorIssue(
	ctx context.Context, j job, repo string, draft workflow.CuratorIssueDraft, private bool,
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
	ctx context.Context, c github.Client, repo, dest string,
) (github.Client, error) {
	if dest == repo {
		return c, nil
	}

	token, err := s.d.mintToken(ctx, workflow.Curator, dest)
	if err != nil {
		return github.Client{}, fmt.Errorf("mint a token for %s: %w", dest, err)
	}

	return s.d.client(token), nil
}
