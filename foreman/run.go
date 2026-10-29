package foreman

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Run is the factory half, run as the factory user: it works the issue as the agent role. For the
// Reviewer the code is the verdict (ReviewClean, ReviewFindings, ReviewExhausted).
func Run(ctx context.Context, d Deps, role Role, repo, model string, number int) (int, error) {
	switch role {
	case Implementer:
		if err := runImplementer(ctx, d, repo, model, number); err != nil {
			return 1, err
		}
		return 0, nil
	case Reviewer:
		return runReviewer(ctx, d, repo, model, number)
	case Planner, Researcher, Curator:
		return 1, fmt.Errorf("%s: %w", role, errNoBehaviour)
	}
	return 1, fmt.Errorf("unknown agent role %q", role)
}

// secrets are the two values the factory half reads at the edge.
type secrets struct {
	githubToken   string
	openrouterKey string
}

// readSecrets reads the token left by `start` for the agent role and the OpenRouter key.
func readSecrets(s Settings, role Role) (secrets, error) {
	token, err := os.ReadFile(s.TokenPath(role))
	if err != nil {
		return secrets{}, fmt.Errorf("read token file: %w", err)
	}
	key, err := os.ReadFile(s.KeyFile)
	if err != nil {
		return secrets{}, fmt.Errorf("read openrouter key: %w", err)
	}
	return secrets{strings.TrimSpace(string(token)), strings.TrimSpace(string(key))}, nil
}

// freshPath is a scratch path outside the clone, empty so a re-run cannot read a stale result.
func freshPath(root, name string) (string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(root, name)
	if err := os.RemoveAll(path); err != nil {
		return "", err
	}
	return path, nil
}

// session is the state of one clone being worked.
type session struct {
	d        Deps
	ctx      context.Context
	workdir  string
	gitEnv   map[string]string
	agentEnv map[string]string
	gitArgv  []string
}

// git runs git in the clone.
func (s session) git(env map[string]string, args ...string) (string, error) {
	argv := append(append([]string{}, s.gitArgv...), args...)
	return s.d.run(s.ctx, Cmd{Argv: argv, Env: env, Capture: true})
}

// head is the clone's current commit.
func (s session) head() (string, error) {
	out, err := s.git(nil, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// clone clones the repository into the session's workdir.
func (s session) clone(repo string) error {
	_, err := s.d.run(s.ctx, Cmd{
		Argv: []string{"git", "clone", "https://github.com/" + repo + ".git", s.workdir}, Env: s.gitEnv,
	})
	return err
}

// opencode runs the model with a prompt in the clone.
func (s session) opencode(model, prompt string, extra []string, timeout time.Duration) error {
	argv := append([]string{"opencode", "run"}, extra...)
	argv = append(argv, "--model", model, prompt)
	_, err := s.d.run(s.ctx, Cmd{Argv: argv, Dir: s.workdir, Env: s.agentEnv, Timeout: timeout})
	return err
}

// newSession prepares the environments for a clone at workdir.
func newSession(ctx context.Context, d Deps, sec secrets, workdir string, identity []string) session {
	gitArgv := append([]string{"git", "-C", workdir}, identity...)
	return session{
		d: d, ctx: ctx, workdir: workdir, gitEnv: GitAuthEnv(sec.githubToken),
		agentEnv: map[string]string{"OPENROUTER_API_KEY": sec.openrouterKey}, gitArgv: gitArgv,
	}
}

// runImplementer clones, runs the model, pushes, opens or updates the pull request, and writes the retro.
func runImplementer(ctx context.Context, d Deps, repo, model string, number int) error {
	sec, err := readSecrets(d.Settings, Implementer)
	if err != nil {
		return err
	}
	c := d.client(sec.githubToken)
	issue, err := c.GetIssue(ctx, repo, number)
	if err != nil {
		return err
	}
	branch := BranchName(number)
	workdir, err := freshPath(d.Settings.WorkRoot, fmt.Sprintf("issue-%d", number)) // a clean slate makes a re-run equal to a first run
	if err != nil {
		return err
	}
	s := newSession(ctx, d, sec, workdir, []string{
		"-c", "user.name=factory-agent", "-c", "user.email=factory-agent@users.noreply.github.com",
	})
	if err := s.clone(repo); err != nil {
		return err
	}
	pr, feedback, err := s.checkoutBranch(c, repo, branch)
	if err != nil {
		return err
	}
	start, err := s.head()
	if err != nil {
		return err
	}
	if err := s.opencode(model, AgentPrompt(repo, issue, feedback), nil, AgentTimeout); err != nil {
		return err
	}
	if err := s.requireNewCommits(start, branch); err != nil {
		return err
	}
	// agent/* branches are ours alone; a first run replaces any stale one, a rework fast-forwards.
	push := []string{"push", "origin", branch}
	if pr == nil {
		push = []string{"push", "--force", "origin", branch}
	}
	if _, err := s.git(s.gitEnv, push...); err != nil {
		return err
	}
	if pr == nil {
		if pr, err = openPR(ctx, c, repo, issue, branch); err != nil {
			return err
		}
	}
	if err := writeLine(d.Out, pr.HTMLURL); err != nil {
		return err
	}
	// The retro is best effort: the PR is already up, so a failed retro is logged, not fatal.
	if err := s.retro(c, repo, model, number); err != nil {
		d.logf("retro failed: %v", err)
	}
	return nil
}

// checkoutBranch checks out the issue's branch. With an open pull request it builds on the reviewed
// commits instead of discarding them, and returns the latest review as feedback.
func (s session) checkoutBranch(c Client, repo, branch string) (*PullRequest, string, error) {
	pr, err := c.ExistingPR(s.ctx, repo, branch)
	if err != nil {
		return nil, "", err
	}
	if pr == nil {
		_, err = s.git(nil, "checkout", "-B", branch)
		return nil, "", err
	}
	if _, err := s.git(s.gitEnv, "fetch", "origin", branch); err != nil {
		return nil, "", err
	}
	if _, err := s.git(nil, "checkout", "-B", branch, "origin/"+branch); err != nil {
		return nil, "", err
	}
	feedback, err := c.LatestReviewBody(s.ctx, repo, pr.Number)
	return pr, feedback, err
}

// requireNewCommits never discards the agent's work silently: no new commits means no PR, and that is a failure.
func (s session) requireNewCommits(start, branch string) error {
	out, err := s.git(nil, "rev-list", "--count", start+".."+branch)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "0" {
		return errors.New("the agent made no commits")
	}
	return nil
}

// openPR opens the pull request for the branch and links it from the issue.
func openPR(ctx context.Context, c Client, repo string, issue Issue, branch string) (*PullRequest, error) {
	base, err := c.DefaultBranch(ctx, repo)
	if err != nil {
		return nil, err
	}
	pr, err := c.CreatePR(ctx, repo, issue.Title, branch, base,
		fmt.Sprintf("Closes #%d\n\nOpened by the factory agent.", issue.Number))
	if err != nil {
		return nil, err
	}
	if err := c.Comment(ctx, repo, issue.Number, "Pull request: "+pr.HTMLURL); err != nil {
		return nil, err
	}
	return &pr, nil
}

// retro resumes the finished session for its retro and posts it on the retro ticket.
func (s session) retro(c Client, repo, model string, number int) error {
	out, err := freshPath(s.d.Settings.WorkRoot, fmt.Sprintf("issue-%d.retro.md", number))
	if err != nil {
		return err
	}
	if err := s.opencode(model, RetroPrompt(repo, number, out), []string{"--continue"}, RetroTimeout); err != nil {
		return err
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return errors.New("the retro wrote nothing")
	}
	sha, err := s.head()
	if err != nil {
		return err
	}
	return c.PostRetro(s.ctx, repo, number, sha, text)
}

// runReviewer clones the pull request's branch, runs the review, posts it, and returns the verdict code.
func runReviewer(ctx context.Context, d Deps, repo, model string, number int) (int, error) {
	sec, err := readSecrets(d.Settings, Reviewer)
	if err != nil {
		return 1, err
	}
	c := d.client(sec.githubToken)
	issue, err := c.GetIssue(ctx, repo, number)
	if err != nil {
		return 1, err
	}
	branch := BranchName(number)
	pr, err := c.ExistingPR(ctx, repo, branch)
	if err != nil {
		return 1, err
	}
	if pr == nil {
		return 1, fmt.Errorf("no open pull request for %s", branch)
	}
	base, err := c.DefaultBranch(ctx, repo)
	if err != nil {
		return 1, err
	}
	paths, err := reviewPaths(d.Settings.WorkRoot, number)
	if err != nil {
		return 1, err
	}
	s := newSession(ctx, d, sec, paths.workdir, nil)
	sha, err := s.checkoutForReview(repo, branch)
	if err != nil {
		return 1, err
	}
	spec := fmt.Sprintf("# %s\n\n%s\n", issue.Title, truncate(issue.Body, BodyLimitChars))
	if err := os.WriteFile(paths.spec, []byte(spec), 0o600); err != nil {
		return 1, err
	}
	if err := s.opencode(model, ReviewPrompt(base, paths.spec, paths.out), nil, ReviewTimeout); err != nil {
		return 1, err
	}
	return postVerdict(ctx, c, repo, pr.Number, sha, paths.out)
}

// reviewFiles are a review run's scratch paths.
type reviewFiles struct {
	workdir, spec, out string
}

// reviewPaths clears and returns the scratch paths of a review run.
func reviewPaths(root string, number int) (reviewFiles, error) {
	var f reviewFiles
	var err error
	for _, p := range []struct {
		dst    *string
		suffix string
	}{{&f.workdir, ""}, {&f.spec, ".spec.md"}, {&f.out, ".json"}} {
		if *p.dst, err = freshPath(root, "review-"+strconv.Itoa(number)+p.suffix); err != nil {
			return reviewFiles{}, err
		}
	}
	return f, nil
}

// checkoutForReview clones the repository on the pull request's branch and returns the reviewed commit.
func (s session) checkoutForReview(repo, branch string) (string, error) {
	if err := s.clone(repo); err != nil {
		return "", err
	}
	if _, err := s.git(s.gitEnv, "fetch", "origin", branch); err != nil {
		return "", err
	}
	if _, err := s.git(nil, "checkout", "-B", branch, "origin/"+branch); err != nil {
		return "", err
	}
	return s.head()
}

// postVerdict parses the review strictly, posts it once per commit, and returns the verdict code.
func postVerdict(ctx context.Context, c Client, repo string, pr int, sha, outPath string) (int, error) {
	raw, err := os.ReadFile(outPath)
	if err != nil {
		return 1, fmt.Errorf("read review: %w", err)
	}
	summary, findings, err := ParseReview(string(raw))
	if err != nil {
		return 1, err
	}
	reviews, err := c.Reviews(ctx, repo, pr)
	if err != nil {
		return 1, err
	}
	bad := Blocking(findings)
	if len(StartsWith(reviews, ReviewMarker(sha))) == 0 { // a repeat of this commit's review posts nothing
		if err := c.PostReview(ctx, repo, pr, sha, RenderReview(sha, summary, findings), len(bad) == 0); err != nil {
			return 1, err
		}
	}
	switch {
	case len(bad) == 0:
		return ReviewClean, nil
	case ReviewRounds(reviews, sha) >= MaxReviewRounds:
		return ReviewExhausted, nil
	}
	return ReviewFindings, nil
}
