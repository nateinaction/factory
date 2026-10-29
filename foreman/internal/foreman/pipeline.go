package foreman

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/sandbox"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// runResult is what one run hands back: the usage its one log line reports, and the code `start`
// reads when the run succeeds. Only the Reviewer's code is a verdict.
type runResult struct {
	usage opencode.Usage
	code  int
}

// run is one run of an agent role: the edge it reads the world through, the agent role's entry, and
// the settings it runs under.
type run struct {
	d Deps
	// role is the agent role, as the command edge named it.
	role workflow.Role
	// entry is the agent role's dispatch entry, resolved once when the run started. No step looks it
	// up again.
	entry dispatchEntry
	spec  runSpec
}

// runEnv is what the run's proxies hand the rest of the run: the secrets read at the edge, the
// addresses the model reaches its proxies at, and the egress policy the sandbox enforces.
type runEnv struct {
	// sec are the factory half's two secrets, read at the edge.
	sec secrets
	// baseURL is the OpenRouter base URL the model is told to call.
	baseURL string
	// token is the run's proxy token, which is the model's OpenRouter credential.
	token string
	// egressPort is the loopback port the run's srt settings point at.
	egressPort int
	// network is the agent role's egress policy: what the model may reach through the egress proxy.
	network sandbox.Network
}

// runCtx is what an agent role's prepare step reads: the run and the clone it works.
type runCtx struct {
	run
	j     job
	s     session
	paths runPaths
}

// prepared is what an agent role's prepare step hands the pipeline: how to start the agent, and
// what to do when it returns.
type prepared struct {
	// prompt is the agent's prompt.
	prompt string
	// toolchainBranch is the branch whose dev environment the sandbox uses; empty is the default.
	toolchainBranch string
	// extraWrite names output files the agent role writes outside the clone, besides its retro.
	extraWrite []string
	// failKey is the retro key of a run whose agent failed. Nil means the agent role runs no retro
	// then.
	failKey func(ctx context.Context, s session) string
	// after is the agent role's post-agent step: it reads the agent's output and applies it.
	after func(ctx context.Context, s session, res opencode.Result) outcome
}

// outcome is how an agent role's post-agent step ended.
type outcome struct {
	// code is the verdict code a successful run returns.
	code int
	// retroKey is the key the retro comment is filed under. An empty key means this run has no
	// retro: there is nothing to file it under.
	retroKey string
	// err is the run's failure, if any.
	err error
}

// roleRun is what an agent role supplies to the run pipeline.
type roleRun struct {
	// identity is the git committer the agent role's commits carry; nil for an agent role that
	// commits nothing.
	identity []string
	// retro is whether the agent role's run ends with a retro, which makes its retro file writable.
	retro bool
	// prepare clones, reads the agent role's inputs and builds its prompt.
	prepare func(ctx context.Context, rc runCtx) (prepared, error)
}

// dispatchEntry is everything dispatch knows about an agent role, so a new role is one entry in
// the agentRoles table: how `run` runs it, how `start` routes its exit code, and what its run
// paths are called.
type dispatchEntry struct {
	// run is the pipeline `foreman run` executes.
	run roleRun
	// route is the outcome both halves record for a run's exit code: the factory half before the
	// retro, and `start` afterwards as the safety net. limit is the review round limit, which only
	// the Reviewer reads.
	route func(limit int, ref workflow.IssueRef, code int) route
	// names are the names the agent role's run directories and output file carry.
	names roleNames
}

// route is the outcome of a run: the transition to record, or the line to print when there is
// none.
type route struct {
	// dst is the state the issue moves to from the agent role's running label; empty records no
	// transition.
	dst workflow.State
	// text is the transition's comment, or the line printed when dst is empty.
	text string
	// exit is the code `start` returns.
	exit int
	// err fails `start` instead of recording anything.
	err error
}

// dispatchEntryFor is the one lookup by agent role that both `run` and `start` dispatch through.
func dispatchEntryFor(role workflow.Role) (dispatchEntry, bool) {
	entry, ok := agentRoles()[role]

	return entry, ok
}

// agentRoles is the table of agent roles: a new role is one entry here.
func agentRoles() map[workflow.Role]dispatchEntry {
	return map[workflow.Role]dispatchEntry{
		workflow.Planner: {
			run:   roleRun{retro: true, prepare: preparePlanner},
			route: routePlanner,
			names: roleNames{"plan", "planner", workflow.PlannerOutputFile},
		},
		workflow.Implementer: {
			run:   roleRun{identity: committerIdentity(), retro: true, prepare: prepareImplementer},
			route: routeImplementer,
			names: roleNames{"issue", "implementer", workflow.ImplementerOutputFile},
		},
		workflow.Researcher: {
			run:   roleRun{identity: committerIdentity(), retro: true, prepare: prepareResearcher},
			route: routeResearcher,
			names: roleNames{"research", "researcher", workflow.ResearcherOutputFile},
		},
		workflow.Reviewer: {
			run:   roleRun{retro: true, prepare: prepareReviewer},
			route: routeReviewer,
			names: roleNames{"review", "reviewer", workflow.ReviewerOutputFile},
		},
		workflow.Curator: {
			run:   roleRun{prepare: prepareCurator},
			route: routeCurator,
			names: roleNames{"curator", "curator-out", workflow.CuratorOutputFile},
		},
	}
}

// failed is the route of a run that failed: the issue moves to failed with the agent role's
// message.
func failed(role workflow.Role, code int) route {
	return route{dst: workflow.Failed, text: failureText(role, code), exit: 1}
}

// routePlanner records nothing for a posted round: the factory half already moved the issue to waiting.
func routePlanner(_ int, _ workflow.IssueRef, code int) route {
	if code == 0 {
		return route{text: "The Planner posted a round; see the issue."}
	}

	return failed(workflow.Planner, code)
}

// routeImplementer queues a finished implementation for review.
func routeImplementer(_ int, _ workflow.IssueRef, code int) route {
	if code == 0 {
		return route{
			dst:  workflow.Review,
			text: "The Implementer finished; see the linked pull request. Queued for review.",
		}
	}

	return failed(workflow.Implementer, code)
}

// routeResearcher closes a resolved ticket, with no label replacing the claim.
func routeResearcher(_ int, _ workflow.IssueRef, code int) route {
	if code == 0 {
		return route{dst: workflow.Closed}
	}

	return failed(workflow.Researcher, code)
}

// routeReviewer routes the issue by what the review found.
func routeReviewer(limit int, _ workflow.IssueRef, code int) route {
	switch code {
	case workflow.ReviewClean:
		return route{dst: workflow.Done, text: "Review found no high or medium issues; see the pull request."}
	case workflow.ReviewFindings:
		return route{
			dst:  workflow.Implement,
			text: "Review found high or medium issues, posted on the pull request. Sent back to the Implementer.",
		}
	case workflow.ReviewExhausted:
		return route{dst: workflow.Failed, exit: 1, text: fmt.Sprintf(
			"Review still found high or medium issues after %d rounds. A person needs to look at the pull request.",
			limit)}
	}

	return failed(workflow.Reviewer, code)
}

// routeCurator records nothing on GitHub: the dashboard carries no factory label, and a finished run's
// output was already applied to its body.
func routeCurator(_ int, ref workflow.IssueRef, code int) route {
	if code != 0 {
		return route{exit: 1, err: fmt.Errorf("the Curator failed on %s (exit %d); see the dagu run log", ref, code)}
	}

	return route{text: "The Curator finished; see the retro dashboard."}
}

// committerIdentity is the git committer of every run that commits: the Implementer and the
// Researcher share it.
func committerIdentity() []string {
	return []string{"-c", "user.name=factory-agent", "-c", "user.email=factory-agent@users.noreply.github.com"}
}

// pipeline is one run, top to bottom: its private directories, its egress policy and secrets, its
// proxies, then its work. Each step's cleanup is deferred where the step opened the resource, so the
// run releases everything it took, however it ends.
func (r run) pipeline(ctx context.Context) (result runResult, err error) {
	files, err := newSandboxFiles(r.d.Settings.WorkRoot, r.role, r.spec.ref.Number)
	if err != nil {
		return runResult{}, err
	}
	// The run directory and the toolchain clone are removed before the run's code is returned, so a
	// failure anywhere below leaves nothing behind.
	defer func() { err = errors.Join(err, files.remove()) }()

	// The domains the egress proxy saw are the run's, and are read in the defer below.
	var domains []string

	// The run's one usage line is logged even when the run failed, once the proxies have added what
	// they read: that merge happens in the defer below, which runs first.
	defer func() {
		r.d.logUsage(opencode.NewUsageReport(r.role, r.spec.ref, r.d.Host, r.spec.agent, result.usage).
			WithDomains(domains))
	}()

	network, err := sandbox.ParseNetwork(r.spec.agent.Egress.AllowedDomains)
	if err != nil {
		return runResult{}, err
	}

	sec, err := readSecrets(r.d.Settings, r.role)
	if err != nil {
		return runResult{}, err
	}

	token, err := r.d.token()
	if err != nil {
		return runResult{}, err
	}

	p, err := r.d.proxy(sec.openrouterKey, token)
	if err != nil {
		return runResult{}, err
	}

	e, err := r.d.egress(ctx, network, p.URL(), token)
	if err != nil {
		// A failure to stop the key proxy is part of the run's outcome, not a diagnostic to drop.
		return runResult{}, errors.Join(err, p.Close())
	}
	// The usage the key proxy read and the domains the egress proxy saw are the run's. Both are read
	// before the proxies stop: a response still being relayed when the model exits cannot be counted,
	// and Close is what ends it.
	defer func() {
		result.usage = result.usage.Use(reported(p.Usage()))
		domains = e.Domains()
		err = errors.Join(err, e.Close(), p.Close())
	}()

	env := runEnv{
		sec: sec, baseURL: e.URL(), token: token, egressPort: proxyPort(e.Address()), network: network,
	}

	return r.work(ctx, env, files)
}

// work is the run's work, top to bottom: the job it reads, the clone its agent role prepares, the
// sandboxed model, the agent role's post-agent step, the outcome the run settles before its retro,
// and the retro itself.
func (r run) work(ctx context.Context, env runEnv, files sandboxFiles) (runResult, error) {
	paths, err := newRunPaths(r.d.Settings.WorkRoot, r.entry.names, r.spec.ref.Number)
	if err != nil {
		return runResult{}, err
	}

	// The model may touch only this run's directories and output file outside its clone. The inline
	// opencode config denies every other external path, so a sibling run's scratch directory is never
	// reachable even though it sits under the same work root.
	j, err := r.loadJob(ctx, env, dedupe([]string{files.dir, paths.scratch, r.retroFile()}), files.db())
	if err != nil {
		return runResult{}, err
	}

	rc := runCtx{run: r, j: j, s: newSession(r.d, j, paths.workdir, r.entry.run.identity), paths: paths}

	prep, err := r.entry.run.prepare(ctx, rc)
	if err != nil {
		return runResult{}, err
	}

	s, err := rc.sandboxed(ctx, files, env, prep)
	if err != nil {
		return runResult{}, err
	}

	res, err := s.agent(ctx, opencode.Run{
		Role: r.role, Prompt: prep.prompt, Timeout: r.spec.agent.Deadline(), Steps: r.spec.agent.Steps,
	})
	if err != nil {
		// A model that failed is the run's failure too, but the run still settles and still gets the
		// retro its agent role keeps for a failed run.
		r.settle(ctx, 1, err)

		return runResult{usage: res.Usage.Add(failedRetro(ctx, rc, s, prep, res, err))}, err
	}

	oc := prep.after(ctx, s, res)
	// The outcome is settled before the retro starts, so a slow retro can never get its run marked
	// dead.
	r.settle(ctx, oc.code, oc.err)

	usage := res.Usage
	if oc.retroKey != "" {
		usage = usage.Add(s.retroEpilogue(
			ctx, j, r.spec.ref.Repo, r.spec.ref.Number, r.role, res.Session, oc.retroKey, oc.err, r.spec.retro,
		))
	}

	return runResult{usage: usage, code: oc.code}, oc.err
}

// retroFile is where the run's retro writes its findings, outside the clone and the run's own
// directories. An agent role that keeps no retro has no file, so the model may not write one.
func (r run) retroFile() string {
	if !r.entry.run.retro {
		return ""
	}

	return retroPath(r.d.Settings.WorkRoot, r.spec.ref.Number, r.role)
}

// settle applies the outcome the run's exit code routes to, before the retro starts, so the retro is
// only ever an epilogue and a slow one can never get its run marked dead. It is best effort: a
// transition it cannot record is logged and `start` applies the same one from the run's exit code
// afterwards.
func (r run) settle(ctx context.Context, code int, runErr error) {
	rt := r.entry.route(r.spec.limit, r.spec.ref, startCode(runErr, code))
	if rt.dst == "" {
		return
	}

	// A role with no queue has nowhere to move the issue to, so the run stops with no transition.
	q, ok := workflow.QueueOf(r.role)
	if !ok {
		r.d.log().Warn("not settling the run: this agent role has no handoff queue",
			"agent_role", r.role, "repo", r.spec.ref.Repo, "issue", r.spec.ref.Number, "state", rt.dst)

		return
	}

	if err := finishRun(ctx, r.d, r.role, r.spec.ref, q.Running, rt.dst, rt.text); err != nil {
		r.d.log().Warn("cannot record the run's outcome before its retro; start will retry it",
			"agent_role", r.role, "repo", r.spec.ref.Repo, "issue", r.spec.ref.Number, "state", rt.dst, "error", err)
	}
}

// failedRetro runs the retro of a run whose agent failed, for an agent role that keeps one then. A
// run whose key cannot be read has no retro: nothing would file it.
func failedRetro(
	ctx context.Context, rc runCtx, s session, prep prepared, res opencode.Result, err error,
) opencode.Usage {
	key := ""
	if prep.failKey != nil {
		key = prep.failKey(ctx, s)
	}

	if key == "" {
		return opencode.Usage{}
	}

	return s.retroEpilogue(
		ctx, rc.j, rc.spec.ref.Repo, rc.spec.ref.Number, rc.role, res.Session, key, err, rc.spec.retro,
	)
}

// headKeyOf is the clone's commit as a retro key.
func headKeyOf(ctx context.Context, s session) string { return s.headKey(ctx) }

// rejected is the outcome of a run whose output file was rejected: nothing is applied, but the run
// still gets its retro when the run has a key for it. A key of empty means the role's retro would
// have nothing to file under, as with a Planner round that is not final.
func rejected(ctx context.Context, rc runCtx, err error, key string) outcome {
	return outcome{
		retroKey: key,
		err:      rejectOutput(ctx, rc.j.c, rc.spec.ref.Repo, rc.spec.ref.Number, rc.role, err),
	}
}

// readOutput reads an agent role's output file and parses it strictly. A file foreman cannot read is an
// output error too, so a model that wrote none fails the same way as one that wrote a bad file.
func readOutput[T any](path, file string, parse func(text string) (T, error)) (T, error) {
	raw, err := readScratch(path)
	if err != nil {
		var none T

		return none, outputProblem(file, err)
	}

	return parse(string(raw))
}

// runPaths are a run's clone, its scratch directory and its output file. The scratch directory and the
// clone are cleared, so a re-run cannot read a stale result; the output sits outside the clone.
type runPaths struct {
	workdir, scratch, out string
}

// runPathsFor is where the agent role's run for an issue works, scratches and writes, before anything
// is created: the agent role table owns the names.
func runPathsFor(root string, names roleNames, number int) runPaths {
	named := func(prefix string) string { return filepath.Join(root, fmt.Sprintf("%s-%d", prefix, number)) }

	return runPaths{
		workdir: named(names.workdir),
		scratch: named(names.scratch),
		out:     filepath.Join(named(names.scratch), names.file),
	}
}

// newRunPaths clears and returns the paths of the agent role's run for an issue.
func newRunPaths(root string, names roleNames, number int) (runPaths, error) {
	paths := runPathsFor(root, names, number)

	// freshPath and scratchDir take a name under the work root, so they get the paths' last element.
	workdir, err := freshPath(root, filepath.Base(paths.workdir))
	if err != nil {
		return runPaths{}, err
	}

	scratch, err := scratchDir(root, filepath.Base(paths.scratch))
	if err != nil {
		return runPaths{}, err
	}

	return runPaths{workdir: workdir, scratch: scratch, out: filepath.Join(scratch, filepath.Base(paths.out))}, nil
}

// roleNames are the names an agent role's run directories and output file carry.
type roleNames struct {
	workdir, scratch, file string
}
