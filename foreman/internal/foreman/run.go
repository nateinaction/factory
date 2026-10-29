package foreman

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/prompt"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// Run is the factory half, run as the factory user: it works the issue as the agent role. For the
// Reviewer the code is the verdict (workflow.ReviewClean, ReviewFindings, ReviewExhausted).
func Run(ctx context.Context, d Deps, role workflow.Role, repo string, agent config.AgentConfig, number int) (int, error) {
	switch role {
	case workflow.Implementer:
		if err := runImplementer(ctx, d, repo, agent, number); err != nil {
			return 1, err
		}
		return 0, nil
	case workflow.Reviewer:
		return runReviewer(ctx, d, repo, agent, number)
	case workflow.Planner, workflow.Researcher, workflow.Curator:
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
func readSecrets(s config.Settings, role workflow.Role) (secrets, error) {
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
	return s.d.run(s.ctx, proc.Cmd{Argv: argv, Env: env, Capture: true})
}

// head is the clone's current commit.
func (s session) head() (string, error) {
	out, err := s.git(nil, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// clone clones the repository into the session's workdir.
func (s session) clone(repo string) error {
	_, err := s.d.run(s.ctx, proc.Cmd{
		Argv: []string{"git", "clone", "https://github.com/" + repo + ".git", s.workdir}, Env: s.gitEnv,
	})
	return err
}

// newSession prepares the environments for a clone at workdir; agentEnv is the model process's environment.
func newSession(ctx context.Context, d Deps, sec secrets, workdir string, identity []string, agentEnv map[string]string) session {
	gitArgv := append([]string{"git", "-C", workdir}, identity...)
	return session{d: d, ctx: ctx, workdir: workdir, gitEnv: GitAuthEnv(sec.githubToken), agentEnv: agentEnv, gitArgv: gitArgv}
}

// runImplementer works the issue in a fresh state directory and logs the run's one usage line, even
// when the run failed.
func runImplementer(ctx context.Context, d Deps, repo string, agent config.AgentConfig, number int) error {
	var usage opencode.Usage
	err := withState(d.Settings.WorkRoot, workflow.Implementer, number, func(db string) error {
		var err error
		usage, err = implement(ctx, d, repo, agent, number, db)
		return err
	})
	d.logUsage(opencode.NewUsageReport(workflow.Implementer, repo, number, d.Host, agent, usage))
	return err
}

// implement clones, runs the model, pushes, opens or updates the pull request, and writes the retro.
func implement(ctx context.Context, d Deps, repo string, agent config.AgentConfig, number int, db string) (opencode.Usage, error) {
	sec, err := readSecrets(d.Settings, workflow.Implementer)
	if err != nil {
		return opencode.Usage{}, err
	}
	env, err := opencode.Env(sec.openrouterKey, db, agent)
	if err != nil {
		return opencode.Usage{}, err
	}
	c := d.client(sec.githubToken)
	issue, err := c.GetIssue(ctx, repo, number)
	if err != nil {
		return opencode.Usage{}, err
	}
	branch := workflow.BranchName(number)
	workdir, err := freshPath(d.Settings.WorkRoot, fmt.Sprintf("issue-%d", number)) // a clean slate makes a re-run equal to a first run
	if err != nil {
		return opencode.Usage{}, err
	}
	s := newSession(ctx, d, sec, workdir, []string{
		"-c", "user.name=factory-agent", "-c", "user.email=factory-agent@users.noreply.github.com",
	}, env)
	if err := s.clone(repo); err != nil {
		return opencode.Usage{}, err
	}
	pr, feedback, err := s.checkoutBranch(c, repo, branch)
	if err != nil {
		return opencode.Usage{}, err
	}
	start, err := s.head()
	if err != nil {
		return opencode.Usage{}, err
	}
	docs, err := prompt.ReadRepoDocs(workdir)
	if err != nil {
		return opencode.Usage{}, err
	}
	res, err := s.agent(opencode.Run{Role: workflow.Implementer, Prompt: prompt.Implementer(docs, repo, issue, feedback), Timeout: AgentTimeout})
	if err != nil {
		return res.Usage, err
	}
	if err := s.requireNewCommits(start, branch); err != nil {
		return res.Usage, err
	}
	// agent/* branches are ours alone; a first run replaces any stale one, a rework fast-forwards.
	push := []string{"push", "origin", branch}
	if pr == nil {
		push = []string{"push", "--force", "origin", branch}
	}
	if _, err := s.git(s.gitEnv, push...); err != nil {
		return res.Usage, err
	}
	if pr == nil {
		if pr, err = openPR(ctx, c, repo, issue, branch); err != nil {
			return res.Usage, err
		}
	}
	if err := writeLine(d.Out, pr.HTMLURL); err != nil {
		return res.Usage, err
	}
	// The retro is best effort: the PR is already up, so a failed retro is logged, not fatal.
	retroUsage, err := s.retro(c, repo, number, res.Session)
	if err != nil {
		d.logf("retro failed: %v", err)
	}
	return res.Usage.Add(retroUsage), nil
}

// checkoutBranch checks out the issue's branch. With an open pull request it builds on the reviewed
// commits instead of discarding them, and returns the latest review as feedback.
func (s session) checkoutBranch(c github.Client, repo, branch string) (*github.PullRequest, string, error) {
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
func openPR(ctx context.Context, c github.Client, repo string, issue workflow.Issue, branch string) (*github.PullRequest, error) {
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

// retro resumes the finished session, by its explicit id, for its retro and posts it on the retro ticket.
func (s session) retro(c github.Client, repo string, number int, session string) (opencode.Usage, error) {
	if session == "" {
		return opencode.Usage{}, errors.New("the run reported no session to resume")
	}
	out, err := freshPath(s.d.Settings.WorkRoot, fmt.Sprintf("issue-%d.retro.md", number))
	if err != nil {
		return opencode.Usage{}, err
	}
	res, err := s.agent(opencode.Run{Role: workflow.Implementer, Prompt: prompt.Retro(repo, number, out), Session: session, Timeout: RetroTimeout})
	if err != nil {
		return res.Usage, err
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		return res.Usage, err
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return res.Usage, errors.New("the retro wrote nothing")
	}
	sha, err := s.head()
	if err != nil {
		return res.Usage, err
	}
	return res.Usage, c.PostRetro(s.ctx, repo, number, sha, text)
}

// runReviewer reviews in memory-only state and logs the run's one usage line, even when the run failed.
func runReviewer(ctx context.Context, d Deps, repo string, agent config.AgentConfig, number int) (int, error) {
	var usage opencode.Usage
	var code int
	err := withState(d.Settings.WorkRoot, workflow.Reviewer, number, func(db string) error {
		var err error
		code, usage, err = review(ctx, d, repo, agent, number, db)
		return err
	})
	d.logUsage(opencode.NewUsageReport(workflow.Reviewer, repo, number, d.Host, agent, usage))
	if err != nil && code == 0 {
		code = 1
	}
	return code, err
}

// review clones the pull request's branch, runs the review, posts it, and returns the verdict code.
func review(ctx context.Context, d Deps, repo string, agent config.AgentConfig, number int, db string) (int, opencode.Usage, error) {
	sec, err := readSecrets(d.Settings, workflow.Reviewer)
	if err != nil {
		return 1, opencode.Usage{}, err
	}
	env, err := opencode.Env(sec.openrouterKey, db, agent)
	if err != nil {
		return 1, opencode.Usage{}, err
	}
	c := d.client(sec.githubToken)
	issue, err := c.GetIssue(ctx, repo, number)
	if err != nil {
		return 1, opencode.Usage{}, err
	}
	branch := workflow.BranchName(number)
	pr, err := c.ExistingPR(ctx, repo, branch)
	if err != nil {
		return 1, opencode.Usage{}, err
	}
	if pr == nil {
		return 1, opencode.Usage{}, fmt.Errorf("no open pull request for %s", branch)
	}
	base, err := c.DefaultBranch(ctx, repo)
	if err != nil {
		return 1, opencode.Usage{}, err
	}
	paths, err := reviewPaths(d.Settings.WorkRoot, number)
	if err != nil {
		return 1, opencode.Usage{}, err
	}
	s := newSession(ctx, d, sec, paths.workdir, nil, env)
	sha, err := s.checkoutForReview(repo, branch)
	if err != nil {
		return 1, opencode.Usage{}, err
	}
	spec := fmt.Sprintf("# %s\n\n%s\n", issue.Title, workflow.Truncate(issue.Body, workflow.BodyLimitChars))
	if err := os.WriteFile(paths.spec, []byte(spec), 0o600); err != nil {
		return 1, opencode.Usage{}, err
	}
	docs, err := prompt.ReadRepoDocs(paths.workdir)
	if err != nil {
		return 1, opencode.Usage{}, err
	}
	res, err := s.agent(opencode.Run{Role: workflow.Reviewer, Prompt: prompt.Reviewer(docs, base, paths.spec, paths.out), Timeout: ReviewTimeout})
	if err != nil {
		return 1, res.Usage, err
	}
	code, err := postVerdict(ctx, c, repo, pr.Number, sha, paths.out)
	return code, res.Usage, err
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
func postVerdict(ctx context.Context, c github.Client, repo string, pr int, sha, outPath string) (int, error) {
	raw, err := os.ReadFile(outPath)
	if err != nil {
		return 1, fmt.Errorf("read review: %w", err)
	}
	summary, findings, err := workflow.ParseReview(string(raw))
	if err != nil {
		return 1, err
	}
	reviews, err := c.Reviews(ctx, repo, pr)
	if err != nil {
		return 1, err
	}
	bad := workflow.Blocking(findings)
	if len(workflow.StartsWith(reviews, workflow.ReviewMarker(sha))) == 0 { // a repeat of this commit's review posts nothing
		if err := c.PostReview(ctx, repo, pr, sha, workflow.RenderReview(sha, summary, findings), len(bad) == 0); err != nil {
			return 1, err
		}
	}
	switch {
	case len(bad) == 0:
		return workflow.ReviewClean, nil
	case workflow.ReviewRounds(reviews, sha) >= workflow.MaxReviewRounds:
		return workflow.ReviewExhausted, nil
	}
	return workflow.ReviewFindings, nil
}
