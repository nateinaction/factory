package foreman

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/dagu"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// MaxAssignedRepos bounds how many assigned repositories one poll looks at.
const MaxAssignedRepos = 200

// supervisorGitRuns is how many git ceilings the supervisor adds around a run: the clone and the push.
const supervisorGitRuns = 2

// tokenArgs is the helper command line that mints one agent role's token for one repository. A
// read-only token is restricted to reading the issue, its pull request and its contents.
func tokenArgs(role workflow.Role, repo workflow.Repo, readOnly bool) []string {
	args := []string{"token", "--app", string(role), "--repo", repo.String()}
	if readOnly {
		args = append(args, "--read-only")
	}

	return args
}

// mintToken asks the helper for a token of the agent role's own App, scoped to the repository.
// There is no fallback: if this App can't mint, no other App's token stands in for it.
func (d Deps) mintToken(ctx context.Context, role workflow.Role, repo workflow.Repo) (string, error) {
	return d.mint(ctx, role, repo, false)
}

// mintReadOnlyToken asks the helper for the token the model gets: the same App and repository, with
// only read permissions, so the model can read GitHub but never write to it.
func (d Deps) mintReadOnlyToken(ctx context.Context, role workflow.Role, repo workflow.Repo) (string, error) {
	return d.mint(ctx, role, repo, true)
}

// mint runs the helper for one token.
func (d Deps) mint(ctx context.Context, role workflow.Role, repo workflow.Repo, readOnly bool) (string, error) {
	// stderr is inherited so the helper's error reaches the dagu log; it never prints the token there.
	out, err := d.run(ctx, proc.Cmd{
		Argv:    append([]string{d.Settings.TokenHelper}, tokenArgs(role, repo, readOnly)...),
		Capture: true, Timeout: MintTimeout,
	})
	if err != nil {
		return "", fmt.Errorf("mint %s token for %s: %w", role, repo, err)
	}

	return strings.TrimSpace(out), nil
}

// installedRepos lists the repositories the agent role's App is installed on. An App that can't
// list its installations is installed nowhere: the failure is logged and the repositories stay unassigned.
func (d Deps) installedRepos(ctx context.Context, role workflow.Role) []workflow.Repo {
	out, err := d.run(ctx, proc.Cmd{
		Argv:    []string{d.Settings.TokenHelper, "repositories", "--app", string(role)},
		Capture: true, Timeout: MintTimeout,
	})
	if err != nil {
		d.log().Warn("cannot list installations; treating the App as installed nowhere", "role", role, "error", err)
		return nil
	}

	var repos []workflow.Repo

	for line := range strings.SplitSeq(out, "\n") {
		// A line that is not owner/name is skipped: the helper may print blank or unrelated lines.
		if repo, err := workflow.ParseRepo(strings.TrimSpace(line)); err == nil {
			repos = append(repos, repo)
		}
	}

	return repos
}

// Assignment says which agent roles' Apps are installed on each assigned repository.
type Assignment map[workflow.Repo][]workflow.Role

// Assigned asks each App where it is installed. A repository with any App installed is assigned.
func Assigned(ctx context.Context, d Deps, roles []workflow.Role) Assignment {
	a := Assignment{}

	for _, role := range roles {
		for _, repo := range d.installedRepos(ctx, role) {
			if !slices.Contains(a[repo], role) {
				a[repo] = append(a[repo], role)
			}
		}
	}

	return a
}

// Repos lists the assigned repositories in a stable order, at most MaxAssignedRepos of them.
func (a Assignment) Repos() []workflow.Repo {
	repos := slices.SortedFunc(maps.Keys(a), func(x, y workflow.Repo) int {
		return strings.Compare(x.String(), y.String())
	})
	if len(repos) > MaxAssignedRepos {
		repos = repos[:MaxAssignedRepos]
	}

	return repos
}

// queueStep is one queue a poll looks at, in priority order.
type queueStep struct {
	src, dst workflow.State
	role     workflow.Role
}

// pollQueues puts reviews first: they are short, and they unblock the pull requests already waiting.
func pollQueues() []queueStep {
	queues := workflow.Queues()
	out := make([]queueStep, 0, len(queues))

	for _, q := range queues {
		out = append(out, queueStep{src: q.State, dst: q.Running, role: q.Role})
	}

	return out
}

// PollRunLimit bounds how many dagu runs a poll reads to know which agent roles are busy.
const PollRunLimit = 1000

// queuedIssue is one claimable issue, with the repository and client that found it.
type queuedIssue struct {
	repo   workflow.Repo
	issue  workflow.Issue
	client github.Client
}

// firstIssue reports whether a comes before b: the oldest creation first, an undated issue after
// every dated one, then the repository name and the issue number.
func firstIssue(a, b queuedIssue) bool {
	if !a.issue.CreatedAt.Equal(b.issue.CreatedAt) {
		if a.issue.CreatedAt.IsZero() {
			return false
		}

		if b.issue.CreatedAt.IsZero() {
			return true
		}

		return a.issue.CreatedAt.Before(b.issue.CreatedAt)
	}

	if a.repo != b.repo { // Repo is comparable but unordered: order by name
		return a.repo.String() < b.repo.String()
	}

	return a.issue.Number < b.issue.Number
}

// Poll is deterministic and cheap: no model involved. It first brings every assigned repository's
// labels, routing, waking and recovery up to date, and calls any dead run failed, then claims at most
// one issue and queues its run. It looks only at repositories where one of the roles' Apps is
// installed, and claims for an agent role only when that role's DAG has nothing waiting in dagu, in
// scheduling order. Every claim uses its own agent role's token.
func Poll(ctx context.Context, d Deps, cfg config.Config) (int, error) {
	assigned := Assigned(ctx, d, cfg.Roles)

	repos := assigned.Repos()
	perms := newPermissions()

	// Reading dagu first means an agent role whose DAG already has a run waiting or running is not
	// queued for again, the Curator included.
	busy, err := busyRoles(ctx, d, d.Settings.DAGPrefix)
	if err != nil {
		return 1, err
	}

	for _, repo := range repos {
		if err := prepareRepo(ctx, d, cfg, perms, repo, assigned[repo], busy); err != nil {
			return 1, err
		}
	}

	// An agent role no worker host takes leaves its issues queued. One warning per poll, and nothing
	// fails: the queue stays visible and pausable until a host takes the agent role.
	warnUnhosted(d, cfg)

	for _, q := range pollQueues() {
		if cfg.Unhosted(q.role) {
			continue
		}

		// An agent role whose DAG has a run waiting or running is not claimed for, so a running label
		// always means a run is happening.
		if busy[q.role] {
			d.log().Info("leaving an agent role's queue while its DAG is busy", "role", q.role)

			continue
		}

		code, done, err := claimFromQueue(ctx, d, cfg, perms, assigned, repos, q)
		if done || err != nil {
			return code, err
		}
	}

	return 0, writeLine(d.Out, "nothing ready")
}

// warnUnhosted logs one warning for every agent role no worker host takes. It fails nothing: the
// agent role's issues stay in their queued label until a host takes the agent role.
func warnUnhosted(d Deps, cfg config.Config) {
	for _, role := range workflow.KnownRoles() {
		if cfg.Unhosted(role) {
			d.log().Warn("no worker host takes this agent role; its issues stay queued", "role", role)
		}
	}
}

// busyRoles reads dagu's active runs, read-only, and names the agent roles whose DAG dagu can still
// move. A dagu read that fails is an error: the poll must never claim while it cannot tell.
func busyRoles(ctx context.Context, d Deps, prefix string) (map[workflow.Role]bool, error) {
	out, err := d.run(ctx, proc.Cmd{
		Argv: dagu.ActiveHistoryArgv(d.Settings.Dagu, PollRunLimit), Capture: true, Timeout: DaguTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("read the busy agent roles from dagu: %w", err)
	}

	runs, err := dagu.ParseHistory([]byte(out))
	if err != nil {
		return nil, err
	}

	busy := map[workflow.Role]bool{}

	for _, run := range runs {
		name, ok := strings.CutPrefix(run.Name, prefix)
		if !ok || !run.Active() {
			continue
		}

		role, err := workflow.ParseRole(name)
		if err != nil {
			continue // another DAG that happens to share the prefix
		}

		busy[role] = true
	}

	return busy, nil
}

// pollingRole is the App that places new issues in their first queue: the first installed agent role
// in scheduling order. Polling is read-only otherwise, so any installed App may do it; a repository
// with only the Curator installed still gets its dashboard polled.
func pollingRole(roles []workflow.Role) (workflow.Role, bool) {
	for _, q := range workflow.Queues() {
		if slices.Contains(roles, q.Role) {
			return q.Role, true
		}
	}

	if slices.Contains(roles, workflow.Curator) {
		return workflow.Curator, true
	}

	return "", false
}

// prepareRepo creates the factory labels, calls dead runs failed, repairs stranded issues from their
// latest marker, wakes an issue a writer has commented on, and routes new issues to their first
// queue. Reads use the polling App's token; a write uses the token of the agent role that owns it.
func prepareRepo(
	ctx context.Context, d Deps, cfg config.Config, perms *permissions, repo workflow.Repo, roles []workflow.Role,
	busy map[workflow.Role]bool,
) error {
	role, ok := pollingRole(roles)
	if !ok {
		return nil
	}

	token, err := d.mintToken(ctx, role, repo)
	if err != nil {
		d.log().Warn("cannot mint token; treating the repository as unassigned",
			"role", role, "repo", repo, "error", err)

		return nil
	}

	c := d.client(token)
	if err := c.EnsureLabels(ctx, repo, workflow.FactoryLabels()); err != nil {
		return err
	}

	// The dashboard is best effort: an issue cannot be handed off without the work below it, but a
	// retro dashboard that cannot be drawn is retried on the next poll.
	if err := redrawDashboard(ctx, d, cfg, c, perms, repo, roles, busy, role); err != nil {
		d.log().Warn("cannot redraw the retro dashboard", "repo", repo, "error", err)
	}

	issues, err := c.ListIssues(ctx, repo, github.StateOpen, github.Oldest)
	if err != nil {
		return err
	}

	for _, issue := range issues {
		if issue.IsPullRequest || workflow.HasLabel(issue, workflow.LabelPaused) {
			continue
		}

		if err := settleIssue(ctx, d, cfg, c, perms, repo, issue); err != nil {
			// One unreadable issue fails closed for that issue; the poll carries on.
			d.log().Warn("skipping an issue the poll cannot read", "repo", repo, "issue", issue.Number, "error", err)
		}
	}

	return nil
}

// settleIssue calls a dead run failed, then repairs, wakes or routes the issue.
func settleIssue(
	ctx context.Context, d Deps, cfg config.Config, c github.Client, perms *permissions,
	repo workflow.Repo, issue workflow.Issue,
) error {
	dead, err := failDeadRun(ctx, d, cfg, c, repo, issue)
	if err != nil || dead {
		return err
	}

	return prepareIssue(ctx, d, c, perms, repo, issue)
}

// failDeadRun moves an issue whose running label outlived its agent role's deadline plus the dead-run
// margin to factory:failed, once, with a marker. A missing deadline or timeline event leaves the
// issue alone: an age it cannot read is never an age that fails a run.
func failDeadRun(
	ctx context.Context, d Deps, cfg config.Config, c github.Client, repo workflow.Repo, issue workflow.Issue,
) (bool, error) {
	role, state, ok := workflow.RunningState(issue)
	if !ok {
		return false, nil
	}

	agent, ok := cfg.Agents[string(role)]
	if !ok || agent.Deadline() <= 0 {
		d.log().Warn("cannot date a running label without its agent role's deadline",
			"repo", repo, "issue", issue.Number, "role", role)

		return false, nil
	}

	events, err := c.Timeline(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number})
	if err != nil {
		return false, fmt.Errorf("read the timeline of #%d: %w", issue.Number, err)
	}

	since, found := workflow.LabeledAt(events, string(state))
	if !found {
		d.log().Warn("the timeline has no labeled event for the running label",
			"repo", repo, "issue", issue.Number, "state", state)

		return false, nil
	}

	margin := cfg.DeadRunMargin()
	if !workflow.DeadRun(since, d.now(), agent.Deadline(), margin) {
		return false, nil
	}

	token, err := d.mintToken(ctx, role, repo)
	if err != nil {
		d.log().Warn("cannot mint the dead run's own token; leaving the issue claimed",
			"role", role, "repo", repo, "issue", issue.Number, "error", err)

		return false, nil
	}

	d.log().Info("failing a dead run", "repo", repo, "issue", issue.Number, "role", role,
		"since", since, "deadline", agent.Deadline(), "margin", margin)

	t := workflow.Transition{Role: role, RunID: d.runID(), From: state, To: workflow.Failed}
	text := fmt.Sprintf("The %s run outlived its %s deadline plus the %s dead-run margin. "+
		"It is not retried automatically; comment to send it back to the %s.",
		role, agent.Deadline(), margin, role)

	_, err = d.client(token).Finish(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number}, t, text)

	return true, err
}

// prepareIssue repairs, wakes and routes one issue. A read failure is returned, so the issue is left alone.
func prepareIssue(
	ctx context.Context, d Deps, c github.Client, perms *permissions, repo workflow.Repo, issue workflow.Issue,
) error {
	comments, err := issueComments(ctx, c, repo, issue)
	if err != nil {
		return err
	}

	latest, found, err := workflow.LatestMarker(comments)
	if err != nil {
		return fmt.Errorf("read the latest marker on #%d: %w", issue.Number, err)
	}

	if state, repair := workflow.Repair(issue, latest.Transition, found); repair {
		d.log().Info("repairing a stranded issue", "repo", repo, "issue", issue.Number, "state", state)

		if err := c.AddLabel(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number}, string(state)); err != nil {
			return err
		}

		issue.Labels = append(issue.Labels, string(state))
	}

	woke, err := wakeIssue(ctx, d, perms, c, repo, issue, comments, latest, found)
	if err != nil || woke {
		return err
	}

	state, ok := workflow.Route(issue)
	if !ok {
		return nil
	}

	if !counted(ctx, d, c, perms, repo, issue, comments) {
		return nil
	}

	if err := c.AddLabel(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number}, string(state)); err != nil {
		return err
	}

	return nil
}

// wakeIssue moves an issue a writer has commented on out of the state that was waiting for them: a
// waiting issue goes back to the Planner, a done one back to the Implementer, and a failed one back
// to the agent role that failed. A done issue is woken by a comment on its pull request; the others
// by a comment on the issue. The wake is made with the token of the agent role it queues for.
func wakeIssue(
	ctx context.Context, d Deps, perms *permissions, c github.Client, repo workflow.Repo, issue workflow.Issue,
	comments []workflow.Item, latest workflow.Marker, found bool,
) (bool, error) {
	if !found {
		return false, nil
	}

	thread, ok, err := wakeThread(ctx, c, repo, issue, comments, latest)
	if err != nil || !ok {
		return false, err
	}

	// A writer's approval of the latest final round applies the stored proposal with no model run.
	approved, err := approvePending(ctx, d, perms, c, repo, issue, thread, latest)
	if err != nil || approved {
		return approved, err
	}

	state, ok := workflow.WakeState(latest.Transition, true, written(ctx, d, perms, c, repo, thread, latest))
	if !ok {
		return false, nil
	}

	role, ok := workflow.RoleOf(state)
	if !ok {
		return false, nil
	}

	token, err := d.mintToken(ctx, role, repo)
	if err != nil {
		d.log().Warn("cannot mint the waking agent role's token; leaving the issue where it is",
			"role", role, "repo", repo, "issue", issue.Number, "error", err)

		return false, nil
	}

	t := workflow.Transition{Role: role, RunID: d.runID(), From: latest.To, To: state}

	won, err := d.client(token).Claim(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number}, t)
	if err != nil {
		return false, err
	}

	d.log().Info("woke an issue on a writer's comment",
		"repo", repo, "issue", issue.Number, "state", state, "won", won)

	return true, nil
}

// wakeThread is the thread a wake is judged on: the pull request's comments for a done issue, the
// issue's own otherwise. ok is false when a done issue has no open pull request to comment on.
func wakeThread(
	ctx context.Context, c github.Client, repo workflow.Repo, issue workflow.Issue,
	comments []workflow.Item, latest workflow.Marker,
) ([]workflow.Item, bool, error) {
	if latest.To != workflow.Done {
		return comments, true, nil
	}

	pr, err := c.ExistingPR(ctx, repo, workflow.BranchName(issue.Number))
	if err != nil {
		return nil, false, err
	}

	if pr == nil {
		return nil, false, nil
	}

	thread, err := c.ListComments(ctx, workflow.IssueRef{Repo: repo, Number: pr.Number})

	return thread, err == nil, err
}

// written reports whether a writer commented after the latest marker.
func written(
	ctx context.Context, d Deps, perms *permissions, c github.Client, repo workflow.Repo,
	thread []workflow.Item, latest workflow.Marker,
) bool {
	for _, comment := range workflow.AfterMarker(thread, latest) {
		if perms.isWriter(ctx, d, c, repo, comment.Author) {
			return true
		}
	}

	return false
}

// issueComments lists an issue's comments, skipping the call when it has none.
func issueComments(
	ctx context.Context, c github.Client, repo workflow.Repo, issue workflow.Issue,
) ([]workflow.Item, error) {
	if issue.Comments == 0 {
		return nil, nil
	}

	return c.ListComments(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number})
}

// permissions caches, for one poll, whether each login may write to the repository. A permission it
// cannot read is cached as not-a-writer, so every decision fails closed.
type permissions struct {
	write map[string]bool
}

// newPermissions starts an empty permission cache.
func newPermissions() *permissions {
	return &permissions{write: map[string]bool{}}
}

// isWriter reports whether login has write access. An unreadable permission is not write access.
func (p *permissions) isWriter(ctx context.Context, d Deps, c github.Client, repo workflow.Repo, login string) bool {
	if login == "" {
		return false
	}

	key := repo.String() + "\x00" + login
	if writer, ok := p.write[key]; ok {
		return writer
	}

	permission, err := c.CollaboratorPermission(ctx, repo, login)
	if err != nil {
		d.log().Warn("cannot read a collaborator's permission; failing closed",
			"repo", repo, "login", login, "error", err)

		p.write[key] = false

		return false
	}

	p.write[key] = workflow.WriterPermission(permission)

	return p.write[key]
}

// counted reports whether an issue counts: its author is a writer, or a writer has commented on it.
func counted(
	ctx context.Context, d Deps, c github.Client, perms *permissions, repo workflow.Repo,
	issue workflow.Issue, comments []workflow.Item,
) bool {
	if perms.isWriter(ctx, d, c, repo, issue.Author) {
		return true
	}

	for _, comment := range comments {
		if perms.isWriter(ctx, d, c, repo, comment.Author) {
			return true
		}
	}

	return false
}

// blocked reports whether an issue has an open blocker. Native dependencies come first; a repository
// without them falls back to the issue's "Blocked by:" line. A read failure is an error, so the
// caller never claims an issue whose blockers it cannot read.
func blocked(ctx context.Context, c github.Client, repo workflow.Repo, issue workflow.Issue) (bool, error) {
	open, supported, err := c.OpenBlockers(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number})
	if err != nil {
		return false, err
	}

	if supported {
		return len(open) > 0, nil
	}

	for _, number := range workflow.BlockedBy(issue.Body) {
		blocker, err := c.GetIssue(ctx, workflow.IssueRef{Repo: repo, Number: number})
		if err != nil {
			return false, fmt.Errorf("read blocker #%d: %w", number, err)
		}

		if blocker.State != "closed" {
			return true, nil
		}
	}

	return false, nil
}

// claimFromQueue gathers every eligible issue in one agent role's queue across the repositories where
// its App is installed, then claims the oldest one. A repository whose token cannot be minted is
// unassigned for this agent role. done is true when the poll is over: an issue was claimed or lost,
// or the CI gate moved one.
func claimFromQueue(
	ctx context.Context, d Deps, cfg config.Config, perms *permissions, assigned Assignment,
	repos []workflow.Repo, q queueStep,
) (code int, done bool, err error) {
	candidates := make([]queuedIssue, 0)

	for _, repo := range repos {
		if !slices.Contains(assigned[repo], q.role) {
			continue // that agent role isn't here: its issues wait in the queue
		}

		token, err := d.mintToken(ctx, q.role, repo)
		if err != nil {
			d.log().Warn("cannot mint token; treating the repository as unassigned",
				"role", q.role, "repo", repo, "error", err)

			continue
		}

		c := d.client(token)

		issues, err := c.ListLabeled(ctx, repo, string(q.src))
		if err != nil {
			return 1, true, err
		}

		for _, issue := range issues {
			if eligible(ctx, d, c, perms, repo, issue) {
				candidates = append(candidates, queuedIssue{repo: repo, issue: issue, client: c})
			}
		}
	}

	return claimOldest(ctx, d, cfg, q, candidates)
}

// claimOldest claims the oldest candidate in one agent role's queue. The Reviewer's CI gate runs
// first: pending CI leaves that issue queued for a later candidate, while an issue the gate moved ends
// the poll with no model run.
func claimOldest(
	ctx context.Context, d Deps, cfg config.Config, q queueStep, candidates []queuedIssue,
) (code int, done bool, err error) {
	slices.SortStableFunc(candidates, compareIssues)

	for _, candidate := range candidates {
		if q.role != workflow.Reviewer {
			code, err := claimAndEnqueue(ctx, d, candidate.client, candidate.repo, candidate.issue, q)

			return code, true, err
		}

		claim, moved, gateErr := ciGate(ctx, d, cfg, candidate.client, candidate.repo, candidate.issue)
		if gateErr != nil {
			d.log().Warn("skipping an issue whose CI cannot be read",
				"repo", candidate.repo, "issue", candidate.issue.Number, "error", gateErr)

			continue
		}

		if moved {
			return 0, true, nil
		}

		if claim {
			code, err := claimAndEnqueue(ctx, d, candidate.client, candidate.repo, candidate.issue, q)

			return code, true, err
		}
		// Pending CI leaves the issue queued; a later issue may still be claimable.
	}

	return 0, false, nil
}

// compareIssues orders two candidates for sorting: firstIssue's order as a three-way comparison.
func compareIssues(a, b queuedIssue) int {
	switch {
	case firstIssue(a, b):
		return -1
	case firstIssue(b, a):
		return 1
	}

	return 0
}

// eligible reports whether the poll may work an issue at all: not a pull request, not paused, not
// already claimed, counted, and unblocked. A read it cannot make leaves the issue alone, never claims it.
func eligible(
	ctx context.Context, d Deps, c github.Client, perms *permissions, repo workflow.Repo, issue workflow.Issue,
) bool {
	if issue.IsPullRequest || workflow.HasLabel(issue, workflow.LabelPaused) || claimed(issue) {
		return false
	}

	comments, err := issueComments(ctx, c, repo, issue)
	if err != nil {
		d.log().Warn("skipping an issue whose comments cannot be read",
			"repo", repo, "issue", issue.Number, "error", err)

		return false
	}

	if !counted(ctx, d, c, perms, repo, issue, comments) {
		return false
	}

	isBlocked, err := blocked(ctx, c, repo, issue)
	if err != nil {
		d.log().Warn("skipping an issue whose blockers cannot be read",
			"repo", repo, "issue", issue.Number, "error", err)

		return false
	}

	return !isBlocked
}

// readCI reads what GitHub reports about sha: its check runs, its commit statuses, and the tail of
// each failed Actions job's log. A log that cannot be read is logged and left out; the failure
// itself still reaches the Implementer.
func readCI(ctx context.Context, d Deps, c github.Client, repo workflow.Repo, sha string) (workflow.CIReport, error) {
	report := workflow.CIReport{SHA: sha}

	checks, err := c.CheckRuns(ctx, repo, sha)
	if err != nil {
		return report, fmt.Errorf("read check runs: %w", err)
	}

	statuses, err := c.CommitStatuses(ctx, repo, sha)
	if err != nil {
		return report, fmt.Errorf("read commit statuses: %w", err)
	}

	report.Checks, report.Statuses = checks, statuses

	for _, check := range workflow.FailedChecks(report) {
		if check.ID == 0 || check.App.Slug != "github-actions" {
			continue // only an Actions check run has an Actions job log
		}

		log, err := c.JobLog(ctx, repo, check.ID)
		if err != nil {
			d.log().Warn("cannot read a failed job's log; reporting the check without it",
				"repo", repo, "check", check.Name, "error", err)

			continue
		}

		report.Logs = append(report.Logs, workflow.JobLog{
			Name: check.Name, Tail: workflow.Tail(log, workflow.JobLogTailChars),
		})
	}

	return report, nil
}

// ciGate decides what the review queue does with an issue: claim it, leave it queued while CI is
// pending, send it back to the Implementer on failed CI, or fail it when pending CI outlived its
// deadline. Neither of the latter starts a model run. The reads and writes use the Reviewer's client,
// so every transition is made by the agent role that owns the review queue.
func ciGate(
	ctx context.Context, d Deps, cfg config.Config, c github.Client, repo workflow.Repo, issue workflow.Issue,
) (claim, moved bool, err error) {
	pr, err := c.ExistingPR(ctx, repo, workflow.BranchName(issue.Number))
	if err != nil {
		return false, false, err
	}

	if pr == nil {
		return true, false, nil // there is nothing to gate; the run fails on its own
	}

	report, err := readCI(ctx, d, c, repo, pr.Head.SHA)
	if err != nil {
		return false, false, err
	}

	state := workflow.Classify(report)
	expired := false

	if state == workflow.CIPending {
		expired, err = ciPendingExpired(ctx, d, cfg, c, repo, issue)
		if err != nil {
			return false, false, err
		}
	}

	switch workflow.DecideCI(state, expired) {
	case workflow.CIClaim:
		return true, false, nil
	case workflow.CIWait:
		d.log().Info("leaving an issue queued while its CI is pending",
			"repo", repo, "issue", issue.Number, "sha", report.SHA)

		return false, false, nil
	case workflow.CIFail:
		d.log().Info("failing an issue whose CI stayed pending past its deadline",
			"repo", repo, "issue", issue.Number, "sha", report.SHA)

		return false, true, ciPendingFailure(ctx, d, cfg, c, repo, issue, report)
	case workflow.CIBack:
		return false, true, ciFailureBack(ctx, d, cfg, c, repo, issue, pr.Number, report)
	}

	return false, false, nil
}

// ciPendingExpired reports whether the issue has waited in factory:review past the CI pending
// deadline. An age it cannot date leaves the issue queued, like a run it cannot date is not dead.
func ciPendingExpired(
	ctx context.Context, d Deps, cfg config.Config, c github.Client, repo workflow.Repo, issue workflow.Issue,
) (bool, error) {
	events, err := c.Timeline(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number})
	if err != nil {
		return false, err
	}

	since, found := workflow.LabeledAt(events, workflow.LabelReview)
	if !found {
		d.log().Warn("cannot date pending CI without the review label's timeline event",
			"repo", repo, "issue", issue.Number)

		return false, nil
	}

	return workflow.PendingExpired(since, d.now(), cfg.CIPendingDeadline()), nil
}

// ciPendingFailure fails an issue whose CI stayed pending past its deadline.
func ciPendingFailure(
	ctx context.Context, d Deps, cfg config.Config, c github.Client, repo workflow.Repo,
	issue workflow.Issue, report workflow.CIReport,
) error {
	t := workflow.Transition{
		Role: workflow.Reviewer, RunID: d.runID(), From: workflow.Review, To: workflow.Failed, SHA: report.SHA,
	}
	text := fmt.Sprintf("CI was still pending after the %s CI pending deadline. "+
		"A person needs to look at the pull request; comment to send it back to the Reviewer.",
		cfg.CIPendingDeadline())

	_, err := c.Finish(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number}, t, text)

	return err
}

// ciFailureBack reports failed CI on the issue, then sends the issue back to the Implementer with no
// model run. At the review round limit it fails the issue instead. The report comes before the
// transition, so the Implementer's run reads it as the reason it was sent back.
func ciFailureBack(
	ctx context.Context, d Deps, cfg config.Config, c github.Client, repo workflow.Repo,
	issue workflow.Issue, pr int, report workflow.CIReport,
) error {
	reviews, err := c.Reviews(ctx, workflow.IssueRef{Repo: repo, Number: pr})
	if err != nil {
		return err
	}

	comments, err := c.ListComments(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number})
	if err != nil {
		return err
	}

	rounds := workflow.ReviewRounds(reviews, comments, report.SHA)

	dst := workflow.Implement
	text := "CI failed on the pull request. Sent back to the Implementer with the failure report above; " +
		"no model run was needed."

	if workflow.RoundLimitReached(rounds, cfg.ReviewRoundLimit) {
		dst = workflow.Failed
		text = fmt.Sprintf("CI failed and this is review round %d of %d. "+
			"A person needs to look at the pull request.", rounds, cfg.ReviewRoundLimit)
	}

	// The report is keyed by the commit, so a retried poll never posts it twice.
	if !workflow.HasCIFailure(comments, report.SHA) {
		ref := workflow.IssueRef{Repo: repo, Number: issue.Number}

		if err := c.Comment(ctx, ref, workflow.RenderCIFailure(report)); err != nil {
			return err
		}
	}

	t := workflow.Transition{
		Role: workflow.Reviewer, RunID: d.runID(), From: workflow.Review, To: dst, SHA: report.SHA,
	}

	_, err = c.Finish(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number}, t, text)

	return err
}

// claimed reports whether the issue already carries a running label: a run is happening.
func claimed(issue workflow.Issue) bool {
	_, _, running := workflow.RunningState(issue)

	return running
}

// headSHA is the commit the issue's open pull request is at, or empty before one exists.
func headSHA(ctx context.Context, c github.Client, ref workflow.IssueRef) (string, error) {
	pr, err := c.ExistingPR(ctx, ref.Repo, workflow.BranchName(ref.Number))
	if err != nil {
		return "", err
	}

	if pr == nil {
		return "", nil
	}

	return pr.Head.SHA, nil
}

// claimAndEnqueue claims the issue, then queues its run. A queue failure fails the issue rather than
// leaving it claimed.
func claimAndEnqueue(
	ctx context.Context, d Deps, c github.Client, repo workflow.Repo, issue workflow.Issue, q queueStep,
) (int, error) {
	sha, err := headSHA(ctx, c, workflow.IssueRef{Repo: repo, Number: issue.Number})
	if err != nil {
		return 1, err
	}

	t := workflow.Transition{Role: q.role, RunID: d.runID(), From: q.src, To: q.dst, SHA: sha}

	won, err := c.Claim(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number}, t)
	if err != nil {
		return 1, err
	}

	if !won {
		return 0, writeLine(d.Out, fmt.Sprintf("issue %d was claimed by someone else", issue.Number))
	}

	_, err = d.run(ctx, proc.Cmd{Argv: []string{
		d.Settings.Dagu, "enqueue", d.Settings.DAGPrefix + string(q.role), "--",
		"REPO=" + repo.String(), "ISSUE=" + strconv.Itoa(issue.Number),
	}})
	if err != nil {
		d.log().Error("enqueue failed", "role", q.role, "repo", repo, "issue", issue.Number, "error", err)

		t.From, t.To = q.dst, workflow.Failed

		ref := workflow.IssueRef{Repo: repo, Number: issue.Number}

		_, err = c.Finish(ctx, ref, t, "Could not queue the agent run; see the dagu log.")

		return 1, err
	}

	return 0, nil
}

// Start runs on the worker: it hands the factory user a token, runs the agent role, and records the outcome.
func Start(ctx context.Context, d Deps, cfg config.Config, role workflow.Role, ref workflow.IssueRef) (int, error) {
	ar, ok := agentRoleFor(role)
	if !ok {
		return 1, fmt.Errorf("unknown agent role %q", role)
	}

	r := ar.route(cfg, ref, factoryExit(ctx, d, cfg, role, ref))

	switch {
	case r.err != nil:
		return r.exit, r.err
	case r.dst == "":
		return r.exit, writeLine(d.Out, r.text)
	}

	q, ok := workflow.QueueOf(role)
	if !ok {
		return 1, fmt.Errorf("agent role %q has no queue to finish", role)
	}

	return r.exit, finishRun(ctx, d, role, ref, q.Running, r.dst, r.text)
}

// failureText is what start says about a failed run. A factory half that already posted its errors in
// one comment needs no second description.
func failureText(role workflow.Role, code int) string {
	if code == workflow.ExitReported {
		return ""
	}

	name := strings.ToUpper(string(role)[:1]) + string(role)[1:]

	return fmt.Sprintf("The %s failed. Check the dagu run log, then comment to send it back to the %s.",
		name, name)
}

// finishRun records a run's outcome. A closed destination closes the ticket instead of labelling it; a
// mint failure always falls back to factory:failed, reported by an App that is still installed.
func finishRun(
	ctx context.Context, d Deps, role workflow.Role, ref workflow.IssueRef, src, dst workflow.State, text string,
) error {
	t := workflow.Transition{Role: role, RunID: d.runID(), From: src, To: dst}

	token, err := d.mintToken(ctx, role, ref.Repo)
	if err == nil {
		c := d.client(token)
		if dst == workflow.Closed {
			return c.CloseResolved(ctx, ref, t)
		}

		// The commit is recorded when it can be read; the transition itself is what matters.
		if sha, shaErr := headSHA(ctx, c, ref); shaErr == nil {
			t.SHA = sha
		} else {
			d.log().Warn("cannot read the pull request head for the transition marker",
				"repo", ref.Repo, "issue", ref.Number, "error", shaErr)
		}

		_, err := c.Finish(ctx, ref, t, text)

		return err
	}

	d.log().Error("cannot mint the owning App's token; reporting the failure as another App",
		"role", role, "repo", ref.Repo, "issue", ref.Number, "error", err)

	t.To = workflow.Failed

	notice := fmt.Sprintf("The %s's GitHub App credential is missing or the App is no longer installed here. "+
		"Fix that, then requeue the issue.", role)
	for _, other := range workflow.KnownRoles() {
		if other == role {
			continue
		}

		otherToken, mintErr := d.mintToken(ctx, other, ref.Repo)
		if mintErr != nil {
			continue // that App isn't installed here or has no credential: try the next
		}

		_, err := d.client(otherToken).Finish(ctx, ref, t, notice)

		return err
	}

	return fmt.Errorf("no installed App can report the failure on %s#%d: %w", ref.Repo, ref.Number, err)
}

// factoryExit is the exit code of `foreman run <agent role> <repo> <n>` as the factory user, with the
// token file
// in place. It is -1 when the run could not be started: that is logged, and the issue must not stay claimed.
func factoryExit(ctx context.Context, d Deps, cfg config.Config, role workflow.Role, ref workflow.IssueRef) int {
	code, err := runAsFactory(ctx, d, cfg, role, ref)
	if err != nil {
		d.log().Error("run as factory failed", "role", role, "issue", ref, "error", err)
		return -1
	}

	return code
}

// runAsFactory writes the token where the factory user can read it, runs the factory half, and removes
// the token. The supervisor gets the agent role's deadline plus its retro's, so foreman's own
// enforcement fires first and this ceiling only catches a factory half that never returns.
func runAsFactory(
	ctx context.Context, d Deps, cfg config.Config, role workflow.Role, ref workflow.IssueRef,
) (code int, err error) {
	repo, number := ref.Repo, ref.Number

	token, err := d.mintToken(ctx, role, repo)
	if err != nil {
		return 0, err
	}

	path := d.Settings.TokenPath(role)
	if err := writeTokenFile(path, token); err != nil {
		return 0, err
	}

	defer func() {
		// The token also dies within the hour; don't leave it lying around.
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
	}()

	ceiling := cfg.Agents[string(role)].Deadline() + cfg.RetroBudget(role).Deadline() + supervisorGitRuns*GitTimeout

	_, runErr := d.Runner.Run(ctx, proc.Cmd{
		Argv: []string{
			"sudo", "-n", "-u", "factory", d.Settings.Self, "run", string(role), repo.String(), strconv.Itoa(number),
		},
		Timeout: ceiling,
	})

	var ee *proc.ExitError
	switch {
	case runErr == nil:
		return 0, nil
	case errors.As(runErr, &ee):
		return ee.Code, nil
	}

	return 0, runErr
}

// writeTokenFile leaves the token readable by the factory group, set by the directory's setgid bit.
func writeTokenFile(path, token string) error {
	// The path is the edge-configured token directory joined with a parsed agent role, never input.
	// The mode is group-readable on purpose: the factory half runs as another user in the factory
	// group.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, tokenFileMode) //nolint:gosec // see above
	if err != nil {
		return fmt.Errorf("write token file: %w", err)
	}

	if _, err := f.WriteString(token); err != nil {
		return errors.Join(fmt.Errorf("write token file: %w", err), f.Close())
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}

	// Chmod again: OpenFile's mode is masked by the umask and ignored for an existing file.
	if err := os.Chmod(path, tokenFileMode); err != nil { //nolint:gosec // group-readable on purpose, as above
		return fmt.Errorf("write token file: %w", err)
	}

	return nil
}
