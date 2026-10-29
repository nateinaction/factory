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
	"github.com/nateinaction/factory/foreman/internal/openrouter"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/prompt"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// Run is the factory half, run as the factory user: it works the issue as the agent role. For the
// Reviewer the code is the verdict (workflow.ReviewClean, ReviewFindings, ReviewExhausted).
func Run(
	ctx context.Context, d Deps, role workflow.Role, repo string, agent config.AgentConfig, number int,
) (int, error) {
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
	if err := os.MkdirAll(root, privateDirMode); err != nil {
		return "", fmt.Errorf("scratch directory: %w", err)
	}

	path := filepath.Join(root, name)
	if err := os.RemoveAll(path); err != nil {
		return "", fmt.Errorf("clear scratch: %w", err)
	}

	return path, nil
}

// readScratch reads a file the model wrote to a scratch path. The model controls what is there, so the
// read goes through os.Root: a symlink out of the scratch directory is refused, not followed.
func readScratch(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("open scratch: %w", err)
	}

	raw, err := root.ReadFile(filepath.Base(path))

	return raw, errors.Join(wrapErr("read scratch", err), wrapErr("close scratch", root.Close()))
}

// wrapErr wraps err with what was being done, and keeps nil as nil.
func wrapErr(doing string, err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("%s: %w", doing, err)
}

// session is the state of one clone being worked.
type session struct {
	d        Deps
	workdir  string
	gitEnv   map[string]string
	agentEnv map[string]string
	gitArgv  []string
	// key is the OpenRouter key, kept only to scrub it from anything echoed. The model never has it.
	key string
}

// git runs git in the clone.
func (s session) git(ctx context.Context, env map[string]string, args ...string) (string, error) {
	argv := append(append([]string{}, s.gitArgv...), args...)
	return s.d.run(ctx, proc.Cmd{Argv: argv, Env: env, Capture: true})
}

// head is the clone's current commit.
func (s session) head(ctx context.Context) (string, error) {
	out, err := s.git(ctx, nil, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// clone clones the repository into the session's workdir.
func (s session) clone(ctx context.Context, repo string) error {
	_, err := s.d.run(ctx, proc.Cmd{
		Argv: []string{"git", "clone", "https://github.com/" + repo + ".git", s.workdir}, Env: s.gitEnv,
	})

	return err
}

// newSession prepares the environments for a clone at workdir; agentEnv is the model process's
// environment.
func newSession(d Deps, j job, workdir string, identity []string) session {
	return session{
		d:        d,
		workdir:  workdir,
		gitEnv:   GitAuthEnv(j.sec.githubToken),
		agentEnv: j.env,
		gitArgv:  append([]string{"git", "-C", workdir}, identity...),
		key:      j.sec.openrouterKey,
	}
}

// job is what an agent role reads before it touches a clone: its secrets, the model's environment, a
// GitHub client and the issue.
type job struct {
	sec   secrets
	env   map[string]string
	c     github.Client
	issue workflow.Issue
}

// loadJob reads the issue an agent role works and builds its model environment around the run's
// proxy. The secrets are already read: the key belongs to the proxy, never to the model.
func loadJob(
	ctx context.Context, d Deps, sec secrets, baseURL string,
	repo string, agent config.AgentConfig, number int, db string,
) (job, error) {
	env, err := opencode.Env(baseURL, db, agent)
	if err != nil {
		return job{}, err
	}

	c := d.client(sec.githubToken)

	issue, err := c.GetIssue(ctx, repo, number)
	if err != nil {
		return job{}, err
	}

	return job{sec: sec, env: env, c: c, issue: issue}, nil
}

// withProxy runs fn with the run's OpenRouter proxy up and stops it when fn returns, folding the
// usage the proxy read into the run's usage. A proxy that cannot start fails the run: the model has
// no other way to reach OpenRouter, and there is never an unsandboxed fallback.
func (d Deps) withProxy(
	key string, fn func(baseURL string) (opencode.Usage, error),
) (opencode.Usage, error) {
	p, err := d.proxy(key)
	if err != nil {
		return opencode.Usage{}, err
	}

	usage, err := fn(p.URL())
	// Read the usage before stopping: a response still being relayed when the model exits cannot be
	// counted, and Close is what ends it.
	read := p.Usage()
	// A failure to stop the proxy is part of the run's outcome, not a diagnostic to drop.
	err = errors.Join(err, p.Close())

	return usage.Use(reported(read)), err
}

// proxy starts the run's OpenRouter proxy with key on the configured loopback address.
func (d Deps) proxy(key string) (Forwarder, error) {
	start := d.Proxy
	if start == nil {
		start = func(key, addr string) (Forwarder, error) { return openrouter.Start(key, addr) }
	}

	addr := d.ProxyAddr
	if addr == "" {
		addr = openrouter.Loopback
	}

	return start(key, addr)
}

// reported is the proxy's usage as the adapter's type.
func reported(u openrouter.Usage) opencode.Usage {
	return opencode.Usage{Input: u.Input, Output: u.Output, Cached: u.Cached, Cost: u.Cost}
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
func implement(
	ctx context.Context, d Deps, repo string, agent config.AgentConfig, number int, db string,
) (opencode.Usage, error) {
	sec, err := readSecrets(d.Settings, workflow.Implementer)
	if err != nil {
		return opencode.Usage{}, err
	}

	return d.withProxy(sec.openrouterKey, func(baseURL string) (opencode.Usage, error) {
		return implementRun(ctx, d, sec, baseURL, repo, agent, number, db)
	})
}

// implementRun is the Implementer's work, with its proxy already up.
func implementRun(
	ctx context.Context, d Deps, sec secrets, baseURL, repo string,
	agent config.AgentConfig, number int, db string,
) (opencode.Usage, error) {
	j, err := loadJob(ctx, d, sec, baseURL, repo, agent, number, db)
	if err != nil {
		return opencode.Usage{}, err
	}

	branch := workflow.BranchName(number)

	// A clean slate makes a re-run equal to a first run.
	workdir, err := freshPath(d.Settings.WorkRoot, fmt.Sprintf("issue-%d", number))
	if err != nil {
		return opencode.Usage{}, err
	}

	s := newSession(d, j, workdir, []string{
		"-c", "user.name=factory-agent", "-c", "user.email=factory-agent@users.noreply.github.com",
	})

	pr, feedback, start, err := s.prepare(ctx, j.c, repo, branch)
	if err != nil {
		return opencode.Usage{}, err
	}

	docs, err := prompt.ReadRepoDocs(workdir)
	if err != nil {
		return opencode.Usage{}, err
	}

	res, err := s.agent(ctx, opencode.Run{
		Role: workflow.Implementer, Prompt: prompt.Implementer(docs, repo, j.issue, feedback), Timeout: AgentTimeout,
	})
	if err != nil {
		return res.Usage, err
	}

	if err := s.publish(ctx, j, repo, branch, start, pr); err != nil {
		return res.Usage, err
	}
	// The retro is best effort: the PR is already up, so a failed retro is logged, not fatal.
	retroUsage, err := s.retro(ctx, j.c, repo, number, res.Session)
	if err != nil {
		d.log().Warn("retro failed", "repo", repo, "issue", number, "error", err)
	}

	return res.Usage.Add(retroUsage), nil
}

// prepare clones the repository and checks out the issue's branch. It returns the open pull request, if
// any, the review feedback on it, and the commit the agent starts from.
func (s session) prepare(
	ctx context.Context, c github.Client, repo, branch string,
) (pr *github.PullRequest, feedback, start string, err error) {
	if err := s.clone(ctx, repo); err != nil {
		return nil, "", "", err
	}

	pr, feedback, err = s.checkoutBranch(ctx, c, repo, branch)
	if err != nil {
		return nil, "", "", err
	}

	start, err = s.head(ctx)

	return pr, feedback, start, err
}

// publish pushes the agent's new commits, opens the pull request if there is none, and prints its URL.
func (s session) publish(ctx context.Context, j job, repo, branch, start string, pr *github.PullRequest) error {
	if err := s.requireNewCommits(ctx, start, branch); err != nil {
		return err
	}
	// agent/* branches are ours alone; a first run replaces any stale one, a rework fast-forwards.
	push := []string{"push", "origin", branch}
	if pr == nil {
		push = []string{"push", "--force", "origin", branch}
	}

	if _, err := s.git(ctx, s.gitEnv, push...); err != nil {
		return err
	}

	if pr == nil {
		var err error
		if pr, err = openPR(ctx, j.c, repo, j.issue, branch); err != nil {
			return err
		}
	}

	return writeLine(s.d.Out, pr.HTMLURL)
}

// checkoutBranch checks out the issue's branch. With an open pull request it builds on the reviewed
// commits instead of discarding them, and returns the latest review as feedback.
func (s session) checkoutBranch(
	ctx context.Context, c github.Client, repo, branch string,
) (*github.PullRequest, string, error) {
	pr, err := c.ExistingPR(ctx, repo, branch)
	if err != nil {
		return nil, "", err
	}

	if pr == nil {
		_, err = s.git(ctx, nil, "checkout", "-B", branch)
		return nil, "", err
	}

	if _, err := s.git(ctx, s.gitEnv, "fetch", "origin", branch); err != nil {
		return nil, "", err
	}

	if _, err := s.git(ctx, nil, "checkout", "-B", branch, "origin/"+branch); err != nil {
		return nil, "", err
	}

	feedback, err := c.LatestReviewBody(ctx, repo, pr.Number)

	return pr, feedback, err
}

// requireNewCommits never discards the agent's work silently: no new commits means no PR, and that is a
// failure.
func (s session) requireNewCommits(ctx context.Context, start, branch string) error {
	out, err := s.git(ctx, nil, "rev-list", "--count", start+".."+branch)
	if err != nil {
		return err
	}

	if strings.TrimSpace(out) == "0" {
		return errors.New("the agent made no commits")
	}

	return nil
}

// openPR opens the pull request for the branch and links it from the issue.
func openPR(
	ctx context.Context, c github.Client, repo string, issue workflow.Issue, branch string,
) (*github.PullRequest, error) {
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
func (s session) retro(
	ctx context.Context, c github.Client, repo string, number int, session string,
) (opencode.Usage, error) {
	if session == "" {
		return opencode.Usage{}, errors.New("the run reported no session to resume")
	}

	out, err := freshPath(s.d.Settings.WorkRoot, fmt.Sprintf("issue-%d.retro.md", number))
	if err != nil {
		return opencode.Usage{}, err
	}

	res, err := s.agent(ctx, opencode.Run{
		Role: workflow.Implementer, Prompt: prompt.Retro(repo, number, out), Session: session, Timeout: RetroTimeout,
	})
	if err != nil {
		return res.Usage, err
	}

	raw, err := readScratch(out)
	if err != nil {
		return res.Usage, err
	}

	text := strings.TrimSpace(string(raw))
	if text == "" {
		return res.Usage, errors.New("the retro wrote nothing")
	}

	sha, err := s.head(ctx)
	if err != nil {
		return res.Usage, err
	}

	return res.Usage, c.PostRetro(ctx, repo, number, sha, text)
}

// runReviewer reviews in memory-only state and logs the run's one usage line, even when the run failed.
func runReviewer(ctx context.Context, d Deps, repo string, agent config.AgentConfig, number int) (int, error) {
	var (
		usage opencode.Usage
		code  int
	)

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
func review(
	ctx context.Context, d Deps, repo string, agent config.AgentConfig, number int, db string,
) (int, opencode.Usage, error) {
	sec, err := readSecrets(d.Settings, workflow.Reviewer)
	if err != nil {
		return 1, opencode.Usage{}, err
	}

	var code int

	usage, err := d.withProxy(sec.openrouterKey, func(baseURL string) (opencode.Usage, error) {
		runCode, u, e := reviewRun(ctx, d, sec, baseURL, repo, agent, number, db)
		code = runCode

		return u, e
	})

	return code, usage, err
}

// reviewRun is the Reviewer's work, with its proxy already up.
func reviewRun(
	ctx context.Context, d Deps, sec secrets, baseURL, repo string,
	agent config.AgentConfig, number int, db string,
) (int, opencode.Usage, error) {
	j, err := loadJob(ctx, d, sec, baseURL, repo, agent, number, db)
	if err != nil {
		return 1, opencode.Usage{}, err
	}

	pr, base, err := reviewTarget(ctx, j.c, repo, workflow.BranchName(number))
	if err != nil {
		return 1, opencode.Usage{}, err
	}

	paths, err := reviewPaths(d.Settings.WorkRoot, number)
	if err != nil {
		return 1, opencode.Usage{}, err
	}

	s := newSession(d, j, paths.workdir, nil)

	sha, err := s.checkoutForReview(ctx, repo, workflow.BranchName(number))
	if err != nil {
		return 1, opencode.Usage{}, err
	}

	docs, err := writeSpec(paths, j.issue)
	if err != nil {
		return 1, opencode.Usage{}, err
	}

	res, err := s.agent(ctx, opencode.Run{
		Role: workflow.Reviewer, Prompt: prompt.Reviewer(docs, base, paths.spec, paths.out), Timeout: ReviewTimeout,
	})
	if err != nil {
		return 1, res.Usage, err
	}

	code, err := postVerdict(ctx, j.c, repo, pr.Number, sha, paths.out)

	return code, res.Usage, err
}

// reviewTarget is the open pull request for branch and the branch it merges into.
func reviewTarget(ctx context.Context, c github.Client, repo, branch string) (*github.PullRequest, string, error) {
	pr, err := c.ExistingPR(ctx, repo, branch)
	if err != nil {
		return nil, "", err
	}

	if pr == nil {
		return nil, "", fmt.Errorf("no open pull request for %s", branch)
	}

	base, err := c.DefaultBranch(ctx, repo)

	return pr, base, err
}

// writeSpec writes the issue where the Reviewer reads it, outside the clone, and returns the clone's
// repository docs.
func writeSpec(paths reviewFiles, issue workflow.Issue) ([]prompt.Doc, error) {
	spec := fmt.Sprintf("# %s\n\n%s\n", issue.Title, workflow.Truncate(issue.Body, workflow.BodyLimitChars))
	if err := os.WriteFile(paths.spec, []byte(spec), privateFileMode); err != nil {
		return nil, fmt.Errorf("write spec: %w", err)
	}

	return prompt.ReadRepoDocs(paths.workdir)
}

// reviewFiles are a review run's scratch paths.
type reviewFiles struct {
	workdir, spec, out string
}

// reviewPaths clears and returns the scratch paths of a review run.
func reviewPaths(root string, number int) (reviewFiles, error) {
	var (
		f   reviewFiles
		err error
	)
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
func (s session) checkoutForReview(ctx context.Context, repo, branch string) (string, error) {
	if err := s.clone(ctx, repo); err != nil {
		return "", err
	}

	if _, err := s.git(ctx, s.gitEnv, "fetch", "origin", branch); err != nil {
		return "", err
	}

	if _, err := s.git(ctx, nil, "checkout", "-B", branch, "origin/"+branch); err != nil {
		return "", err
	}

	return s.head(ctx)
}

// postVerdict parses the review strictly, posts it once per commit, and returns the verdict code.
func postVerdict(ctx context.Context, c github.Client, repo string, pr int, sha, outPath string) (int, error) {
	raw, err := readScratch(outPath)
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
	// A repeat of this commit's review posts nothing.
	if len(workflow.StartsWith(reviews, workflow.ReviewMarker(sha))) == 0 {
		body := workflow.RenderReview(sha, summary, findings)
		if err := c.PostReview(ctx, repo, pr, sha, body, len(bad) == 0); err != nil {
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
