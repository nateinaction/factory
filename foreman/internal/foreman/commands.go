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

	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// MaxAssignedRepos bounds how many assigned repositories one poll looks at.
const MaxAssignedRepos = 200

// mintToken asks the helper for a token of the agent role's own App, scoped to the repository.
// There is no fallback: if this App can't mint, no other App's token stands in for it.
func (d Deps) mintToken(ctx context.Context, role workflow.Role, repo string) (string, error) {
	// stderr is inherited so the helper's error reaches the dagu log; it never prints the token there.
	out, err := d.run(ctx, proc.Cmd{
		Argv:    []string{d.Settings.TokenHelper, "token", "--app", string(role), "--repo", repo},
		Capture: true, Timeout: MintTimeout,
	})
	if err != nil {
		return "", fmt.Errorf("mint %s token for %s: %w", role, repo, err)
	}

	return strings.TrimSpace(out), nil
}

// installedRepos lists the repositories the agent role's App is installed on. An App that can't
// list its installations is installed nowhere: the failure is logged and the repositories stay unassigned.
func (d Deps) installedRepos(ctx context.Context, role workflow.Role) []string {
	out, err := d.run(ctx, proc.Cmd{
		Argv:    []string{d.Settings.TokenHelper, "repositories", "--app", string(role)},
		Capture: true, Timeout: MintTimeout,
	})
	if err != nil {
		d.log().Warn("cannot list installations; treating the App as installed nowhere", "role", role, "error", err)
		return nil
	}

	var repos []string

	for line := range strings.SplitSeq(out, "\n") {
		repo := strings.TrimSpace(line)
		if workflow.ValidateRepo(repo) == nil {
			repos = append(repos, repo)
		}
	}

	return repos
}

// Assignment says which agent roles' Apps are installed on each assigned repository.
type Assignment map[string][]workflow.Role

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
func (a Assignment) Repos() []string {
	repos := slices.Sorted(maps.Keys(a))
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

// Poll is deterministic and cheap: no model involved. It first brings every assigned repository's
// labels, routing and recovery up to date, then claims at most one issue and queues its run. It looks
// only at repositories where one of the roles' Apps is installed, and in each only at the queues of
// agent roles whose Apps are installed there; every claim uses its own agent role's token.
func Poll(ctx context.Context, d Deps, roles []workflow.Role) (int, error) {
	assigned := Assigned(ctx, d, roles)

	repos := assigned.Repos()
	perms := newPermissions()

	for _, repo := range repos {
		if err := prepareRepo(ctx, d, perms, repo, assigned[repo]); err != nil {
			return 1, err
		}
	}

	for _, q := range pollQueues() {
		for _, repo := range repos {
			if !slices.Contains(assigned[repo], q.role) {
				continue // that agent role isn't here: its issues wait in the queue
			}

			code, done, err := pollQueue(ctx, d, perms, repo, q)
			if done || err != nil {
				return code, err
			}
		}
	}

	return 0, writeLine(d.Out, "nothing ready")
}

// pollingRole is the App that places new issues in their first queue: the first installed agent role
// in scheduling order. Polling is read-only otherwise, so any installed App may do it.
func pollingRole(roles []workflow.Role) (workflow.Role, bool) {
	for _, q := range workflow.Queues() {
		if slices.Contains(roles, q.Role) {
			return q.Role, true
		}
	}

	return "", false
}

// prepareRepo creates the factory labels, repairs stranded issues from their latest marker, and
// routes new issues to their first queue. Every write uses the polling App's token.
func prepareRepo(ctx context.Context, d Deps, perms *permissions, repo string, roles []workflow.Role) error {
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

	issues, err := c.ListOpenIssues(ctx, repo)
	if err != nil {
		return err
	}

	for _, issue := range issues {
		if issue.IsPullRequest || workflow.HasLabel(issue, workflow.LabelPaused) {
			continue
		}

		if err := prepareIssue(ctx, d, c, perms, repo, issue); err != nil {
			// One unreadable issue fails closed for that issue; the poll carries on.
			d.log().Warn("skipping an issue the poll cannot read", "repo", repo, "issue", issue.Number, "error", err)
		}
	}

	return nil
}

// prepareIssue repairs and routes one issue. A read failure is returned, so the issue is left alone.
func prepareIssue(
	ctx context.Context, d Deps, c github.Client, perms *permissions, repo string, issue workflow.Issue,
) error {
	comments, err := issueComments(ctx, c, repo, issue)
	if err != nil {
		return err
	}

	latest, found, err := workflow.LatestTransition(comments)
	if err != nil {
		return fmt.Errorf("read the latest marker on #%d: %w", issue.Number, err)
	}

	if state, repair := workflow.Repair(issue, latest, found); repair {
		d.log().Info("repairing a stranded issue", "repo", repo, "issue", issue.Number, "state", state)

		if err := c.AddLabel(ctx, repo, issue.Number, string(state)); err != nil {
			return err
		}

		issue.Labels = append(issue.Labels, string(state))
	}

	state, ok := workflow.Route(issue)
	if !ok {
		return nil
	}

	if !counted(ctx, d, c, perms, repo, issue, comments) {
		return nil
	}

	if err := c.AddLabel(ctx, repo, issue.Number, string(state)); err != nil {
		return err
	}

	return nil
}

// issueComments lists an issue's comments, skipping the call when it has none.
func issueComments(ctx context.Context, c github.Client, repo string, issue workflow.Issue) ([]workflow.Item, error) {
	if issue.Comments == 0 {
		return nil, nil
	}

	return c.ListComments(ctx, repo, issue.Number)
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
func (p *permissions) isWriter(ctx context.Context, d Deps, c github.Client, repo, login string) bool {
	if login == "" {
		return false
	}

	key := repo + "\x00" + login
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
	ctx context.Context, d Deps, c github.Client, perms *permissions, repo string,
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
func blocked(ctx context.Context, c github.Client, repo string, issue workflow.Issue) (bool, error) {
	open, supported, err := c.OpenBlockers(ctx, repo, issue.Number)
	if err != nil {
		return false, err
	}

	if supported {
		return len(open) > 0, nil
	}

	for _, number := range workflow.BlockedBy(issue.Body) {
		blocker, err := c.GetIssue(ctx, repo, number)
		if err != nil {
			return false, fmt.Errorf("read blocker #%d: %w", number, err)
		}

		if blocker.State != "closed" {
			return true, nil
		}
	}

	return false, nil
}

// pollQueue looks at one queue of one repository with the owning agent role's token. done is true when
// the poll is over: an issue was claimed or lost. A token that can't be minted makes the repository
// unassigned for this role.
func pollQueue(
	ctx context.Context, d Deps, perms *permissions, repo string, q queueStep,
) (code int, done bool, err error) {
	token, err := d.mintToken(ctx, q.role, repo)
	if err != nil {
		d.log().Warn("cannot mint token; treating the repository as unassigned",
			"role", q.role, "repo", repo, "error", err)

		return 0, false, nil
	}

	c := d.client(token)

	issues, err := c.ListLabeled(ctx, repo, string(q.src))
	if err != nil {
		return 1, true, err
	}

	issue, ok := claimable(ctx, d, c, perms, repo, issues)
	if !ok {
		return 0, false, nil
	}

	code, err = claimAndEnqueue(ctx, d, c, repo, issue, q)

	return code, true, err
}

// claimable is the oldest issue in the queue that is counted, not already claimed, and unblocked.
func claimable(
	ctx context.Context, d Deps, c github.Client, perms *permissions, repo string, issues []workflow.Issue,
) (workflow.Issue, bool) {
	for _, issue := range issues {
		if issue.IsPullRequest || workflow.HasLabel(issue, workflow.LabelPaused) || claimed(issue) {
			continue
		}

		comments, err := issueComments(ctx, c, repo, issue)
		if err != nil {
			d.log().Warn("skipping an issue whose comments cannot be read",
				"repo", repo, "issue", issue.Number, "error", err)

			continue
		}

		if !counted(ctx, d, c, perms, repo, issue, comments) {
			continue
		}

		isBlocked, err := blocked(ctx, c, repo, issue)
		if err != nil {
			d.log().Warn("skipping an issue whose blockers cannot be read",
				"repo", repo, "issue", issue.Number, "error", err)

			continue
		}

		if isBlocked {
			continue
		}

		return issue, true
	}

	return workflow.Issue{}, false
}

// claimed reports whether the issue already carries a running label: a run is happening.
func claimed(issue workflow.Issue) bool {
	for _, q := range workflow.Queues() {
		if workflow.HasLabel(issue, string(q.Running)) {
			return true
		}
	}

	return false
}

// headSHA is the commit the issue's open pull request is at, or empty before one exists.
func headSHA(ctx context.Context, c github.Client, repo string, number int) (string, error) {
	pr, err := c.ExistingPR(ctx, repo, workflow.BranchName(number))
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
	ctx context.Context, d Deps, c github.Client, repo string, issue workflow.Issue, q queueStep,
) (int, error) {
	sha, err := headSHA(ctx, c, repo, issue.Number)
	if err != nil {
		return 1, err
	}

	t := workflow.Transition{Role: q.role, RunID: d.runID(), From: q.src, To: q.dst, SHA: sha}

	won, err := c.Claim(ctx, repo, issue.Number, t)
	if err != nil {
		return 1, err
	}

	if !won {
		return 0, writeLine(d.Out, fmt.Sprintf("issue %d was claimed by someone else", issue.Number))
	}

	_, err = d.run(ctx, proc.Cmd{Argv: []string{
		d.Settings.Dagu, "enqueue", d.Settings.DAGPrefix + string(q.role), "--",
		"REPO=" + repo, "ISSUE=" + strconv.Itoa(issue.Number),
	}})
	if err != nil {
		d.log().Error("enqueue failed", "role", q.role, "repo", repo, "issue", issue.Number, "error", err)

		t.From, t.To = q.dst, workflow.Failed

		return 1, c.Finish(ctx, repo, issue.Number, t, "Could not queue the agent run; see the dagu log.")
	}

	return 0, nil
}

// errNoBehaviour is returned for a known agent role this slice does not implement yet.
var errNoBehaviour = errors.New("agent role has no behaviour yet")

// Start runs on the worker: it hands the factory user a token, runs the agent role, and records the outcome.
func Start(ctx context.Context, d Deps, role workflow.Role, repo string, number int) (int, error) {
	switch role {
	case workflow.Implementer:
		return startImplementer(ctx, d, repo, number)
	case workflow.Reviewer:
		return startReviewer(ctx, d, repo, number)
	case workflow.Planner, workflow.Researcher, workflow.Curator:
		return 1, fmt.Errorf("%s: %w", role, errNoBehaviour)
	}

	return 1, fmt.Errorf("unknown agent role %q", role)
}

// startImplementer implements the issue, then queues it for review or fails it.
func startImplementer(ctx context.Context, d Deps, repo string, number int) (int, error) {
	code := factoryExit(ctx, d, workflow.Implementer, repo, number)
	if code == 0 {
		return 0, finishAs(ctx, d, workflow.Implementer, repo, number, workflow.Implementing, workflow.Review,
			"The Implementer finished; see the linked pull request. Queued for review.")
	}

	return 1, finishAs(ctx, d, workflow.Implementer, repo, number, workflow.Implementing, workflow.Failed,
		"The Implementer failed. Check the dagu run log, then comment to send it back to the Implementer.")
}

// startReviewer reviews the issue's pull request and routes the issue by what the review found.
func startReviewer(ctx context.Context, d Deps, repo string, number int) (int, error) {
	code := factoryExit(ctx, d, workflow.Reviewer, repo, number)
	dst := workflow.Failed
	text := "The Reviewer failed. Check the dagu run log, then comment to send it back to the Reviewer."

	switch code {
	case workflow.ReviewClean:
		dst, text = workflow.Done, "Review found no high or medium issues; see the pull request."
	case workflow.ReviewFindings:
		dst = workflow.Implement
		text = "Review found high or medium issues, posted on the pull request. Sent back to the Implementer."
	case workflow.ReviewExhausted:
		dst, text = workflow.Failed, fmt.Sprintf("Review still found high or medium issues after %d rounds. "+
			"A person needs to look at the pull request.", workflow.MaxReviewRounds)
	}

	exit := 1
	if code == workflow.ReviewClean || code == workflow.ReviewFindings {
		exit = 0
	}

	return exit, finishAs(ctx, d, workflow.Reviewer, repo, number, workflow.Reviewing, dst, text)
}

// finishAs records the outcome with the owning agent role's own token (fresh: the run may have outlived
// the first one). If that App can't mint, because its credential is missing or it was removed, the run
// fails closed: an App that is still installed moves the issue to failed and says why. No other App ever
// makes the owning agent role's transition.
func finishAs(
	ctx context.Context, d Deps, role workflow.Role, repo string, number int, src, dst workflow.State, text string,
) error {
	t := workflow.Transition{Role: role, RunID: d.runID(), From: src, To: dst}

	token, err := d.mintToken(ctx, role, repo)
	if err == nil {
		// The commit is recorded when it can be read; the transition itself is what matters.
		if sha, shaErr := headSHA(ctx, d.client(token), repo, number); shaErr == nil {
			t.SHA = sha
		} else {
			d.log().Warn("cannot read the pull request head for the transition marker",
				"repo", repo, "issue", number, "error", shaErr)
		}

		return d.client(token).Finish(ctx, repo, number, t, text)
	}

	d.log().Error("cannot mint the owning App's token; reporting the failure as another App",
		"role", role, "repo", repo, "issue", number, "error", err)

	t.To = workflow.Failed

	notice := fmt.Sprintf("The %s's GitHub App credential is missing or the App is no longer installed here. "+
		"Fix that, then requeue the issue.", role)
	for _, other := range workflow.KnownRoles() {
		if other == role {
			continue
		}

		otherToken, mintErr := d.mintToken(ctx, other, repo)
		if mintErr != nil {
			continue // that App isn't installed here or has no credential: try the next
		}

		return d.client(otherToken).Finish(ctx, repo, number, t, notice)
	}

	return fmt.Errorf("no installed App can report the failure on %s#%d: %w", repo, number, err)
}

// factoryExit is the exit code of `foreman run <role> <repo> <n>` as the factory user, with the token file
// in place. It is -1 when the run could not be started: that is logged, and the issue must not stay claimed.
func factoryExit(ctx context.Context, d Deps, role workflow.Role, repo string, number int) int {
	code, err := runAsFactory(ctx, d, role, repo, number)
	if err != nil {
		d.log().Error("run as factory failed", "role", role, "repo", repo, "issue", number, "error", err)
		return -1
	}

	return code
}

// runAsFactory writes the token where the factory user can read it, runs the factory half, and removes
// the token.
func runAsFactory(ctx context.Context, d Deps, role workflow.Role, repo string, number int) (code int, err error) {
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

	_, runErr := d.Runner.Run(ctx, proc.Cmd{
		Argv:    []string{"sudo", "-n", "-u", "factory", d.Settings.Self, "run", string(role), repo, strconv.Itoa(number)},
		Timeout: AgentTimeout + 2*GitTimeout,
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
	// The path is the edge-configured token directory joined with a parsed role, never input. The
	// mode is group-readable on purpose: the factory half runs as another user in the factory group.
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
