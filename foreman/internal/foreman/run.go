package foreman

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/egress"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/openrouter"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/prompt"
	"github.com/nateinaction/factory/foreman/internal/sandbox"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// Run is the factory half, run as the factory user: it works the issue as the agent role. For the
// Reviewer the code is the verdict (workflow.ReviewClean, ReviewFindings, ReviewExhausted); a run
// that already reported its failure on the issue returns workflow.ExitReported.
func Run(
	ctx context.Context, d Deps, role workflow.Role, repo string, agent config.AgentConfig, retro config.Budget,
	number int,
) (int, error) {
	// A host the sandbox adapter cannot serve does no work: there is never an unsandboxed fallback.
	if err := sandbox.Supported(runtime.GOOS); err != nil {
		return 1, err
	}

	switch role {
	case workflow.Planner:
		return exitCode(runPlanner(ctx, d, repo, agent, retro, number), 0)
	case workflow.Implementer:
		return exitCode(runImplementer(ctx, d, repo, agent, retro, number), 0)
	case workflow.Researcher:
		return exitCode(runResearcher(ctx, d, repo, agent, retro, number), 0)
	case workflow.Reviewer:
		code, err := runReviewer(ctx, d, repo, agent, retro, number)

		return exitCode(err, code)
	case workflow.Curator:
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
	// srt is the sandbox backend every model process runs under, and settings is the run's policy.
	// Both are required: a session with either missing refuses to start the model.
	srt      string
	settings string
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
// environment. Every git call runs with the clone's hooks disabled, so nothing the model wrote can
// run outside the sandbox; identity adds the committer a run needs.
func newSession(d Deps, j job, workdir string, identity []string) session {
	gitArgv := append([]string{"git", "-C", workdir, "-c", "core.hooksPath=" + nullHooksPath}, identity...)

	return session{
		d:        d,
		workdir:  workdir,
		gitEnv:   GitAuthEnv(j.sec.githubToken),
		agentEnv: j.env,
		gitArgv:  gitArgv,
		key:      j.sec.openrouterKey,
	}
}

// sandboxed puts the model process inside the run's sandbox: its own HOME and XDG directories, the
// default branch's toolchain, and the srt settings compiled from the role's spec.
func (s session) sandboxed(f sandboxFiles, spec sandbox.Spec, toolchainPath string, proxyPort int) (session, error) {
	if err := f.writeSettings(spec, proxyPort); err != nil {
		return session{}, err
	}

	env := f.env(toolchainPath)
	maps.Copy(env, s.agentEnv)

	s.srt = s.d.Settings.Srt
	s.settings = f.settings
	s.agentEnv = env

	return s, nil
}

// nullHooksPath disables every git hook of the clone.
const nullHooksPath = "/dev/null"

// gitRemote is the remote foreman fetches from and pushes to.
const gitRemote = "origin"

// gitPush is git's push subcommand.
const gitPush = "push"

// job is what an agent role reads before it touches a clone: its secrets, the model's environment, a
// GitHub client and the issue.
type job struct {
	sec   secrets
	env   map[string]string
	c     github.Client
	issue workflow.Issue
}

// loadJob reads the issue an agent role works and builds its model environment around the run's
// proxy. The secrets are already read: the key belongs to the proxy, never to the model. The model's
// one GitHub credential is a read-only token of its own App's, scoped to the repository.
func loadJob(
	ctx context.Context, d Deps, sec secrets, baseURL, repo string, role workflow.Role,
	agent config.AgentConfig, number int, db string,
) (job, error) {
	env, err := opencode.Env(baseURL, db, agent)
	if err != nil {
		return job{}, err
	}

	readOnly, err := d.mintReadOnlyToken(ctx, role, repo)
	if err != nil {
		return job{}, err
	}

	env["GH_TOKEN"] = readOnly

	c := d.client(sec.githubToken)

	issue, err := c.GetIssue(ctx, repo, number)
	if err != nil {
		return job{}, err
	}

	return job{sec: sec, env: env, c: c, issue: issue}, nil
}

// withProxy runs fn with the run's OpenRouter key proxy and its egress proxy up, and stops both when
// fn returns. The usage the key proxy read and the domains the egress proxy saw are the run's. A
// proxy that cannot start fails the run: the model has no other way to reach OpenRouter, and there is
// never an unsandboxed fallback.
func (d Deps) withProxy(
	ctx context.Context, network sandbox.Network, key string,
	fn func(baseURL, proxyAddr string) (opencode.Usage, error),
) (opencode.Usage, []string, error) {
	p, err := d.proxy(key)
	if err != nil {
		return opencode.Usage{}, nil, err
	}

	e, err := d.egress(ctx, network, p.URL())
	if err != nil {
		return opencode.Usage{}, nil, errors.Join(err, p.Close())
	}

	usage, err := fn(e.URL(), e.Address())
	// Read the usage before stopping: a response still being relayed when the model exits cannot be
	// counted, and Close is what ends it.
	read := p.Usage()
	domains := e.Domains()
	// A failure to stop either proxy is part of the run's outcome, not a diagnostic to drop.
	err = errors.Join(err, e.Close(), p.Close())

	return usage.Use(reported(read)), domains, err
}

// egress starts the run's egress proxy with the role's policy, forwarding its API calls to apiURL.
func (d Deps) egress(ctx context.Context, policy sandbox.Network, apiURL string) (EgressProxy, error) {
	start := d.Egress
	if start == nil {
		start = func(ctx context.Context, policy sandbox.Network, apiURL string) (EgressProxy, error) {
			return egress.Start(ctx, policy, apiURL, d.egressAddr())
		}
	}

	return start(ctx, policy, apiURL)
}

// egressAddr is the loopback address the egress proxy binds.
func (d Deps) egressAddr() string {
	if d.EgressAddr == "" {
		return egress.Loopback
	}

	return d.EgressAddr
}

// proxyPort is the port of a loopback host:port, or zero when it cannot be parsed. A zero port fails
// the settings compile, so the run never starts without its egress boundary.
func proxyPort(addr string) int {
	parsed, err := url.Parse("http://" + addr)
	if err != nil {
		return 0
	}

	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		return 0
	}

	return port
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

// reportedError is a failure whose reason foreman already posted on the issue in one comment. The
// start half only records the failure, so the output errors are never described twice.
type reportedError struct {
	// cause is the failure that was reported.
	cause error
}

// Error is the reported failure's message.
func (e *reportedError) Error() string { return e.cause.Error() }

// Unwrap exposes the reported failure.
func (e *reportedError) Unwrap() error { return e.cause }

// exitCode maps a finished run to the code `start` reads: a reported failure becomes ExitReported,
// because the factory half already commented; code is what a successful run returns. The error is
// kept so the dagu log records it too.
func exitCode(err error, code int) (int, error) {
	if _, ok := errors.AsType[*reportedError](err); ok {
		return workflow.ExitReported, err
	}

	if err != nil {
		return 1, err
	}

	return code, nil
}

// runImplementer works the issue in a fresh run directory and logs the run's one usage line, even
// when the run failed.
func runImplementer(
	ctx context.Context, d Deps, repo string, agent config.AgentConfig, retro config.Budget, number int,
) (err error) {
	var (
		usage   opencode.Usage
		domains []string
	)

	files, err := newSandboxFiles(d.Settings.WorkRoot, workflow.Implementer, number)
	if err != nil {
		return err
	}

	defer func() { err = errors.Join(err, files.remove()) }()

	usage, domains, err = implement(ctx, d, repo, agent, retro, number, files)
	d.logUsage(opencode.NewUsageReport(workflow.Implementer, repo, number, d.Host, agent, usage).
		WithDomains(domains))

	return err
}

// implement clones, runs the model under the sandbox, applies its output, and writes the retro.
func implement(
	ctx context.Context, d Deps, repo string, agent config.AgentConfig, retro config.Budget, number int,
	files sandboxFiles,
) (opencode.Usage, []string, error) {
	network, err := sandbox.ParseNetwork(agent.Egress.AllowedDomains)
	if err != nil {
		return opencode.Usage{}, nil, err
	}

	sec, err := readSecrets(d.Settings, workflow.Implementer)
	if err != nil {
		return opencode.Usage{}, nil, err
	}

	return d.withProxy(ctx, network, sec.openrouterKey, func(baseURL, proxyAddr string) (opencode.Usage, error) {
		return implementRun(ctx, d, sec, baseURL, proxyAddr, network, repo, agent, retro, number, files)
	})
}

// implementRun is the Implementer's work, with its proxies already up.
func implementRun(
	ctx context.Context, d Deps, sec secrets, baseURL, proxyAddr string, network sandbox.Network,
	repo string, agent config.AgentConfig, retro config.Budget, number int, files sandboxFiles,
) (opencode.Usage, error) {
	j, err := loadJob(ctx, d, sec, baseURL, repo, workflow.Implementer, agent, number,
		files.db(workflow.Implementer))
	if err != nil {
		return opencode.Usage{}, err
	}

	branch := workflow.BranchName(number)

	paths, err := implementerPaths(d.Settings.WorkRoot, number)
	if err != nil {
		return opencode.Usage{}, err
	}

	s := newSession(d, j, paths.workdir, []string{
		"-c", "user.name=factory-agent", "-c", "user.email=factory-agent@users.noreply.github.com",
	})

	pr, feedback, start, err := s.prepare(ctx, j.c, repo, branch)
	if err != nil {
		return opencode.Usage{}, err
	}

	implementerPrompt, err := buildImplementerPrompt(ctx, j.c, repo, j.issue, paths.workdir, feedback, paths.out)
	if err != nil {
		return opencode.Usage{}, err
	}

	if s, err = d.sandboxedWithToolchain(
		ctx, s, j, sec, repo, "", files, workflow.Implementer, paths.workdir, paths.out,
		[]string{retroPath(d.Settings.WorkRoot, number, workflow.Implementer)}, network, proxyAddr,
	); err != nil {
		return opencode.Usage{}, err
	}

	res, err := s.agent(ctx, opencode.Run{
		Role: workflow.Implementer, Prompt: implementerPrompt,
		Timeout: agent.Deadline(), Steps: agent.Steps,
	})
	if err != nil {
		return res.Usage.Add(s.retroEpilogue(
			ctx, j, repo, number, workflow.Implementer, res.Session, s.headKey(ctx), err, retro,
		)), err
	}

	out, err := readImplementerOutput(paths.out, pr != nil)
	if err != nil {
		return res.Usage, rejectOutput(ctx, j.c, repo, number, workflow.Implementer, err)
	}

	if err := s.applyImplementer(ctx, j, repo, branch, start, pr, out); err != nil {
		return res.Usage.Add(s.retroEpilogue(
			ctx, j, repo, number, workflow.Implementer, res.Session, s.headKey(ctx), err, retro,
		)), err
	}
	// The output is applied: the retro is the run's epilogue and never changes its outcome.
	return res.Usage.Add(s.retroEpilogue(
		ctx, j, repo, number, workflow.Implementer, res.Session, s.headKey(ctx), nil, retro,
	)), nil
}

// buildImplementerPrompt builds the Implementer's prompt from the clone's docs, the review feedback
// and the comments that woke the issue.
func buildImplementerPrompt(
	ctx context.Context, c github.Client, repo string, issue workflow.Issue, workdir, feedback, out string,
) (string, error) {
	docs, err := prompt.ReadRepoDocs(workdir)
	if err != nil {
		return "", err
	}

	thread, err := guidance(ctx, c, repo, issue.Number)
	if err != nil {
		return "", err
	}

	return prompt.Implementer(docs, repo, issue, feedback, thread, out), nil
}

// guidance reads every comment since the marker that woke the issue, from the issue and its pull
// request, oldest first. An issue that was not woken reads nothing.
func guidance(ctx context.Context, c github.Client, repo string, number int) ([]workflow.Item, error) {
	comments, err := c.ListComments(ctx, repo, number)
	if err != nil {
		return nil, err
	}

	pr, err := c.ExistingPR(ctx, repo, workflow.BranchName(number))
	if err != nil {
		return nil, err
	}

	if pr != nil {
		onPR, err := c.ListComments(ctx, repo, pr.Number)
		if err != nil {
			return nil, err
		}

		comments = append(comments, onPR...)
	}

	slices.SortStableFunc(comments, func(a, b workflow.Item) int {
		return cmp.Compare(a.ID, b.ID)
	})

	return workflow.Guidance(comments), nil
}

// implementerPaths clears and returns an Implementer run's clone and scratch paths. The output file
// sits outside the clone, in the run's own scratch directory, with the name the spec fixes.
func implementerPaths(root string, number int) (runFiles, error) {
	workdir, err := freshPath(root, fmt.Sprintf("issue-%d", number))
	if err != nil {
		return runFiles{}, err
	}

	scratch, err := scratchDir(root, fmt.Sprintf("implementer-%d", number))
	if err != nil {
		return runFiles{}, err
	}

	return runFiles{workdir: workdir, out: filepath.Join(scratch, workflow.ImplementerOutputFile)}, nil
}

// runFiles are the clone and output path of an Implementer run.
type runFiles struct {
	workdir, out string
}

// scratchDir clears and creates one run's scratch directory, which is outside the clone.
func scratchDir(root, name string) (string, error) {
	path, err := freshPath(root, name)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(path, privateDirMode); err != nil {
		return "", fmt.Errorf("scratch directory: %w", err)
	}

	return path, nil
}

// readImplementerOutput reads and validates the Implementer's output file. A file foreman cannot
// read is an output error too, so the model that wrote none fails the same way as a bad one.
func readImplementerOutput(path string, rework bool) (workflow.ImplementerOutput, error) {
	raw, err := readScratch(path)
	if err != nil {
		return workflow.ImplementerOutput{}, outputProblem(workflow.ImplementerOutputFile, err)
	}

	return workflow.ParseImplementerOutput(string(raw), rework)
}

// outputProblem turns an unreadable output file into the error a rejected one returns.
func outputProblem(file string, err error) error {
	return &workflow.OutputError{File: file, Problems: []string{"cannot be read: " + err.Error()}}
}

// rejectOutput posts every problem with a rejected output file in one comment, then returns the
// failure the run reports. Nothing in the file is applied: the issue goes to failed instead.
func rejectOutput(
	ctx context.Context, c github.Client, repo string, number int, role workflow.Role, err error,
) error {
	var rejected *workflow.OutputError
	if !errors.As(err, &rejected) {
		return err
	}

	lines := []string{
		fmt.Sprintf("The %s's output file was rejected, so nothing was applied.", role), "",
	}
	for _, problem := range rejected.Problems {
		lines = append(lines, "- "+problem)
	}

	text := workflow.Truncate(strings.Join(lines, "\n"), workflow.CommentLimitChars)
	if postErr := c.Comment(ctx, repo, number, text); postErr != nil {
		// Both failures matter: the file was bad and the report never reached the issue.
		return errors.Join(err, postErr)
	}

	return &reportedError{cause: err}
}

// applyImplementer commits the working tree with hooks disabled, pushes the branch, opens or updates
// the pull request, and posts the replies to the review. Every step is keyed to the tree, the branch
// or the commit, so applying the same output twice changes nothing.
func (s session) applyImplementer(
	ctx context.Context, j job, repo, branch, start string, original *github.PullRequest,
	out workflow.ImplementerOutput,
) error {
	if err := s.commit(ctx, out.CommitMessage); err != nil {
		return err
	}

	pr, err := s.publish(ctx, j, repo, branch, start, original, out)
	if err != nil {
		return err
	}

	if err := s.reply(ctx, j, repo, pr, original != nil, out); err != nil {
		return err
	}

	return writeLine(s.d.Out, pr.HTMLURL)
}

// commit stages the working tree and commits it, with the clone's hooks disabled so nothing the
// model wrote runs outside the sandbox. A tree with nothing to commit is left as it is.
func (s session) commit(ctx context.Context, message string) error {
	if _, err := s.git(ctx, nil, "add", "-A"); err != nil {
		return err
	}

	status, err := s.git(ctx, nil, "status", "--porcelain")
	if err != nil {
		return err
	}

	if strings.TrimSpace(status) == "" {
		return nil
	}

	_, err = s.git(ctx, nil, "commit", "-m", message)

	return err
}

// reply posts the answers to the review's findings, keyed to the commit they answer. A first run has
// no review to answer.
func (s session) reply(
	ctx context.Context, j job, repo string, pr *github.PullRequest, rework bool, out workflow.ImplementerOutput,
) error {
	if !rework || len(out.Replies) == 0 {
		return nil
	}

	sha, err := s.head(ctx)
	if err != nil {
		return err
	}

	return j.c.PostReplies(ctx, repo, pr.Number, sha, workflow.RenderReplies(sha, out.Replies))
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

// publish pushes the agent's new commits and opens or updates the pull request. A first run must
// carry a commit; a rerun may change nothing and still refreshes the pull request.
func (s session) publish(
	ctx context.Context, j job, repo, branch, start string, pr *github.PullRequest, out workflow.ImplementerOutput,
) (*github.PullRequest, error) {
	if err := s.requireNewCommits(ctx, start, branch, pr); err != nil {
		return nil, err
	}
	// agent/* branches are ours alone; a first run replaces any stale one, a rework fast-forwards.
	push := []string{gitPush, gitRemote, branch}
	if pr == nil {
		push = []string{gitPush, "--force", gitRemote, branch}
	}

	if _, err := s.git(ctx, s.gitEnv, push...); err != nil {
		return nil, err
	}

	return s.upsertPR(ctx, j, repo, branch, pr, out)
}

// upsertPR opens the pull request linked to the issue, or updates the open one's text when the output
// carries it.
func (s session) upsertPR(
	ctx context.Context, j job, repo, branch string, pr *github.PullRequest, out workflow.ImplementerOutput,
) (*github.PullRequest, error) {
	if pr == nil {
		return openPR(ctx, j.c, repo, j.issue, branch, out)
	}

	if out.PRTitle != "" && out.PRBody != "" {
		if err := j.c.UpdatePR(ctx, repo, pr.Number, out.PRTitle, workflow.PRBody(j.issue.Number, out.PRBody)); err != nil {
			return nil, err
		}
	}

	return pr, nil
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

	if _, err := s.git(ctx, s.gitEnv, "fetch", gitRemote, branch); err != nil {
		return nil, "", err
	}

	if _, err := s.git(ctx, nil, "checkout", "-B", branch, "origin/"+branch); err != nil {
		return nil, "", err
	}

	feedback, err := c.LatestReviewBody(ctx, repo, pr.Number)

	return pr, feedback, err
}

// requireNewCommits never discards the agent's work silently: a first run with no new commits fails,
// because there is no pull request to carry them. A rerun may change nothing.
func (s session) requireNewCommits(ctx context.Context, start, branch string, pr *github.PullRequest) error {
	if pr != nil {
		return nil
	}

	out, err := s.git(ctx, nil, "rev-list", "--count", start+".."+branch)
	if err != nil {
		return err
	}

	if strings.TrimSpace(out) == "0" {
		return errors.New("the agent made no commits")
	}

	return nil
}

// openPR opens the pull request for the branch, linked to the issue, and comments its link.
func openPR(
	ctx context.Context, c github.Client, repo string, issue workflow.Issue, branch string,
	out workflow.ImplementerOutput,
) (*github.PullRequest, error) {
	base, err := c.DefaultBranch(ctx, repo)
	if err != nil {
		return nil, err
	}

	pr, err := c.CreatePR(ctx, repo, out.PRTitle, branch, base, workflow.PRBody(issue.Number, out.PRBody))
	if err != nil {
		return nil, err
	}

	if err := c.Comment(ctx, repo, issue.Number, "Pull request: "+pr.HTMLURL); err != nil {
		return nil, err
	}

	return &pr, nil
}

// runResearcher researches the ticket in a fresh sandbox directory and logs the run's one usage line,
// even when the run failed.
func runResearcher(
	ctx context.Context, d Deps, repo string, agent config.AgentConfig, retro config.Budget, number int,
) (err error) {
	var (
		usage   opencode.Usage
		domains []string
	)

	files, err := newSandboxFiles(d.Settings.WorkRoot, workflow.Researcher, number)
	if err != nil {
		return err
	}

	defer func() { err = errors.Join(err, files.remove()) }()

	usage, domains, err = research(ctx, d, repo, agent, retro, number, files.db(workflow.Researcher), files)
	d.logUsage(opencode.NewUsageReport(workflow.Researcher, repo, number, d.Host, agent, usage).
		WithDomains(domains))

	return err
}

// research clones, runs the model under the sandbox, applies its findings, and writes the retro.
func research(
	ctx context.Context, d Deps, repo string, agent config.AgentConfig, retro config.Budget, number int,
	db string, files sandboxFiles,
) (opencode.Usage, []string, error) {
	network, err := sandbox.ParseNetwork(agent.Egress.AllowedDomains)
	if err != nil {
		return opencode.Usage{}, nil, err
	}

	sec, err := readSecrets(d.Settings, workflow.Researcher)
	if err != nil {
		return opencode.Usage{}, nil, err
	}

	return d.withProxy(ctx, network, sec.openrouterKey, func(baseURL, proxyAddr string) (opencode.Usage, error) {
		return researchRun(ctx, d, sec, baseURL, proxyAddr, network, repo, agent, retro, number, db, files)
	})
}

// researchRun is the Researcher's work, with its proxies already up.
func researchRun(
	ctx context.Context, d Deps, sec secrets, baseURL, proxyAddr string, network sandbox.Network,
	repo string, agent config.AgentConfig, retro config.Budget, number int, db string, files sandboxFiles,
) (opencode.Usage, error) {
	j, err := loadJob(ctx, d, sec, baseURL, repo, workflow.Researcher, agent, number, db)
	if err != nil {
		return opencode.Usage{}, err
	}

	paths, err := researcherPaths(d.Settings.WorkRoot, number)
	if err != nil {
		return opencode.Usage{}, err
	}

	s, docs, thread, err := researchContext(ctx, d, j, sec, repo, number, paths, files, network, proxyAddr)
	if err != nil {
		return opencode.Usage{}, err
	}

	res, err := s.agent(ctx, opencode.Run{
		Role: workflow.Researcher, Prompt: prompt.Researcher(docs, repo, j.issue, thread, paths.out),
		Timeout: agent.Deadline(), Steps: agent.Steps,
	})
	if err != nil {
		return s.researchOutcome(ctx, j, repo, number, res, s.headKey(ctx), err, retro)
	}

	out, err := readResearcherOutput(paths.out)
	if err != nil {
		return res.Usage, rejectOutput(ctx, j.c, repo, number, workflow.Researcher, err)
	}

	branch, err := workflow.ResearchBranch(out.Branch)
	if err != nil {
		return res.Usage, rejectOutput(ctx, j.c, repo, number, workflow.Researcher,
			&workflow.OutputError{File: workflow.ResearcherOutputFile, Problems: []string{err.Error()}})
	}

	sha, err := s.applyResearch(ctx, j, repo, number, branch, out)
	if err != nil {
		return s.researchOutcome(ctx, j, repo, number, res, s.headKey(ctx), err, retro)
	}
	// The output is applied: the retro is the run's epilogue and never changes its outcome.
	return s.researchOutcome(ctx, j, repo, number, res, sha, nil, retro)
}

// researchContext clones the repository, reads its docs and the ticket's guidance, and puts the
// Researcher's session inside its sandbox. The clone and the reads are foreman's, outside the
// sandbox; only the model and its retro run inside.
func researchContext(
	ctx context.Context, d Deps, j job, sec secrets, repo string, number int, paths researcherFiles,
	files sandboxFiles, network sandbox.Network, proxyAddr string,
) (session, []prompt.Doc, []workflow.Item, error) {
	s := newSession(d, j, paths.workdir, []string{
		"-c", "user.name=factory-agent", "-c", "user.email=factory-agent@users.noreply.github.com",
	})

	if err := s.clone(ctx, repo); err != nil {
		return session{}, nil, nil, err
	}

	docs, err := prompt.ReadRepoDocs(paths.workdir)
	if err != nil {
		return session{}, nil, nil, err
	}

	thread, err := guidance(ctx, j.c, repo, number)
	if err != nil {
		return session{}, nil, nil, err
	}

	s, err = d.sandboxedWithToolchain(
		ctx, s, j, sec, repo, "", files, workflow.Researcher, paths.workdir, paths.out,
		[]string{retroPath(d.Settings.WorkRoot, number, workflow.Researcher)}, network, proxyAddr,
	)

	return s, docs, thread, err
}

// researchOutcome folds a research run's retro into its usage and returns the run's failure.
func (s session) researchOutcome(
	ctx context.Context, j job, repo string, number int, res opencode.Result,
	key string, err error, retro config.Budget,
) (opencode.Usage, error) {
	usage := res.Usage.Add(s.retroEpilogue(
		ctx, j, repo, number, workflow.Researcher, res.Session, key, err, retro,
	))

	return usage, err
}

// applyResearch publishes the findings and records the resolution. It reads the ticket's map before
// pushing anything, so a ticket without one fails the run with nothing published.
func (s session) applyResearch(
	ctx context.Context, j job, repo string, number int, branch string, out workflow.ResearcherOutput,
) (string, error) {
	parent, err := parentMap(ctx, j.c, repo, j.issue)
	if err != nil {
		return "", err
	}

	sha, err := s.publishResearch(ctx, number, branch)
	if err != nil {
		return "", err
	}

	return sha, s.resolveResearch(ctx, j, repo, branch, parent, out)
}

// researcherFiles are a research run's clone and output path.
type researcherFiles struct {
	workdir, out string
}

// researcherPaths clears and returns the clone and scratch paths of a research run. The output file
// sits outside the clone, in the run's own scratch directory, with the name the spec fixes.
func researcherPaths(root string, number int) (researcherFiles, error) {
	workdir, err := freshPath(root, "research-"+strconv.Itoa(number))
	if err != nil {
		return researcherFiles{}, err
	}

	scratch, err := scratchDir(root, "researcher-"+strconv.Itoa(number))
	if err != nil {
		return researcherFiles{}, err
	}

	return researcherFiles{workdir: workdir, out: filepath.Join(scratch, workflow.ResearcherOutputFile)}, nil
}

// readResearcherOutput reads and validates the Researcher's output file.
func readResearcherOutput(path string) (workflow.ResearcherOutput, error) {
	raw, err := readScratch(path)
	if err != nil {
		return workflow.ResearcherOutput{}, outputProblem(workflow.ResearcherOutputFile, err)
	}

	return workflow.ParseResearcherOutput(string(raw))
}

// parentMap finds the map a research ticket belongs to: its sub-issue parent when the repository has
// them, otherwise the "Part of #N" line at the top of the ticket. A parent that is not a map fails
// closed, so the decision line is never written to an unrelated issue.
func parentMap(ctx context.Context, c github.Client, repo string, issue workflow.Issue) (workflow.Issue, error) {
	parent, supported, err := c.Parent(ctx, repo, issue.Number)
	if err != nil {
		return workflow.Issue{}, fmt.Errorf("read the parent of #%d: %w", issue.Number, err)
	}

	if !supported {
		number, ok := workflow.PartOf(issue.Body)
		if !ok {
			return workflow.Issue{}, fmt.Errorf("#%d has no parent map", issue.Number)
		}

		parent, err = c.GetIssue(ctx, repo, number)
		if err != nil {
			return workflow.Issue{}, fmt.Errorf("read the parent map #%d: %w", number, err)
		}
	}

	if !workflow.HasLabel(parent, workflow.LabelMap) {
		return workflow.Issue{}, fmt.Errorf("the parent of #%d is not a %s issue", issue.Number, workflow.LabelMap)
	}

	return parent, nil
}

// publishResearch commits the findings and pushes them to research/<name>. It never opens a pull
// request: the branch is a record, never merged. A branch that already exists remote is reused and
// fast-forwarded, so a re-run keeps the branch and its earlier findings.
func (s session) publishResearch(ctx context.Context, number int, branch string) (string, error) {
	start, err := s.head(ctx)
	if err != nil {
		return "", err
	}

	reused, err := s.remoteBranch(ctx, branch)
	if err != nil {
		return "", err
	}

	// The model's findings are uncommitted in the working tree, so the branch is made from where
	// they sit; a reused branch is put under the commit once it is made.
	if _, err := s.git(ctx, nil, "checkout", "-B", branch); err != nil {
		return "", err
	}

	message := fmt.Sprintf("Add research findings for #%d", number)

	if err := s.commit(ctx, message); err != nil {
		return "", err
	}

	if err := s.requireFindings(ctx, start, branch, reused); err != nil {
		return "", err
	}

	if reused {
		if err := s.rebaseOntoBranch(ctx, branch, message); err != nil {
			return "", err
		}
	}

	push := []string{gitPush, gitRemote, branch}
	if !reused {
		push = []string{gitPush, "--force", gitRemote, branch}
	}

	if _, err := s.git(ctx, s.gitEnv, push...); err != nil {
		return "", err
	}

	return s.head(ctx)
}

// remoteBranch reports whether the findings branch is already pushed.
func (s session) remoteBranch(ctx context.Context, branch string) (bool, error) {
	out, err := s.git(ctx, s.gitEnv, "ls-remote", "--heads", gitRemote, branch)
	if err != nil {
		return false, err
	}

	return strings.TrimSpace(out) != "", nil
}

// requireFindings never discards the agent's work silently: a new branch with no commit fails,
// because there is no branch to carry the findings. A reused branch may change nothing.
func (s session) requireFindings(ctx context.Context, start, branch string, reused bool) error {
	if reused {
		return nil
	}

	out, err := s.git(ctx, nil, "rev-list", "--count", start+".."+branch)
	if err != nil {
		return err
	}

	if strings.TrimSpace(out) == "0" {
		return errors.New("the agent made no commits")
	}

	return nil
}

// rebaseOntoBranch puts the findings commit on top of the branch it reuses, without rewriting that
// branch's history. The commit is reset onto the branch's tip and made again, so the push is a
// fast-forward. The findings tree is the whole commit, so a re-run replaces the ticket's findings.
func (s session) rebaseOntoBranch(ctx context.Context, branch, message string) error {
	if _, err := s.git(ctx, s.gitEnv, "fetch", gitRemote, branch); err != nil {
		return err
	}

	if _, err := s.git(ctx, nil, "reset", "--soft", "FETCH_HEAD"); err != nil {
		return err
	}

	status, err := s.git(ctx, nil, "status", "--porcelain")
	if err != nil {
		return err
	}

	if strings.TrimSpace(status) == "" {
		return nil
	}

	_, err = s.git(ctx, nil, "commit", "-m", message)

	return err
}

// resolveResearch posts the resolution, keyed to the branch, and appends the decision line to the
// ticket's map. Both are keyed, so applying the same output twice changes nothing.
func (s session) resolveResearch(
	ctx context.Context, j job, repo, branch string, parent workflow.Issue, out workflow.ResearcherOutput,
) error {
	if err := j.c.PostResolution(ctx, repo, j.issue.Number, branch,
		workflow.RenderResolution(repo, branch, out.Resolution)); err != nil {
		return err
	}

	entry := workflow.DecisionEntry(j.issue.Title, workflow.IssueURL(repo, j.issue.Number), out.DecisionLine)

	body, err := workflow.AppendDecision(parent.Body, entry)
	if err != nil {
		return fmt.Errorf("append the decision to #%d: %w", parent.Number, err)
	}

	if body == parent.Body {
		return nil
	}

	return j.c.UpdateIssueBody(ctx, repo, parent.Number, body)
}

// runReviewer reviews in memory-only state and logs the run's one usage line, even when the run failed.
func runReviewer(
	ctx context.Context, d Deps, repo string, agent config.AgentConfig, retro config.Budget, number int,
) (int, error) {
	var (
		usage   opencode.Usage
		domains []string
		code    int
		err     error
	)

	files, err := newSandboxFiles(d.Settings.WorkRoot, workflow.Reviewer, number)
	if err != nil {
		return 1, err
	}

	defer func() { err = errors.Join(err, files.remove()) }()

	code, usage, domains, err = review(ctx, d, repo, agent, retro, number, files)
	d.logUsage(opencode.NewUsageReport(workflow.Reviewer, repo, number, d.Host, agent, usage).
		WithDomains(domains))

	if err != nil && code == 0 {
		code = 1
	}

	return code, err
}

// review clones the pull request's branch, runs the review under the sandbox, posts it, and returns
// the verdict code.
func review(
	ctx context.Context, d Deps, repo string, agent config.AgentConfig, retro config.Budget, number int,
	files sandboxFiles,
) (int, opencode.Usage, []string, error) {
	network, err := sandbox.ParseNetwork(agent.Egress.AllowedDomains)
	if err != nil {
		return 1, opencode.Usage{}, nil, err
	}

	sec, err := readSecrets(d.Settings, workflow.Reviewer)
	if err != nil {
		return 1, opencode.Usage{}, nil, err
	}

	var code int

	usage, domains, err := d.withProxy(
		ctx, network, sec.openrouterKey, func(baseURL, proxyAddr string) (opencode.Usage, error) {
			runCode, u, e := reviewRun(ctx, d, sec, baseURL, proxyAddr, network, repo, agent, retro, number, files)
			code = runCode

			return u, e
		})

	return code, usage, domains, err
}

// reviewRun is the Reviewer's work, with its proxies already up.
func reviewRun(
	ctx context.Context, d Deps, sec secrets, baseURL, proxyAddr string, network sandbox.Network,
	repo string, agent config.AgentConfig, retro config.Budget, number int, files sandboxFiles,
) (int, opencode.Usage, error) {
	j, err := loadJob(ctx, d, sec, baseURL, repo, workflow.Reviewer, agent, number,
		files.db(workflow.Reviewer))
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

	if s, err = d.sandboxedWithToolchain(
		ctx, s, j, sec, repo, base, files, workflow.Reviewer, paths.workdir, paths.out,
		[]string{paths.spec, retroPath(d.Settings.WorkRoot, number, workflow.Reviewer)}, network, proxyAddr,
	); err != nil {
		return 1, opencode.Usage{}, err
	}

	docs, err := writeSpec(paths, j.issue)
	if err != nil {
		return 1, opencode.Usage{}, err
	}

	res, err := s.agent(ctx, opencode.Run{
		Role: workflow.Reviewer, Prompt: prompt.Reviewer(docs, base, paths.spec, paths.out),
		Timeout: agent.Deadline(), Steps: agent.Steps,
	})
	// The retro is keyed by the reviewed PR head SHA, on every path that reached the model.
	if err != nil {
		return 1, res.Usage.Add(s.retroEpilogue(
			ctx, j, repo, number, workflow.Reviewer, res.Session, sha, err, retro,
		)), err
	}

	out, err := readReviewerOutput(paths.out)
	if err != nil {
		return 1, res.Usage, rejectOutput(ctx, j.c, repo, number, workflow.Reviewer, err)
	}

	code, err := postVerdict(ctx, j.c, repo, pr.Number, sha, out, j.issue)

	return code, res.Usage.Add(s.retroEpilogue(
		ctx, j, repo, number, workflow.Reviewer, res.Session, sha, err, retro,
	)), err
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

// reviewFiles are a review run's paths: the clone, the spec and the model's output file.
type reviewFiles struct {
	workdir, spec, out string
}

// reviewPaths clears and returns the paths of a review run. The output file and the spec sit in the
// run's scratch directory, outside the clone, with the names the spec fixes.
func reviewPaths(root string, number int) (reviewFiles, error) {
	workdir, err := freshPath(root, "review-"+strconv.Itoa(number))
	if err != nil {
		return reviewFiles{}, err
	}

	scratch, err := scratchDir(root, "reviewer-"+strconv.Itoa(number))
	if err != nil {
		return reviewFiles{}, err
	}

	return reviewFiles{
		workdir: workdir,
		spec:    filepath.Join(scratch, "issue.md"),
		out:     filepath.Join(scratch, workflow.ReviewerOutputFile),
	}, nil
}

// checkoutForReview clones the repository on the pull request's branch and returns the reviewed commit.
func (s session) checkoutForReview(ctx context.Context, repo, branch string) (string, error) {
	if err := s.clone(ctx, repo); err != nil {
		return "", err
	}

	if _, err := s.git(ctx, s.gitEnv, "fetch", gitRemote, branch); err != nil {
		return "", err
	}

	if _, err := s.git(ctx, nil, "checkout", "-B", branch, "origin/"+branch); err != nil {
		return "", err
	}

	return s.head(ctx)
}

// readReviewerOutput reads and validates the Reviewer's output file.
func readReviewerOutput(path string) (workflow.ReviewerOutput, error) {
	raw, err := readScratch(path)
	if err != nil {
		return workflow.ReviewerOutput{}, outputProblem(workflow.ReviewerOutputFile, err)
	}

	return workflow.ParseReviewerOutput(string(raw))
}

// postVerdict posts the verdict once per reviewed commit and returns the code `start` routes by. The
// verdict leaves only through the output file, so nothing else can decide it.
func postVerdict(
	ctx context.Context, c github.Client, repo string, pr int, sha string, out workflow.ReviewerOutput,
	issue workflow.Issue,
) (int, error) {
	reviews, err := c.Reviews(ctx, repo, pr)
	if err != nil {
		return 1, err
	}

	// A repeat of this commit's review posts nothing: the same head SHA is reviewed once.
	if len(workflow.StartsWith(reviews, workflow.ReviewMarker(sha))) == 0 {
		body := workflow.RenderReview(sha, out)
		if err := c.PostReview(ctx, repo, pr, sha, body, out.Verdict); err != nil {
			return 1, err
		}
	}

	if out.Verdict == workflow.Approve {
		// An approval asks the issue's writer, a human, to review and merge. A bot cannot.
		return workflow.ReviewClean, c.RequestReviewers(ctx, repo, pr, humans(issue.Author))
	}

	if workflow.ReviewRounds(reviews, sha) >= workflow.MaxReviewRounds {
		return workflow.ReviewExhausted, nil
	}

	return workflow.ReviewFindings, nil
}

// humans returns the logins a review may be requested from: a bot is not one.
func humans(author string) []string {
	if author == "" || workflow.BotAuthored(author) {
		return nil
	}

	return []string{author}
}
