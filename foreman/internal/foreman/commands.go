package foreman

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/dagu"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/telemetry"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// MaxAssignedRepos is how many assigned repositories one poll looks at. More is an error: a poll
// never leaves an assigned repository unpolled without saying so.
const MaxAssignedRepos = 200

// supervisorGitRuns is how many git ceilings the supervisor adds around a run: the clone and the push.
const supervisorGitRuns = 2

// The two tokens mint asks the helper for: the writable one foreman writes with, and the read-only
// one the model gets.
const (
	writableToken = false
	readOnlyToken = true
)

// mint asks the helper for one token of an agent role: its own App, scoped to the repository. A
// read-only token is the same App and repository with only read permissions, so the model can read
// GitHub but never write to it. There is no fallback: if this App can't mint, no other App's token
// stands in for it.
func (d Deps) mint(ctx context.Context, role workflow.Role, repo workflow.Repo, readOnly bool) (string, error) {
	args := []string{d.Settings.TokenHelper, "token", "--app", string(role), "--repo", repo.String()}
	if readOnly {
		args = append(args, "--read-only")
	}

	// stderr is inherited so the helper's error reaches the dagu log; it never prints the token there.
	out, err := d.run(ctx, proc.Cmd{Argv: args, Capture: true, Timeout: MintTimeout})
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
		d.log().Warn("cannot list installations; treating the App as installed nowhere", "agent_role", role, "error", err)
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

// Repos lists the assigned repositories in a stable order. More than MaxAssignedRepos is an error:
// nothing is skipped, so a poll that would not look at every assigned repository fails instead.
func (a Assignment) Repos() ([]workflow.Repo, error) {
	repos := slices.SortedFunc(maps.Keys(a), func(x, y workflow.Repo) int {
		return strings.Compare(x.String(), y.String())
	})
	if len(repos) > MaxAssignedRepos {
		return nil, fmt.Errorf("%d repositories are assigned, over the %d a poll looks at",
			len(repos), MaxAssignedRepos)
	}

	return repos, nil
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

// PollRunLimit bounds how many dagu runs a poll reads to know how many runs an agent role has.
const PollRunLimit = 1000

// runState is dagu's active runs counted by agent role: how many runs dagu can still move, and
// whether one of them is waiting for a worker.
type runState struct {
	// active is how many runs the agent role's DAG has that dagu can still move.
	active map[workflow.Role]int
	// waiting is whether one of those runs is waiting for a worker.
	waiting map[workflow.Role]bool
}

// free is how many more runs the agent role may start: its fleet-wide capacity less the runs dagu
// can still move. It is never negative.
func (s runState) free(role workflow.Role, capacity int) int {
	return max(0, capacity-s.active[role])
}

// queuedIssue is one claimable issue, with the repository, client and repository span that found it.
type queuedIssue struct {
	ctx    context.Context
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
// labels, routing, waking and recovery up to date, and calls any dead run failed, then claims issues
// and queues their runs. It looks only at repositories where one of the roles' Apps is installed, and
// works the queues in scheduling order. An agent role whose DAG has a run waiting for a worker is not
// claimed for; one whose runs are all in progress is claimed for while its fleet-wide run capacity
// has room, so one poll fills a run cap above one. Every claim uses its own agent role's token.
func Poll(ctx context.Context, d Deps, cfg config.Config) (int, error) {
	assigned := Assigned(ctx, d, cfg.Roles)

	repos, err := assigned.Repos()
	if err != nil {
		return 1, err
	}

	perms := newPermissions()

	// Reading dagu first means an agent role whose DAG already has a run waiting for a worker is not
	// queued for again, the Curator included.
	runs, err := readRuns(ctx, d, d.Settings.DAGPrefix)
	if err != nil {
		return 1, err
	}

	inst := newPollInstruments(d)

	// Capacity is what the poll finds after reading dagu, before this poll's own claims.
	for _, role := range workflow.KnownRoles() {
		inst.capacityLeft(ctx, role, runs.free(role, cfg.Capacity(role)))
	}

	traces := newPollTraces()
	defer traces.end()

	prepareAll(ctx, d, cfg, perms, inst, traces, assigned, repos, runs.waiting)

	// An agent role no worker host takes leaves its issues queued. One warning per poll, and nothing
	// fails: the queue stays visible and pausable until a host takes the agent role.
	warnUnhosted(d, cfg)

	for _, q := range pollQueues() {
		if cfg.Unhosted(q.role) {
			queueSkipped(ctx, q.role, telemetry.SkipUnhosted)

			continue
		}

		// A run waiting for a worker is not a run in progress, so an agent role with one is not claimed
		// for yet: its queued issue is what the worker takes next.
		if runs.waiting[q.role] {
			d.log().Info("leaving an agent role's queue while its DAG is busy", "agent_role", q.role)
			queueSkipped(ctx, q.role, telemetry.SkipDAGBusy)

			continue
		}

		// Runs in progress still leave room while the fleet-wide capacity is not full.
		slots := runs.free(q.role, cfg.Capacity(q.role))
		if slots == 0 {
			d.log().Info("leaving an agent role's queue while its runs fill its capacity", "agent_role", q.role)
			queueSkipped(ctx, q.role, telemetry.SkipCapacityFull)

			continue
		}

		code, done, err := claimFromQueue(ctx, d, cfg, perms, inst, traces, assigned, repos, q, slots)
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
			d.log().Warn("no worker host takes this agent role; its issues stay queued", "agent_role", role)
		}
	}
}

// readRuns reads dagu's active runs, read-only, and counts them by agent role. A dagu read that
// fails is an error: the poll must never claim while it cannot tell.
func readRuns(ctx context.Context, d Deps, prefix string) (runState, error) {
	out, err := d.run(ctx, proc.Cmd{
		Argv: dagu.ActiveHistoryArgv(d.Settings.Dagu, PollRunLimit), Capture: true, Timeout: DaguTimeout,
	})
	if err != nil {
		return runState{}, fmt.Errorf("read the agent roles' runs from dagu: %w", err)
	}

	history, err := dagu.ParseHistory([]byte(out))
	if err != nil {
		return runState{}, err
	}

	state := runState{active: map[workflow.Role]int{}, waiting: map[workflow.Role]bool{}}

	for _, run := range history {
		name, ok := strings.CutPrefix(run.Name, prefix)
		if !ok || !run.Active() {
			continue
		}

		role, err := workflow.ParseRole(name)
		if err != nil {
			continue // another DAG that happens to share the prefix
		}

		state.active[role]++

		if run.Waiting() {
			state.waiting[role] = true
		}
	}

	return state, nil
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

// prepareAll brings every assigned repository up to date. A repository the poll cannot read marks its
// span as an error and is skipped; the poll still works the others.
func prepareAll(
	ctx context.Context, d Deps, cfg config.Config, perms permissions, inst pollInstruments,
	traces *pollTraces, assigned Assignment, repos []workflow.Repo, waiting map[workflow.Role]bool,
) {
	for _, repo := range repos {
		repoCtx, span := startRepoSpan(ctx, d, repo)
		traces.set(repo, span)

		if err := prepareRepo(repoCtx, d, cfg, perms, inst, repo, assigned[repo], waiting); err != nil {
			d.log().Warn("skipping a repository the poll cannot read", "repo", repo, "error", err)
			failSpan(span, "repository read failed")
		}
	}
}

// prepareRepo creates the factory labels, calls dead runs failed, repairs stranded issues from their
// latest marker, wakes an issue a writer has commented on, and routes new issues to their first
// queue. Reads use the polling App's token; a write uses the token of the agent role that owns it.
func prepareRepo(
	ctx context.Context, d Deps, cfg config.Config, perms permissions, inst pollInstruments,
	repo workflow.Repo, roles []workflow.Role, waiting map[workflow.Role]bool,
) error {
	role, ok := pollingRole(roles)
	if !ok {
		return nil
	}

	token, err := d.mint(ctx, role, repo, writableToken)
	if err != nil {
		d.log().Warn("cannot mint token; treating the repository as unassigned",
			"agent_role", role, "repo", repo, "error", err)

		return fmt.Errorf("mint %s token: %w", role, err)
	}

	c := d.client(token)
	if err := c.EnsureLabels(ctx, repo, workflow.FactoryLabels()); err != nil {
		return err
	}

	// The dashboard is best effort: an issue cannot be handed off without the work below it, but a
	// retro dashboard that cannot be drawn is retried on the next poll.
	if err := redrawDashboard(ctx, d, cfg, c, perms, repo, roles, waiting, role); err != nil {
		d.log().Warn("cannot redraw the retro dashboard", "repo", repo, "error", err)
	}

	issues, err := c.ListIssues(ctx, repo, github.StateOpen, github.Oldest)
	if err != nil {
		return err
	}

	measureIssues(ctx, inst, repo, issues)

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

// measureIssues records how many open issues the repository has in each factory state. A stranded
// issue carrying more than one state label counts in each, which is what the labels say.
func measureIssues(ctx context.Context, inst pollInstruments, repo workflow.Repo, issues []workflow.Issue) {
	counts := map[workflow.State]int64{}

	for _, issue := range issues {
		if issue.IsPullRequest {
			continue
		}

		for _, state := range workflow.States() {
			if state != workflow.Closed && workflow.HasLabel(issue, string(state)) {
				counts[state]++
			}
		}
	}

	for state, count := range counts {
		role, _ := workflow.RoleOf(state)
		inst.issuesInState(ctx, repo, state, role, count)
	}
}

// settleIssue calls a dead run failed, then repairs, wakes or routes the issue.
func settleIssue(
	ctx context.Context, d Deps, cfg config.Config, c github.Client, perms permissions,
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
			"repo", repo, "issue", issue.Number, "agent_role", role)

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

	token, err := d.mint(ctx, role, repo, writableToken)
	if err != nil {
		d.log().Warn("cannot mint the dead run's own token; leaving the issue claimed",
			"agent_role", role, "repo", repo, "issue", issue.Number, "error", err)

		return false, nil
	}

	d.log().Info("failing a dead run", "repo", repo, "issue", issue.Number, "agent_role", role,
		"since", since, "deadline", agent.Deadline(), "margin", margin)

	t := workflow.Transition{Role: role, RunID: d.runID(), From: state, To: workflow.Failed}
	text := fmt.Sprintf("The %s run outlived its %s deadline plus the %s dead-run margin. "+
		"It is not retried automatically; comment to send it back to the %s.",
		role, agent.Deadline(), margin, role)

	_, err = d.client(token).Finish(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number}, t, text)
	if err == nil {
		newAlertInstruments(d).runReaped(ctx, role, repo)
	}

	return true, err
}

// prepareIssue repairs, wakes and routes one issue. A read failure is returned, so the issue is left alone.
func prepareIssue(
	ctx context.Context, d Deps, c github.Client, perms permissions, repo workflow.Repo, issue workflow.Issue,
) error {
	comments, err := issueComments(ctx, c, repo, issue)
	if err != nil {
		return err
	}

	latest, found, err := workflow.LatestMarker(comments, repo.Owner())
	if err != nil {
		return fmt.Errorf("read the latest marker on #%d: %w", issue.Number, err)
	}

	if plan, repair := workflow.Repair(issue, latest.Transition, found); repair {
		if err := repairIssue(ctx, d, repo, issue, latest.Transition, plan); err != nil {
			return err
		}

		issue.Labels = plan.Apply(issue.Labels)
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

// repairIssue leaves the issue carrying exactly the state the latest marker names, with the token of
// the agent role that made the transition. The polling App's token never stands in for it: an issue
// whose owning role cannot mint stays stranded for the next poll. The state is added before the stale
// ones are removed, so a failed repair leaves the marker's state to retry rather than an unlabelled
// issue the next poll would route afresh.
func repairIssue(
	ctx context.Context, d Deps, repo workflow.Repo, issue workflow.Issue, t workflow.Transition,
	plan workflow.RepairPlan,
) error {
	if t.Role == "" {
		return fmt.Errorf("repair #%d: the marker names no agent role", issue.Number)
	}

	token, err := d.mint(ctx, t.Role, repo, writableToken)
	if err != nil {
		return fmt.Errorf("repair #%d as %s: %w", issue.Number, t.Role, err)
	}

	ref := workflow.IssueRef{Repo: repo, Number: issue.Number}
	c := d.client(token)

	d.log().Info("repairing a stranded issue", "repo", repo, "issue", issue.Number,
		"state", plan.Add, "stale", plan.Remove, "agent_role", t.Role)

	if plan.Add != "" {
		if err := c.AddLabel(ctx, ref, string(plan.Add)); err != nil {
			return err
		}
	}

	for _, stale := range plan.Remove {
		if err := c.RemoveLabel(ctx, ref, string(stale)); err != nil {
			return err
		}
	}

	return nil
}

// wakeIssue moves an issue a writer has commented on out of the state that was waiting for them: a
// waiting issue goes back to the Planner, a done one back to the Implementer, and a failed one back
// to the agent role that failed. A done issue is woken by a comment on its pull request; the others
// by a comment on the issue. The wake is made with the token of the agent role it queues for.
func wakeIssue(
	ctx context.Context, d Deps, perms permissions, c github.Client, repo workflow.Repo, issue workflow.Issue,
	comments []workflow.Item, latest workflow.Marker, found bool,
) (bool, error) {
	if !found {
		return false, nil
	}

	thread, ok, err := wakeThread(ctx, c, repo, issue, comments, latest)
	if err != nil || !ok {
		return false, err
	}

	// A writer's reply that opens the gate on the latest final round applies the stored proposal with
	// no model run.
	applied, err := applyGateReply(ctx, d, perms, c, repo, issue, thread, latest)
	if err != nil || applied {
		return applied, err
	}

	state, ok := workflow.WakeState(latest.Transition, true, written(ctx, d, perms, c, repo, thread, latest))
	if !ok {
		return false, nil
	}

	role, ok := workflow.RoleOf(state)
	if !ok {
		return false, nil
	}

	token, err := d.mint(ctx, role, repo, writableToken)
	if err != nil {
		d.log().Warn("cannot mint the waking agent role's token; leaving the issue where it is",
			"agent_role", role, "repo", repo, "issue", issue.Number, "error", err)

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
	ctx context.Context, d Deps, perms permissions, c github.Client, repo workflow.Repo,
	thread []workflow.Item, latest workflow.Marker,
) bool {
	for _, comment := range workflow.AfterMarker(thread, latest, repo.Owner()) {
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

// permissionState is what one poll read about a login: whether it may write, and whether the read
// succeeded at all, so a failed-closed check is never mistaken for a plain non-writer.
type permissionState struct {
	writer   bool
	readable bool
}

// permissions caches, for one poll, whether each login may write to the repository and whether that
// could be read, keyed by "repo\x00login". A permission it cannot read is cached as not-a-writer, so
// every decision fails closed. It is a map so every reader of one poll's cache shares the one cache.
type permissions map[string]permissionState

// newPermissions starts an empty permission cache.
func newPermissions() permissions {
	return permissions{}
}

// isWriter reports whether login has write access. An unreadable permission is not write access.
func (p permissions) isWriter(ctx context.Context, d Deps, c github.Client, repo workflow.Repo, login string) bool {
	return p.check(ctx, d, c, repo, login).writer
}

// check reports whether login has write access and whether the permission could be read at all. An
// unreadable permission is cached as not-a-writer, so every decision fails closed.
func (p permissions) check(
	ctx context.Context, d Deps, c github.Client, repo workflow.Repo, login string,
) permissionState {
	if login == "" {
		return permissionState{}
	}

	key := repo.String() + "\x00" + login
	if state, ok := p[key]; ok {
		return state
	}

	permission, err := c.CollaboratorPermission(ctx, repo, login)
	if err != nil {
		d.log().Warn("cannot read a collaborator's permission; failing closed",
			"repo", repo, "login", login, "error", err)

		p[key] = permissionState{writer: false, readable: false}

		return p[key]
	}

	p[key] = permissionState{writer: workflow.WriterPermission(permission), readable: true}

	return p[key]
}

// counted reports whether an issue counts: its author is a writer, or a writer has commented on it.
func counted(
	ctx context.Context, d Deps, c github.Client, perms permissions, repo workflow.Repo,
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

		if blocker.State != string(workflow.Closed) {
			return true, nil
		}
	}

	return false, nil
}

// claimFromQueue gathers every eligible issue in one agent role's queue across the repositories where
// its App is installed, then claims up to slots of the oldest ones, oldest first. A repository whose
// token cannot be minted is unassigned for this agent role. done is true when the poll is over: an
// issue was claimed or lost, or the CI gate moved one.
func claimFromQueue(
	ctx context.Context, d Deps, cfg config.Config, perms permissions, inst pollInstruments,
	traces *pollTraces, assigned Assignment, repos []workflow.Repo, q queueStep, slots int,
) (code int, done bool, err error) {
	candidates := make([]queuedIssue, 0)

	for _, repo := range repos {
		if !slices.Contains(assigned[repo], q.role) {
			continue // that agent role isn't here: its issues wait in the queue
		}

		repoCtx := traces.context(ctx, repo)

		token, err := d.mint(repoCtx, q.role, repo, writableToken)
		if err != nil {
			d.log().Warn("cannot mint token; treating the repository as unassigned",
				"agent_role", q.role, "repo", repo, "error", err)
			traces.fail(repo, "token mint failed")

			continue
		}

		c := d.client(token)

		issues, err := c.ListLabeled(repoCtx, repo, string(q.src))
		if err != nil {
			// One unreadable repository must not stop the poll working the others.
			d.log().Warn("skipping a repository whose queue cannot be read",
				"agent_role", q.role, "repo", repo, "error", err)
			traces.fail(repo, "queue read failed")

			continue
		}

		for _, issue := range issues {
			if eligible(repoCtx, d, c, perms, repo, issue) {
				candidates = append(candidates, queuedIssue{ctx: repoCtx, repo: repo, issue: issue, client: c})
			}
		}
	}

	return claimOldest(d, cfg, inst, q, candidates, slots)
}

// claimOldest claims the oldest candidates in one agent role's queue, up to the slots the role's run
// capacity leaves free. The Reviewer's CI gate runs first: pending CI leaves that issue queued for a
// later candidate, while an issue the gate moved ends the poll with no model run. A lost claim ends it
// too, so a second poll never takes the issue this one just lost.
func claimOldest(
	d Deps, cfg config.Config, inst pollInstruments, q queueStep, candidates []queuedIssue, slots int,
) (code int, done bool, err error) {
	slices.SortStableFunc(candidates, compareIssues)

	claimed := 0

	for _, candidate := range candidates {
		if claimed == slots {
			break
		}

		if q.role == workflow.Reviewer {
			claim, moved, gateErr := ciGate(candidate.ctx, d, cfg, candidate.client, candidate.repo, candidate.issue)
			if gateErr != nil {
				d.log().Warn("skipping an issue whose CI cannot be read",
					"repo", candidate.repo, "issue", candidate.issue.Number, "error", gateErr)

				continue
			}

			if moved {
				return 0, true, nil
			}

			if !claim {
				// Pending CI leaves the issue queued; a later issue may still be claimable.
				continue
			}
		}

		won, code, err := claimAndEnqueue(candidate.ctx, d, inst, candidate.client, candidate.repo, candidate.issue, q)
		if err != nil || !won {
			return code, true, err
		}

		claimed++
	}

	return 0, claimed > 0, nil
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
	ctx context.Context, d Deps, c github.Client, perms permissions, repo workflow.Repo, issue workflow.Issue,
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

	rounds := workflow.ReviewRounds(reviews, comments, report.SHA, repo.Owner())

	dst := workflow.Implement
	text := "CI failed on the pull request. Sent back to the Implementer with the failure report above; " +
		"no model run was needed."

	if workflow.RoundLimitReached(rounds, cfg.ReviewRoundLimit) {
		dst = workflow.Failed
		text = fmt.Sprintf("CI failed and this is review round %d of %d. "+
			"A person needs to look at the pull request.", rounds, cfg.ReviewRoundLimit)
	}

	// The report is keyed by the commit, so a retried poll never posts it twice.
	if !workflow.HasCIFailure(comments, report.SHA, repo.Owner()) {
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

// claimAndEnqueue claims the issue, then queues its run. won is false when another run claimed the
// issue first. A queue failure fails the issue rather than leaving it claimed. The enqueue is its own
// span, so a run can continue this trace through the DAG's parameters.
func claimAndEnqueue(
	ctx context.Context, d Deps, inst pollInstruments, c github.Client, repo workflow.Repo,
	issue workflow.Issue, q queueStep,
) (won bool, code int, err error) {
	sha, err := headSHA(ctx, c, workflow.IssueRef{Repo: repo, Number: issue.Number})
	if err != nil {
		return false, 1, err
	}

	t := workflow.Transition{Role: q.role, RunID: d.runID(), From: q.src, To: q.dst, SHA: sha}

	won, err = c.Claim(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number}, t)
	if err != nil {
		return false, 1, err
	}

	if !won {
		return false, 0, writeLine(d.Out, fmt.Sprintf("issue %d was claimed by someone else", issue.Number))
	}

	trace.SpanFromContext(ctx).AddEvent(telemetry.EventClaim, trace.WithAttributes(
		attribute.String(telemetry.AttrAgentRole, string(q.role)),
		attribute.Int(telemetry.AttrIssue, issue.Number),
	))

	enqueueCtx, span := startEnqueueSpan(ctx, d, repo, q.role, issue.Number)

	_, err = d.run(enqueueCtx, proc.Cmd{Argv: []string{
		d.Settings.Dagu, "enqueue", d.Settings.DAGPrefix + string(q.role), "--",
		"REPO=" + repo.String(), "ISSUE=" + strconv.Itoa(issue.Number),
	}})
	if err == nil {
		inst.claim(ctx, repo, q.role)
	} else {
		d.log().Error("enqueue failed", "agent_role", q.role, "repo", repo, "issue", issue.Number, "error", err)
		failSpan(span, "enqueue failed")
	}

	span.End()

	if err != nil {
		t.From, t.To = q.dst, workflow.Failed

		ref := workflow.IssueRef{Repo: repo, Number: issue.Number}

		_, err = c.Finish(ctx, ref, t, "Could not queue the agent run; see the dagu log.")

		return false, 1, err
	}

	return true, 0, nil
}

// Start runs on the worker: it hands the factory user a token, runs the agent role, and records the outcome.
func Start(ctx context.Context, d Deps, cfg config.Config, role workflow.Role, ref workflow.IssueRef) (int, error) {
	entry, ok := dispatchEntryFor(role)
	if !ok {
		return 1, fmt.Errorf("unknown agent role %q", role)
	}

	r := entry.route(cfg.ReviewRoundLimit, ref, factoryExit(ctx, d, cfg, role, ref))

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
// mint failure always falls back to factory:failed, reported by an App that is still installed. The
// factory half applies it before its retro, and `start` applies it again afterwards as the safety net
// for a factory half that never returned; a transition already recorded is not recorded again.
func finishRun(
	ctx context.Context, d Deps, role workflow.Role, ref workflow.IssueRef, src, dst workflow.State, text string,
) error {
	t := workflow.Transition{Role: role, RunID: d.runID(), From: src, To: dst}

	token, err := d.mint(ctx, role, ref.Repo, writableToken)
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
		"agent_role", role, "repo", ref.Repo, "issue", ref.Number, "error", err)

	t.To = workflow.Failed

	notice := fmt.Sprintf("The %s's GitHub App credential is missing or the App is no longer installed here. "+
		"Fix that, then requeue the issue.", role)
	for _, other := range workflow.KnownRoles() {
		if other == role {
			continue
		}

		otherToken, mintErr := d.mint(ctx, other, ref.Repo, writableToken)
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
		d.log().Error("run as factory failed", "agent_role", role, "issue", ref, "error", err)
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

	token, err := d.mint(ctx, role, repo, writableToken)
	if err != nil {
		return 0, err
	}

	path, err := d.tokenPath(role)
	if err != nil {
		return 0, err
	}

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
		// The factory half records the same transition markers, so both halves must name one run.
		// The sudoers rule keeps this variable for that reason.
		Env:     map[string]string{"DAG_RUN_ID": d.runID()},
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
// It writes a private temporary file in the same directory, sets the final mode, then renames it into
// place, so a reader sees the whole token or no file, never an empty or partial one.
func writeTokenFile(path, token string) (err error) {
	// The path is the edge-configured token directory joined with a vetted name, never input.
	// CreateTemp opens exclusively with a random name and mode 0600.
	f, err := os.CreateTemp(filepath.Dir(path), ".gh-token-*")
	if err != nil {
		return fmt.Errorf("write token file: %w", err)
	}

	tmp := f.Name()

	defer func() {
		if err != nil {
			err = errors.Join(err, os.Remove(tmp)) // nothing is renamed on failure
		}
	}()

	if _, err := f.WriteString(token); err != nil {
		return errors.Join(fmt.Errorf("write token file: %w", err), f.Close())
	}

	// The mode is group-readable on purpose: the factory half runs as another user in the factory
	// group. Set it before the rename so the file is never visible with another mode.
	if err := f.Chmod(tokenFileMode); err != nil {
		return errors.Join(fmt.Errorf("write token file: %w", err), f.Close())
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}

	return nil
}
