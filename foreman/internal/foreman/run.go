package foreman

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/nateinaction/factory/foreman/internal/boundedfile"
	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/egress"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/openrouter"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/prompt"
	"github.com/nateinaction/factory/foreman/internal/sandbox"
	"github.com/nateinaction/factory/foreman/internal/security"
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

	// The agent role's entry is resolved here, once: every step of the run reads it from the run.
	entry, ok := dispatchEntryFor(role)
	if !ok {
		return 1, fmt.Errorf("unknown agent role %q", role)
	}

	r := run{d: d, role: role, entry: entry, spec: runSpec{ref: ref, agent: agent, retro: retro, limit: limit}}

	result, err := r.pipeline(ctx)

	return startCode(err, result.code), err
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

// secrets are the two values the factory half reads at the edge.
type secrets struct {
	githubToken   string
	openrouterKey string
}

// readSecrets reads the token left by `start` for this run and the OpenRouter key.
func readSecrets(tokenPath, keyFile string) (secrets, error) {
	token, err := os.ReadFile(tokenPath) //nolint:gosec // the path is the token directory joined with a vetted name
	if err != nil {
		return secrets{}, fmt.Errorf("read token file: %w", err)
	}

	key, err := os.ReadFile(keyFile) //nolint:gosec // the path is foreman's configured key file
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

// maxScratchBytes bounds what foreman reads from a file the model wrote. Every output foreman parses
// is far smaller: the largest thing it renders is one comment, capped well below this. A file over
// the limit is refused rather than read whole, so a model cannot exhaust the host's memory with it.
const maxScratchBytes = 8 << 20

// readScratch reads a file the model wrote to a scratch path, through maxScratchBytes. The model
// controls what is there, so the read goes through os.Root, opens only a regular file and stops at
// the limit: a symlink out of the scratch directory is refused, not followed, and an output file is
// always a file the model wrote there, never a link or a device.
func readScratch(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("open scratch: %w", err)
	}

	raw, err := readBounded(root, filepath.Base(path), maxScratchBytes)

	return raw, errors.Join(err, wrapErr("close scratch", root.Close()))
}

// readBounded reads a regular file under root whole, refusing one over limit bytes instead of
// returning its first limit bytes: a caller that parses an output asks for all of it or none.
func readBounded(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := boundedfile.Open(root, name)
	if err != nil {
		return nil, err
	}

	// One byte over the limit tells a file at the limit from one over it.
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))

	if err := errors.Join(err, f.Close()); err != nil {
		return nil, wrapErr("read scratch", err)
	}

	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("the file is over the %d-byte read limit", limit)
	}

	return raw, nil
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
	// secrets are every secret the run holds (the OpenRouter key, the proxy token and the model's
	// read-only GitHub token), kept only to scrub them from anything the model process writes.
	secrets []string
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
		secrets:  []string{j.sec.openrouterKey, j.token, j.env["GH_TOKEN"]},
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

// loadJob reads the issue the run works and builds its model environment around the run's proxies.
// The secrets are already read at the edge: the key belongs to the proxy, never to the model. The
// model's one GitHub credential is a read-only token of its own App's, scoped to the repository.
// external names the paths outside the clone the model may reach, and db is its state database.
func (r run) loadJob(ctx context.Context, env runEnv, external []string, db string) (job, error) {
	model, err := opencode.Env(env.baseURL, env.token, db, external, r.spec.agent)
	if err != nil {
		return job{}, err
	}

	readOnly, err := r.d.mint(ctx, r.role, r.spec.ref.Repo, readOnlyToken)
	if err != nil {
		return job{}, err
	}

	model["GH_TOKEN"] = readOnly

	c := r.d.client(env.sec.githubToken)

	issue, err := c.GetIssue(ctx, r.spec.ref)
	if err != nil {
		return job{}, err
	}

	return job{sec: env.sec, env: model, token: env.token, c: c, issue: issue}, nil
}

// egress starts the run's egress proxy with the agent role's policy, forwarding its API calls to
// apiURL. A proxy that cannot start fails the run: the model has no other egress, and there is never
// an unsandboxed fallback.
func (d Deps) egress(
	ctx context.Context, role workflow.Role, ref workflow.IssueRef, policy sandbox.Network, apiURL, token string,
) (EgressProxy, error) {
	start := d.Egress
	if start == nil {
		start = func(ctx context.Context, policy sandbox.Network, apiURL, token string) (EgressProxy, error) {
			// The proxy's security events carry the run they belong to.
			log := security.Scope(d.log(), string(role), ref.Repo.String(), ref.Number)

			return egress.StartWithLogger(ctx, policy, apiURL, token, egress.Loopback, log)
		}
	}

	return start(ctx, policy, apiURL, token)
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

// proxy starts the run's OpenRouter proxy with key on the loopback address. A proxy that cannot start
// fails the run: the model has no other way to reach OpenRouter.
func (d Deps) proxy(key, token string) (Forwarder, error) {
	start := d.Proxy
	if start == nil {
		start = func(key, token, addr string) (Forwarder, error) {
			return openrouter.Start(key, token, addr)
		}
	}

	return start(key, token, openrouter.Loopback)
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
// the issue, and the CI failure report when that is why it came back. Both come from one listing of
// the issue's comments.
func rework(ctx context.Context, c github.Client, ref workflow.IssueRef) ([]workflow.Item, string, error) {
	comments, err := c.ListComments(ctx, ref)
	if err != nil {
		return nil, "", err
	}

	thread, err := issueThread(ctx, c, ref, comments)
	if err != nil {
		return nil, "", err
	}

	ci, err := ciFeedback(comments, ref)

	return thread, ci, err
}

// guidance reads every comment since the marker that woke the issue, from the issue and its pull
// request, oldest first. An issue that was not woken reads nothing.
func guidance(ctx context.Context, c github.Client, ref workflow.IssueRef) ([]workflow.Item, error) {
	comments, err := c.ListComments(ctx, ref)
	if err != nil {
		return nil, err
	}

	return issueThread(ctx, c, ref, comments)
}

// issueThread is the guidance the issue's comments carry: the open pull request's comments join them,
// everything is put in id order, and the markers that woke the issue are read.
func issueThread(
	ctx context.Context, c github.Client, ref workflow.IssueRef, comments []workflow.Item,
) ([]workflow.Item, error) {
	pr, err := c.ExistingPR(ctx, ref.Repo, workflow.BranchName(ref.Number))
	if err != nil {
		return nil, err
	}

	// The caller's listing is left as it is: the CI report reads the same issue's comments.
	thread := slices.Clone(comments)

	if pr != nil {
		onPR, err := c.ListComments(ctx, workflow.IssueRef{Repo: ref.Repo, Number: pr.Number})
		if err != nil {
			return nil, err
		}

		thread = append(thread, onPR...)
	}

	slices.SortStableFunc(thread, func(a, b workflow.Item) int {
		return cmp.Compare(a.ID, b.ID)
	})

	return workflow.Guidance(thread, ref.Repo.Owner()), nil
}

// ciFeedback is the CI failure report that sent this run back, when the report comes after the
// handoff that sent the pull request to review. A report from an earlier round is stale and left out.
func ciFeedback(comments []workflow.Item, ref workflow.IssueRef) (string, error) {
	report, ok, err := workflow.LatestCIFailure(comments, ref.Repo.Owner())
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

// outputProblem turns an unreadable output file into the error a rejected one returns. The error
// carries the file's name, which the comment shows once, so the problem does not repeat it.
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
		fmt.Sprintf("File: `%s`", rejected.File), "",
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
// ticket's map the way an applied resolution does. Both are keyed, so applying the same output twice
// changes nothing.
func (s session) resolveResearch(
	ctx context.Context, j job, repo workflow.Repo, branch string, parent workflow.Issue, out workflow.ResearcherOutput,
) error {
	ref := workflow.IssueRef{Repo: repo, Number: j.issue.Number}

	if err := j.c.UpsertComment(ctx, ref, workflow.ResearchMarker(branch),
		workflow.RenderResolution(ref.Repo, branch, out.Resolution)); err != nil {
		return err
	}

	return appendDecision(ctx, j.c, repo, parent, j.issue, out.DecisionLine)
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

	// A repeat of this commit's review posts nothing: the same head SHA is reviewed once. Only the
	// factory's own review counts, so another author's marker cannot suppress the verdict.
	if len(workflow.FactoryStartsWith(reviews, workflow.ReviewMarker(sha), repo.Owner())) == 0 {
		body := workflow.RenderReview(sha, out)
		if err := c.PostReview(ctx, workflow.IssueRef{Repo: repo, Number: pr}, sha, body, out.Verdict); err != nil {
			return 1, err
		}
	}

	if out.Verdict == workflow.Approve {
		// A clean verdict asks a human writer to review and merge. A bot-authored issue, such as a
		// promoted retro group, names no human itself, so the latest human writer who commented is
		// asked instead.
		reviewer, err := mergeReviewer(ctx, d, c, repo, issue, comments)
		if err != nil {
			return 1, err
		}

		if reviewer == "" {
			d.log().Warn("no human writer to ask to review and merge", "repo", repo, "issue", issue.Number,
				"pr", pr, "author", issue.Author)

			return workflow.ReviewClean, nil
		}

		return workflow.ReviewClean, c.RequestReviewers(ctx, workflow.IssueRef{Repo: repo, Number: pr}, []string{reviewer})
	}

	if workflow.RoundLimitReached(workflow.ReviewRounds(reviews, comments, sha, repo.Owner()), limit) {
		return workflow.ReviewExhausted, nil
	}

	return workflow.ReviewFindings, nil
}

// mergeReviewer is the human writer a clean verdict asks to review and merge: the issue's author
// when a writer wrote it, otherwise the latest human writer who commented. Empty means no human
// writer was found.
func mergeReviewer(
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
			d.log().Info("asking the latest human writer to review and merge", "repo", repo,
				"issue", issue.Number, "reviewer", login)

			return login, nil
		}
	}

	return "", nil
}
