package foreman

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
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

// Run is the factory half, run as the factory user: it works the issue as the agent role, settles the
// run's outcome on the issue, then runs the retro as its epilogue. For the Reviewer the code is the
// verdict (workflow.ReviewClean, ReviewFindings, ReviewExhausted); a run that already reported its
// failure on the issue returns workflow.ExitReported. `start` applies the same settled transition
// afterwards, from this code, as the safety net for a factory half that never returned.
func Run(
	ctx context.Context, d Deps, role workflow.Role, ref workflow.IssueRef,
	agent config.AgentConfig, retro config.Budget, limit int,
) (int, error) {
	// A host the sandbox adapter cannot serve does no work: there is never an unsandboxed fallback.
	if err := sandbox.Supported(runtime.GOOS); err != nil {
		return 1, err
	}

	spec := runSpec{ref: ref, agent: agent, retro: retro, limit: limit}

	ar, ok := agentRoleFor(role)
	if !ok {
		return 1, fmt.Errorf("unknown agent role %q", role)
	}

	code, err := runRole(ctx, d, spec, role, ar.run, func(ctx context.Context, code int, runErr error) {
		settleRun(ctx, d, role, ref, spec.limit, code, runErr)
	})

	return exitCode(err, code)
}

// runSpec is one run's work: what it works, the agent role's settings, and the budgets it runs under.
// It travels through the run's helpers as one value, so adding a setting does not widen every
// signature that carries it.
type runSpec struct {
	// ref is the issue the run works.
	ref workflow.IssueRef
	// agent holds the agent role's model, egress and run budget.
	agent config.AgentConfig
	// retro is the budget the run's retro gets.
	retro config.Budget
	// limit is the review round limit, which only the Reviewer reads.
	limit int
}

// withSandboxRun gives one run its private sandbox directories, runs work, and logs the run's one
// usage line even when the work failed. The sandbox directories are removed afterwards; the retro
// file lives in the work root and stays.
func withSandboxRun(
	d Deps, spec runSpec, role workflow.Role,
	work func(files sandboxFiles) (runResult, []string, error),
) (code int, err error) {
	files, err := newSandboxFiles(d.Settings.WorkRoot, role, spec.ref.Number)
	if err != nil {
		return 1, err
	}

	defer func() { err = errors.Join(err, files.remove()) }()

	result, domains, err := work(files)
	d.logUsage(opencode.NewUsageReport(role, spec.ref, d.Host, spec.agent, result.usage).
		WithDomains(domains))

	return result.code, err
}

// withRunSecrets reads the agent role's token and the OpenRouter key, starts the run's proxies and
// calls work inside them. A parse, read or bind failure fails the run: there is never an unsandboxed
// or unproxied fallback.
func withRunSecrets(
	ctx context.Context, d Deps, spec runSpec, role workflow.Role,
	work func(sec secrets, baseURL, token, proxyAddr string, network sandbox.Network) (runResult, error),
) (runResult, []string, error) {
	network, err := sandbox.ParseNetwork(spec.agent.Egress.AllowedDomains)
	if err != nil {
		return runResult{}, nil, err
	}

	sec, err := readSecrets(d.Settings, role)
	if err != nil {
		return runResult{}, nil, err
	}

	return d.withProxy(ctx, network, sec.openrouterKey, func(baseURL, token, proxyAddr string) (runResult, error) {
		return work(sec, baseURL, token, proxyAddr, network)
	})
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
	// token is the run's proxy token, kept only to scrub it from anything echoed.
	token string
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
func (s session) clone(ctx context.Context, repo workflow.Repo) error {
	_, err := s.d.run(ctx, proc.Cmd{
		Argv: []string{gitProgram, gitClone, "https://github.com/" + repo.String() + ".git", s.workdir}, Env: s.gitEnv,
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
		token:    j.token,
	}
}

// sandboxed puts the model process inside the run's sandbox: its own HOME and XDG directories, the
// default branch's toolchain, and the srt settings compiled from the agent role's spec.
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
	token string
	c     github.Client
	issue workflow.Issue
}

// loadJob reads the issue an agent role works and builds its model environment around the run's
// proxy. The secrets are already read: the key belongs to the proxy, never to the model. The model's
// one GitHub credential is a read-only token of its own App's, scoped to the repository.
func loadJob(
	ctx context.Context, d Deps, sec secrets, baseURL, token string, repo workflow.Repo, role workflow.Role,
	agent config.AgentConfig, number int, db string,
) (job, error) {
	env, err := opencode.Env(baseURL, token, db, agent)
	if err != nil {
		return job{}, err
	}

	readOnly, err := d.mintReadOnlyToken(ctx, role, repo)
	if err != nil {
		return job{}, err
	}

	env["GH_TOKEN"] = readOnly

	c := d.client(sec.githubToken)

	issue, err := c.GetIssue(ctx, workflow.IssueRef{Repo: repo, Number: number})
	if err != nil {
		return job{}, err
	}

	return job{sec: sec, env: env, token: token, c: c, issue: issue}, nil
}

// withProxy runs fn with the run's OpenRouter key proxy and its egress proxy up, and stops both when
// fn returns. The usage the key proxy read and the domains the egress proxy saw are the run's. A
// proxy that cannot start fails the run: the model has no other way to reach OpenRouter, and there is
// never an unsandboxed fallback.
func (d Deps) withProxy(
	ctx context.Context, network sandbox.Network, key string,
	fn func(baseURL, token, proxyAddr string) (runResult, error),
) (runResult, []string, error) {
	token, err := d.token()
	if err != nil {
		return runResult{}, nil, err
	}

	p, err := d.proxy(key, token)
	if err != nil {
		return runResult{}, nil, err
	}

	e, err := d.egress(ctx, network, p.URL(), token)
	if err != nil {
		return runResult{}, nil, errors.Join(err, p.Close())
	}

	result, err := fn(e.URL(), token, e.Address())
	// Read the usage before stopping: a response still being relayed when the model exits cannot be
	// counted, and Close is what ends it.
	read := p.Usage()
	domains := e.Domains()
	// A failure to stop either proxy is part of the run's outcome, not a diagnostic to drop.
	err = errors.Join(err, e.Close(), p.Close())

	result.usage = result.usage.Use(reported(read))

	return result, domains, err
}

// egress starts the run's egress proxy with the agent role's policy, forwarding its API calls to
// apiURL.
func (d Deps) egress(ctx context.Context, policy sandbox.Network, apiURL, token string) (EgressProxy, error) {
	start := d.Egress
	if start == nil {
		start = func(ctx context.Context, policy sandbox.Network, apiURL, token string) (EgressProxy, error) {
			return egress.StartWithLogger(ctx, policy, apiURL, token, d.egressAddr(), d.log())
		}
	}

	return start(ctx, policy, apiURL, token)
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
func (d Deps) proxy(key, token string) (Forwarder, error) {
	start := d.Proxy
	if start == nil {
		start = func(key, token, addr string) (Forwarder, error) {
			return openrouter.Start(key, token, addr)
		}
	}

	addr := d.ProxyAddr
	if addr == "" {
		addr = openrouter.Loopback
	}

	return start(key, token, addr)
}

// tokenBytes is the entropy of the per-run proxy token.
const tokenBytes = 32

// token mints the run's random proxy token, or reads the injected one. An empty token is an error:
// both proxies would refuse every request.
func (d Deps) token() (string, error) {
	if d.Token != nil {
		token, err := d.Token()
		if err != nil {
			return "", fmt.Errorf("mint proxy token: %w", err)
		}

		if token == "" {
			return "", fmt.Errorf("mint proxy token: empty")
		}

		return token, nil
	}

	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("mint proxy token: %w", err)
	}

	return hex.EncodeToString(raw), nil
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
	if err == nil {
		return code, nil
	}

	return startCode(err, code), err
}

// startCode is the code `start` reads for a run: a reported failure becomes ExitReported, because the
// factory half already commented; any other failure is 1; a success is its own verdict.
func startCode(err error, code int) int {
	if _, ok := errors.AsType[*reportedError](err); ok {
		return workflow.ExitReported
	}

	if err != nil {
		return 1
	}

	return code
}

// buildImplementerPrompt builds the Implementer's prompt from the clone's docs, the review feedback
// and the comments that woke the issue.
func buildImplementerPrompt(
	ctx context.Context, c github.Client, repo workflow.Repo, issue workflow.Issue, workdir, feedback, out string,
) (string, error) {
	docs, err := prompt.ReadRepoDocs(workdir)
	if err != nil {
		return "", err
	}

	thread, ci, err := rework(ctx, c, workflow.IssueRef{Repo: repo, Number: issue.Number})
	if err != nil {
		return "", err
	}

	return prompt.Implementer(docs, repo.String(), issue, feedback, thread, ci, out), nil
}

// rework reads what a run that was sent back must answer: every comment since the marker that woke
// the issue, and the CI failure report when that is why it came back.
func rework(ctx context.Context, c github.Client, ref workflow.IssueRef) ([]workflow.Item, string, error) {
	thread, err := guidance(ctx, c, ref)
	if err != nil {
		return nil, "", err
	}

	ci, err := ciFeedback(ctx, c, ref)

	return thread, ci, err
}

// guidance reads every comment since the marker that woke the issue, from the issue and its pull
// request, oldest first. An issue that was not woken reads nothing.
func guidance(ctx context.Context, c github.Client, ref workflow.IssueRef) ([]workflow.Item, error) {
	comments, err := c.ListComments(ctx, ref)
	if err != nil {
		return nil, err
	}

	pr, err := c.ExistingPR(ctx, ref.Repo, workflow.BranchName(ref.Number))
	if err != nil {
		return nil, err
	}

	if pr != nil {
		onPR, err := c.ListComments(ctx, workflow.IssueRef{Repo: ref.Repo, Number: pr.Number})
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

// ciFeedback is the CI failure report that sent this run back, when the report comes after the
// handoff that sent the pull request to review. A report from an earlier round is stale and left out.
func ciFeedback(ctx context.Context, c github.Client, ref workflow.IssueRef) (string, error) {
	comments, err := c.ListComments(ctx, ref)
	if err != nil {
		return "", err
	}

	report, ok, err := workflow.LatestCIFailure(comments)
	if err != nil || !ok {
		return "", err
	}

	return workflow.CIFailureText(report.Body), nil
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

// outputProblem turns an unreadable output file into the error a rejected one returns.
func outputProblem(file string, err error) error {
	return &workflow.OutputError{File: file, Problems: []string{"cannot be read: " + err.Error()}}
}

// rejectOutput posts every problem with a rejected output file in one comment, then returns the
// failure the run reports. Nothing in the file is applied: the issue goes to failed instead.
func rejectOutput(
	ctx context.Context, c github.Client, repo workflow.Repo, number int, role workflow.Role, err error,
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
	if postErr := c.Comment(ctx, workflow.IssueRef{Repo: repo, Number: number}, text); postErr != nil {
		// Both failures matter: the file was bad and the report never reached the issue.
		return errors.Join(err, postErr)
	}

	return &reportedError{cause: err}
}

// applyImplementer commits the working tree with hooks disabled, pushes the branch, opens or updates
// the pull request, and posts the replies to the review. Every step is keyed to the tree, the branch
// or the commit, so applying the same output twice changes nothing.
func (s session) applyImplementer(
	ctx context.Context, j job, repo workflow.Repo, branch, start string, original *github.PullRequest,
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
	ctx context.Context, j job, repo workflow.Repo, pr *github.PullRequest, rework bool, out workflow.ImplementerOutput,
) error {
	if !rework || len(out.Replies) == 0 {
		return nil
	}

	sha, err := s.head(ctx)
	if err != nil {
		return err
	}

	ref := workflow.IssueRef{Repo: repo, Number: pr.Number}

	return j.c.UpsertComment(ctx, ref, workflow.RepliesMarker(sha), workflow.RenderReplies(sha, out.Replies))
}

// prepare clones the repository and checks out the issue's branch. It returns the open pull request, if
// any, the review feedback on it, and the commit the agent starts from.
func (s session) prepare(
	ctx context.Context, c github.Client, repo workflow.Repo, branch string,
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
	ctx context.Context, j job, repo workflow.Repo, branch, start string, pr *github.PullRequest,
	out workflow.ImplementerOutput,
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
	ctx context.Context, j job, repo workflow.Repo, branch string, pr *github.PullRequest, out workflow.ImplementerOutput,
) (*github.PullRequest, error) {
	if pr == nil {
		return openPR(ctx, j.c, repo, j.issue, branch, out)
	}

	if out.PRTitle != "" && out.PRBody != "" {
		ref := workflow.IssueRef{Repo: repo, Number: pr.Number}

		if err := j.c.UpdatePR(ctx, ref, out.PRTitle, workflow.PRBody(j.issue.Number, out.PRBody)); err != nil {
			return nil, err
		}
	}

	return pr, nil
}

// checkoutBranch checks out the issue's branch. With an open pull request it builds on the reviewed
// commits instead of discarding them, and returns the latest review as feedback.
func (s session) checkoutBranch(
	ctx context.Context, c github.Client, repo workflow.Repo, branch string,
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

	feedback, err := c.LatestReviewBody(ctx, workflow.IssueRef{Repo: repo, Number: pr.Number})

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
	ctx context.Context, c github.Client, repo workflow.Repo, issue workflow.Issue, branch string,
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

	ref := workflow.IssueRef{Repo: repo, Number: issue.Number}

	if err := c.Comment(ctx, ref, "Pull request: "+pr.HTMLURL); err != nil {
		return nil, err
	}

	return &pr, nil
}

// applyResearch publishes the findings and records the resolution. It reads the ticket's map before
// pushing anything, so a ticket without one fails the run with nothing published.
func (s session) applyResearch(
	ctx context.Context, j job, spec runSpec, branch string, out workflow.ResearcherOutput,
) (string, error) {
	parent, err := parentMap(ctx, j.c, spec.ref.Repo, j.issue)
	if err != nil {
		return "", err
	}

	sha, err := s.publishResearch(ctx, spec.ref.Number, branch)
	if err != nil {
		return "", err
	}

	return sha, s.resolveResearch(ctx, j, spec.ref.Repo, branch, parent, out)
}

// parentMap finds the map a research ticket belongs to: its sub-issue parent when the repository has
// them, otherwise the "Part of #N" line at the top of the ticket. A parent that is not a map fails
// closed, so the decision line is never written to an unrelated issue.
func parentMap(ctx context.Context, c github.Client, repo workflow.Repo, issue workflow.Issue) (workflow.Issue, error) {
	parent, supported, err := c.Parent(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number})
	if err != nil {
		return workflow.Issue{}, fmt.Errorf("read the parent of #%d: %w", issue.Number, err)
	}

	if !supported {
		number, ok := workflow.PartOf(issue.Body)
		if !ok {
			return workflow.Issue{}, fmt.Errorf("#%d has no parent map", issue.Number)
		}

		parent, err = c.GetIssue(ctx, workflow.IssueRef{Repo: repo, Number: number})
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
	ctx context.Context, j job, repo workflow.Repo, branch string, parent workflow.Issue, out workflow.ResearcherOutput,
) error {
	ref := workflow.IssueRef{Repo: repo, Number: j.issue.Number}

	if err := j.c.UpsertComment(ctx, ref, workflow.ResearchMarker(branch),
		workflow.RenderResolution(ref.Repo, branch, out.Resolution)); err != nil {
		return err
	}

	entry := workflow.DecisionEntry(j.issue.Title, workflow.IssueURL(ref), out.DecisionLine)

	body, err := workflow.AppendDecision(parent.Body, entry)
	if err != nil {
		return fmt.Errorf("append the decision to #%d: %w", parent.Number, err)
	}

	if body == parent.Body {
		return nil
	}

	return j.c.UpdateIssueBody(ctx, workflow.IssueRef{Repo: repo, Number: parent.Number}, body)
}

// reviewTarget is the open pull request for branch and the branch it merges into.
func reviewTarget(
	ctx context.Context, c github.Client, repo workflow.Repo, branch string,
) (*github.PullRequest, string, error) {
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

// writeSpec writes the issue, followed by the spec a single proposal left in its final-round
// comment, where the Reviewer reads it outside the clone. It returns the clone's repository docs.
func writeSpec(specPath, workdir string, issue workflow.Issue, finalSpec string) ([]prompt.Doc, error) {
	spec := fmt.Sprintf("# %s\n\n%s\n", issue.Title, workflow.Truncate(issue.Body, workflow.BodyLimitChars))
	if strings.TrimSpace(finalSpec) != "" {
		spec += "\n" + workflow.Truncate(finalSpec, workflow.BodyLimitChars) + "\n"
	}

	if err := os.WriteFile(specPath, []byte(spec), privateFileMode); err != nil {
		return nil, fmt.Errorf("write spec: %w", err)
	}

	return prompt.ReadRepoDocs(workdir)
}

// checkoutForReview clones the repository on the pull request's branch and returns the reviewed commit.
func (s session) checkoutForReview(ctx context.Context, repo workflow.Repo, branch string) (string, error) {
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

// postVerdict posts the verdict once per reviewed commit and returns the code `start` routes by. The
// verdict leaves only through the output file, so nothing else can decide it.
func postVerdict(
	ctx context.Context, d Deps, c github.Client, repo workflow.Repo, pr int, sha string,
	out workflow.ReviewerOutput, issue workflow.Issue, limit int,
) (int, error) {
	reviews, err := c.Reviews(ctx, workflow.IssueRef{Repo: repo, Number: pr})
	if err != nil {
		return 1, err
	}

	comments, err := c.ListComments(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number})
	if err != nil {
		return 1, err
	}

	// A repeat of this commit's review posts nothing: the same head SHA is reviewed once.
	if len(workflow.StartsWith(reviews, workflow.ReviewMarker(sha))) == 0 {
		body := workflow.RenderReview(sha, out)
		if err := c.PostReview(ctx, workflow.IssueRef{Repo: repo, Number: pr}, sha, body, out.Verdict); err != nil {
			return 1, err
		}
	}

	if out.Verdict == workflow.Approve {
		// An approval asks a human writer to review and merge. A bot-authored issue, such as a
		// promoted retro group, names no human itself, so the latest human writer who commented is
		// asked instead.
		reviewer, err := approvalReviewer(ctx, d, c, repo, issue, comments)
		if err != nil {
			return 1, err
		}

		if reviewer == "" {
			d.log().Warn("no human writer to ask for an approval review", "repo", repo, "issue", issue.Number,
				"pr", pr, "author", issue.Author)

			return workflow.ReviewClean, nil
		}

		return workflow.ReviewClean, c.RequestReviewers(ctx, workflow.IssueRef{Repo: repo, Number: pr}, []string{reviewer})
	}

	if workflow.RoundLimitReached(workflow.ReviewRounds(reviews, comments, sha), limit) {
		return workflow.ReviewExhausted, nil
	}

	return workflow.ReviewFindings, nil
}

// approvalReviewer is the human writer an approval asks to merge: the issue's author when a writer
// wrote it, otherwise the latest human writer who commented. Empty means no human writer was found.
func approvalReviewer(
	ctx context.Context, d Deps, c github.Client, repo workflow.Repo, issue workflow.Issue,
	comments []workflow.Item,
) (string, error) {
	if issue.Author != "" && !workflow.BotAuthored(issue.Author) {
		permission, err := c.CollaboratorPermission(ctx, repo, issue.Author)
		if err != nil {
			// Fail closed: without a readable permission, the author's write access is unknown.
			return "", fmt.Errorf("read %s's permission: %w", issue.Author, err)
		}

		if workflow.WriterPermission(permission) {
			return issue.Author, nil
		}
	}

	for _, comment := range slices.Backward(comments) {
		login := comment.Author
		if login == "" || workflow.BotAuthored(login) {
			continue
		}

		permission, err := c.CollaboratorPermission(ctx, repo, login)
		if err != nil {
			// Fail closed: without a readable permission, no human writer can be established.
			return "", fmt.Errorf("read %s's permission: %w", login, err)
		}

		if workflow.WriterPermission(permission) {
			d.log().Info("asking the latest human writer to review an approval", "repo", repo,
				"issue", issue.Number, "reviewer", login)

			return login, nil
		}
	}

	return "", nil
}
