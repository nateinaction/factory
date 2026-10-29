package foreman

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// Deps is everything at the edge: settings, processes, HTTP and the output streams.
type Deps struct {
	// Settings are the paths and endpoints.
	Settings Settings
	// Runner starts processes.
	Runner Runner
	// HTTP talks to GitHub.
	HTTP *http.Client
	// Out receives normal output.
	Out io.Writer
	// Err receives diagnostics.
	Err io.Writer
}

// client returns a GitHub client for token.
func (d Deps) client(token string) Client {
	return NewClient(d.Settings.API, token, d.HTTP)
}

// run starts a process under a ceiling.
func (d Deps) run(ctx context.Context, c Cmd) (string, error) {
	out, err := d.Runner.Run(ctx, c)
	if err != nil {
		return out, fmt.Errorf("%s: %w", c.Argv[0], err)
	}
	return out, nil
}

// logf writes a diagnostic line.
func (d Deps) logf(format string, args ...any) {
	// A failed diagnostic write has nowhere left to be reported.
	_, _ = fmt.Fprintf(d.Err, "foreman: "+format+"\n", args...)
}

// mintToken asks the helper for a token scoped to the repository.
func (d Deps) mintToken(ctx context.Context, repo string) (string, error) {
	_, name, _ := strings.Cut(repo, "/")
	// stderr is inherited so the helper's error reaches the dagu log; it never prints the token there.
	out, err := d.run(ctx, Cmd{
		Argv: []string{d.Settings.TokenHelper, "token", "--repo", name}, Capture: true, Timeout: MintTimeout,
	})
	if err != nil {
		return "", fmt.Errorf("mint token: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// queueStep is one queue a poll looks at, in priority order.
type queueStep struct {
	src, dst string
	role     Role
	note     string
}

// pollQueues puts reviews first: they are short, and they unblock the pull requests already waiting.
var pollQueues = []queueStep{
	{LabelReviewReady, LabelReviewing, Reviewer, "Picked up for review by the factory agent."},
	{LabelReady, LabelWorking, Implementer, "Picked up by the factory agent."},
}

// Poll is deterministic and cheap: no model involved. It claims at most one issue and queues its run.
func Poll(ctx context.Context, d Deps, repo string) (int, error) {
	token, err := d.mintToken(ctx, repo)
	if err != nil {
		return 1, err
	}
	c := d.client(token)
	for _, q := range pollQueues {
		issues, err := c.ListLabeled(ctx, repo, q.src)
		if err != nil {
			return 1, err
		}
		issue, ok := PickIssue(issues)
		if !ok {
			continue
		}
		return claimAndEnqueue(ctx, d, c, repo, issue.Number, q)
	}
	return 0, writeLine(d.Out, "nothing ready")
}

// writeLine writes text and a newline.
func writeLine(w io.Writer, text string) error {
	_, err := fmt.Fprintln(w, text)
	return err
}

// claimAndEnqueue claims the issue, then queues its run. A queue failure fails the issue rather than leaving it claimed.
func claimAndEnqueue(ctx context.Context, d Deps, c Client, repo string, number int, q queueStep) (int, error) {
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
	_, err = d.run(ctx, Cmd{Argv: []string{
		d.Settings.Dagu, "enqueue", d.Settings.DAGPrefix + string(q.role), "--",
		"REPO=" + repo, "ISSUE=" + strconv.Itoa(number),
	}})
	if err != nil {
		d.logf("%v", err)
		return 1, c.Finish(ctx, repo, number, q.dst, LabelFailed, "Could not queue the agent run; see the dagu log.")
	}
	return 0, nil
}

// errNoBehaviour is returned for a known agent role this slice does not implement yet.
var errNoBehaviour = errors.New("agent role has no behaviour yet")

// Start runs on the worker: it hands the factory user a token, runs the agent role, and records the outcome.
func Start(ctx context.Context, d Deps, role Role, repo string, number int) (int, error) {
	switch role {
	case Implementer:
		return startImplementer(ctx, d, repo, number)
	case Reviewer:
		return startReviewer(ctx, d, repo, number)
	case Planner, Researcher, Curator:
		return 1, fmt.Errorf("%s: %w", role, errNoBehaviour)
	}
	return 1, fmt.Errorf("unknown agent role %q", role)
}

// startImplementer implements the issue, then queues it for review or fails it.
func startImplementer(ctx context.Context, d Deps, repo string, number int) (int, error) {
	code := factoryExit(ctx, d, Implementer, repo, number)
	c, err := freshClient(ctx, d, repo) // fresh: the run may have outlived the first token
	if err != nil {
		return 1, err
	}
	if code == 0 {
		return 0, c.Finish(ctx, repo, number, LabelWorking, LabelReviewReady,
			"The agent finished; see the linked pull request. Queued for review.")
	}
	return 1, c.Finish(ctx, repo, number, LabelWorking, LabelFailed,
		"The agent failed. Check the dagu run log, then re-apply `ready-for-agent` to retry.")
}

// startReviewer reviews the issue's pull request and routes the issue by what the review found.
func startReviewer(ctx context.Context, d Deps, repo string, number int) (int, error) {
	code := factoryExit(ctx, d, Reviewer, repo, number)
	c, err := freshClient(ctx, d, repo)
	if err != nil {
		return 1, err
	}
	dst, text := LabelFailed, "The review failed. Check the dagu run log, then re-apply `ready-for-agent-review` to retry."
	switch code {
	case ReviewClean:
		dst, text = LabelDone, "Review found no high or medium issues; see the pull request."
	case ReviewFindings:
		dst, text = LabelReady, "Review found high or medium issues, posted on the pull request. Sent back to the agent."
	case ReviewExhausted:
		dst, text = LabelFailed, fmt.Sprintf("Review still found high or medium issues after %d rounds. "+
			"A person needs to look at the pull request.", MaxReviewRounds)
	}
	exit := 1
	if code == ReviewClean || code == ReviewFindings {
		exit = 0
	}
	return exit, c.Finish(ctx, repo, number, LabelReviewing, dst, text)
}

// freshClient mints a token and returns a client for it.
func freshClient(ctx context.Context, d Deps, repo string) (Client, error) {
	token, err := d.mintToken(ctx, repo)
	if err != nil {
		return Client{}, err
	}
	return d.client(token), nil
}

// factoryExit is the exit code of `foreman run <role> <repo> <n>` as the factory user, with the token file
// in place. It is -1 when the run could not be started: that is logged, and the issue must not stay claimed.
func factoryExit(ctx context.Context, d Deps, role Role, repo string, number int) int {
	code, err := runAsFactory(ctx, d, role, repo, number)
	if err != nil {
		d.logf("%v", err)
		return -1
	}
	return code
}

// runAsFactory writes the token where the factory user can read it, runs the factory half, and removes the token.
func runAsFactory(ctx context.Context, d Deps, role Role, repo string, number int) (code int, err error) {
	token, err := d.mintToken(ctx, repo)
	if err != nil {
		return 0, err
	}
	if err := writeTokenFile(d.Settings.TokenFile, token); err != nil {
		return 0, err
	}
	defer func() {
		// The token also dies within the hour; don't leave it lying around.
		if rmErr := os.Remove(d.Settings.TokenFile); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
	}()
	_, runErr := d.Runner.Run(ctx, Cmd{
		Argv:    []string{"sudo", "-n", "-u", "factory", d.Settings.Self, "run", string(role), repo, strconv.Itoa(number)},
		Timeout: AgentTimeout + 2*GitTimeout,
	})
	var ee *ExitError
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
