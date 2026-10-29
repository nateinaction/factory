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
		d.logf("list %s installations: %v", role, err)
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
	src, dst string
	role     workflow.Role
	note     string
}

// pollQueues puts reviews first: they are short, and they unblock the pull requests already waiting.
var pollQueues = []queueStep{
	{workflow.LabelReviewReady, workflow.LabelReviewing, workflow.Reviewer, "Picked up for review by the factory agent."},
	{workflow.LabelReady, workflow.LabelWorking, workflow.Implementer, "Picked up by the factory agent."},
}

// Poll is deterministic and cheap: no model involved. It claims at most one issue and queues its run.
// It looks only at repositories where one of the roles' Apps is installed, and in each only at the
// queues of agent roles whose Apps are installed there; every claim uses its own agent role's token.
func Poll(ctx context.Context, d Deps, roles []workflow.Role) (int, error) {
	assigned := Assigned(ctx, d, roles)
	repos := assigned.Repos()
	for _, q := range pollQueues {
		for _, repo := range repos {
			if !slices.Contains(assigned[repo], q.role) {
				continue // that agent role isn't here: its issues wait in the queue
			}
			code, done, err := pollQueue(ctx, d, repo, q)
			if done || err != nil {
				return code, err
			}
		}
	}
	return 0, writeLine(d.Out, "nothing ready")
}

// pollQueue looks at one queue of one repository with the owning agent role's token. done is true when
// the poll is over: an issue was claimed or lost. A token that can't be minted makes the repository unassigned for this role.
func pollQueue(ctx context.Context, d Deps, repo string, q queueStep) (code int, done bool, err error) {
	token, err := d.mintToken(ctx, q.role, repo)
	if err != nil {
		d.logf("%v; treating %s as unassigned for %s", err, repo, q.role)
		return 0, false, nil
	}
	c := d.client(token)
	issues, err := c.ListLabeled(ctx, repo, q.src)
	if err != nil {
		return 1, true, err
	}
	issue, ok := workflow.PickIssue(issues)
	if !ok {
		return 0, false, nil
	}
	code, err = claimAndEnqueue(ctx, d, c, repo, issue.Number, q)
	return code, true, err
}

// claimAndEnqueue claims the issue, then queues its run. A queue failure fails the issue rather than leaving it claimed.
func claimAndEnqueue(ctx context.Context, d Deps, c github.Client, repo string, number int, q queueStep) (int, error) {
	won, err := c.Claim(ctx, repo, number, q.src, q.dst)
	if err != nil {
		return 1, err
	}
	if !won {
		return 0, writeLine(d.Out, fmt.Sprintf("issue %d was claimed by someone else", number))
	}
	if err := c.Comment(ctx, repo, number, q.note); err != nil {
		return 1, err
	}
	_, err = d.run(ctx, proc.Cmd{Argv: []string{
		d.Settings.Dagu, "enqueue", d.Settings.DAGPrefix + string(q.role), "--",
		"REPO=" + repo, "ISSUE=" + strconv.Itoa(number),
	}})
	if err != nil {
		d.logf("%v", err)
		return 1, c.Finish(ctx, repo, number, q.dst, workflow.LabelFailed, "Could not queue the agent run; see the dagu log.")
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
		return 0, finishAs(ctx, d, workflow.Implementer, repo, number, workflow.LabelWorking, workflow.LabelReviewReady,
			"The agent finished; see the linked pull request. Queued for review.")
	}
	return 1, finishAs(ctx, d, workflow.Implementer, repo, number, workflow.LabelWorking, workflow.LabelFailed,
		"The agent failed. Check the dagu run log, then re-apply `ready-for-agent` to retry.")
}

// startReviewer reviews the issue's pull request and routes the issue by what the review found.
func startReviewer(ctx context.Context, d Deps, repo string, number int) (int, error) {
	code := factoryExit(ctx, d, workflow.Reviewer, repo, number)
	dst, text := workflow.LabelFailed, "The review failed. Check the dagu run log, then re-apply `ready-for-agent-review` to retry."
	switch code {
	case workflow.ReviewClean:
		dst, text = workflow.LabelDone, "Review found no high or medium issues; see the pull request."
	case workflow.ReviewFindings:
		dst, text = workflow.LabelReady, "Review found high or medium issues, posted on the pull request. Sent back to the agent."
	case workflow.ReviewExhausted:
		dst, text = workflow.LabelFailed, fmt.Sprintf("Review still found high or medium issues after %d rounds. "+
			"A person needs to look at the pull request.", workflow.MaxReviewRounds)
	}
	exit := 1
	if code == workflow.ReviewClean || code == workflow.ReviewFindings {
		exit = 0
	}
	return exit, finishAs(ctx, d, workflow.Reviewer, repo, number, workflow.LabelReviewing, dst, text)
}

// finishAs records the outcome with the owning agent role's own token (fresh: the run may have outlived
// the first one). If that App can't mint, because its credential is missing or it was removed, the run
// fails closed: an App that is still installed moves the issue to failed and says why. No other App ever
// makes the owning agent role's transition.
func finishAs(ctx context.Context, d Deps, role workflow.Role, repo string, number int, src, dst, text string) error {
	token, err := d.mintToken(ctx, role, repo)
	if err == nil {
		return d.client(token).Finish(ctx, repo, number, src, dst, text)
	}
	d.logf("%v", err)
	notice := fmt.Sprintf("The %s's GitHub App credential is missing or the App is no longer installed here. "+
		"Fix that, then re-queue the issue.", role)
	for _, other := range workflow.KnownRoles() {
		if other == role {
			continue
		}
		t, mintErr := d.mintToken(ctx, other, repo)
		if mintErr != nil {
			continue // that App isn't installed here or has no credential: try the next
		}
		return d.client(t).Finish(ctx, repo, number, src, workflow.LabelFailed, notice)
	}
	return fmt.Errorf("no installed App can report the failure on %s#%d: %w", repo, number, err)
}

// factoryExit is the exit code of `foreman run <role> <repo> <n>` as the factory user, with the token file
// in place. It is -1 when the run could not be started: that is logged, and the issue must not stay claimed.
func factoryExit(ctx context.Context, d Deps, role workflow.Role, repo string, number int) int {
	code, err := runAsFactory(ctx, d, role, repo, number)
	if err != nil {
		d.logf("%v", err)
		return -1
	}
	return code
}

// runAsFactory writes the token where the factory user can read it, runs the factory half, and removes the token.
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
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	if _, err := f.WriteString(token); err != nil {
		return errors.Join(fmt.Errorf("write token file: %w", err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	return os.Chmod(path, 0o640)
}
