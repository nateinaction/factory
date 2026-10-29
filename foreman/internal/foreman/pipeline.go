package foreman

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/nateinaction/factory/foreman/internal/config"
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

// runCtx is what a role's prepare step reads: the run and the clone it works.
type runCtx struct {
	d     Deps
	spec  runSpec
	role  workflow.Role
	j     job
	s     session
	paths runPaths
}

// prepared is what a role's prepare step hands the pipeline: how to start the agent, and what to do
// when it returns.
type prepared struct {
	// prompt is the agent's prompt.
	prompt string
	// toolchainBranch is the branch whose dev environment the sandbox uses; empty is the default.
	toolchainBranch string
	// extraWrite names output files the role writes outside the clone, besides its retro.
	extraWrite []string
	// failKey is the retro key of a run whose agent failed. Nil means the role runs no retro then.
	failKey func(ctx context.Context, s session) string
	// after is the role's post-agent step: it reads the agent's output and applies it.
	after func(ctx context.Context, s session, res opencode.Result) outcome
}

// outcome is how a role's post-agent step ended.
type outcome struct {
	// code is the verdict code a successful run returns.
	code int
	// retro asks for the retro epilogue, keyed by key. The retro never changes the run's outcome.
	retro bool
	key   string
	// err is the run's failure, if any.
	err error
}

// roleRun is what an agent role supplies to the run pipeline.
type roleRun struct {
	// identity is the git committer the role's commits carry; nil for a role that commits nothing.
	identity []string
	// retro is whether the role's run ends with a retro, which makes its retro file writable.
	retro bool
	// prepare clones, reads the role's inputs and builds its prompt.
	prepare func(ctx context.Context, rc runCtx) (prepared, error)
}

// agentRole is everything dispatch knows about an agent role, so a new role is one entry in
// agentRoleFor: how `run` runs it, and how `start` routes its exit code.
type agentRole struct {
	// run is the pipeline `foreman run` executes.
	run roleRun
	// route is what `start` records for the exit code of the role's run.
	route func(cfg config.Config, ref workflow.IssueRef, code int) route
}

// route is what `start` does with a run's exit code.
type route struct {
	// dst is the state the issue moves to from the role's running label; empty records no transition.
	dst workflow.State
	// text is the transition's comment, or the line printed when dst is empty.
	text string
	// exit is the code `start` returns.
	exit int
	// err fails `start` instead of recording anything.
	err error
}

// agentRoleFor is the one lookup by agent role that both `run` and `start` dispatch through.
func agentRoleFor(role workflow.Role) (agentRole, bool) {
	switch role {
	case workflow.Planner:
		return agentRole{run: roleRun{retro: true, prepare: preparePlanner}, route: routePlanner}, true
	case workflow.Implementer:
		return agentRole{
			run:   roleRun{identity: committerIdentity(), retro: true, prepare: prepareImplementer},
			route: routeImplementer,
		}, true
	case workflow.Researcher:
		return agentRole{
			run:   roleRun{identity: committerIdentity(), retro: true, prepare: prepareResearcher},
			route: routeResearcher,
		}, true
	case workflow.Reviewer:
		return agentRole{run: roleRun{retro: true, prepare: prepareReviewer}, route: routeReviewer}, true
	case workflow.Curator:
		return agentRole{run: roleRun{prepare: prepareCurator}, route: routeCurator}, true
	}

	return agentRole{}, false
}

// failed is the route of a run that failed: the issue moves to failed with the role's message.
func failed(role workflow.Role, code int) route {
	return route{dst: workflow.Failed, text: failureText(role, code), exit: 1}
}

// routePlanner records nothing for a posted round: the factory half already moved the issue to waiting.
func routePlanner(_ config.Config, _ workflow.IssueRef, code int) route {
	if code == 0 {
		return route{text: "The Planner posted a round; see the issue."}
	}

	return failed(workflow.Planner, code)
}

// routeImplementer queues a finished implementation for review.
func routeImplementer(_ config.Config, _ workflow.IssueRef, code int) route {
	if code == 0 {
		return route{
			dst:  workflow.Review,
			text: "The Implementer finished; see the linked pull request. Queued for review.",
		}
	}

	return failed(workflow.Implementer, code)
}

// routeResearcher closes a resolved ticket, with no label replacing the claim.
func routeResearcher(_ config.Config, _ workflow.IssueRef, code int) route {
	if code == 0 {
		return route{dst: workflow.Closed}
	}

	return failed(workflow.Researcher, code)
}

// routeReviewer routes the issue by what the review found.
func routeReviewer(cfg config.Config, _ workflow.IssueRef, code int) route {
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
			cfg.ReviewRoundLimit)}
	}

	return failed(workflow.Reviewer, code)
}

// routeCurator records nothing on GitHub: the dashboard carries no factory label, and a finished run's
// output was already applied to its body.
func routeCurator(_ config.Config, ref workflow.IssueRef, code int) route {
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

// runRole runs one agent role's pipeline in a fresh sandbox directory and logs the run's one usage
// line, even when the run failed.
func runRole(ctx context.Context, d Deps, spec runSpec, role workflow.Role, rr roleRun) (int, error) {
	return withSandboxRun(d, spec, role, func(files sandboxFiles) (runResult, []string, error) {
		return withRunSecrets(ctx, d, spec, role,
			func(sec secrets, baseURL, proxyAddr string, network sandbox.Network) (runResult, error) {
				return pipeline(ctx, d, spec, role, rr, sec, baseURL, proxyAddr, network, files)
			})
	})
}

// pipeline is every role's run, with its proxies already up: job, clone and inputs, sandbox, agent,
// then the role's post-agent step and the retro epilogue.
func pipeline(
	ctx context.Context, d Deps, spec runSpec, role workflow.Role, rr roleRun, sec secrets,
	baseURL, proxyAddr string, network sandbox.Network, files sandboxFiles,
) (runResult, error) {
	j, err := loadJob(ctx, d, sec, baseURL, spec.repo, role, spec.agent, spec.number, files.db(role))
	if err != nil {
		return runResult{}, err
	}

	paths, err := newRunPaths(d.Settings.WorkRoot, role, spec.number)
	if err != nil {
		return runResult{}, err
	}

	rc := runCtx{d: d, spec: spec, role: role, j: j, s: newSession(d, j, paths.workdir, rr.identity), paths: paths}

	prep, err := rr.prepare(ctx, rc)
	if err != nil {
		return runResult{}, err
	}

	extra := slices.Clone(prep.extraWrite)
	if rr.retro {
		extra = append(extra, retroPath(d.Settings.WorkRoot, spec.number, role))
	}

	s, err := d.sandboxedWithToolchain(
		ctx, rc.s, j, sec, spec.repo, prep.toolchainBranch, files, role, paths.workdir, paths.out, extra,
		network, proxyAddr,
	)
	if err != nil {
		return runResult{}, err
	}

	res, err := s.agent(ctx, opencode.Run{
		Role: role, Prompt: prep.prompt, Timeout: spec.agent.Deadline(), Steps: spec.agent.Steps,
	})
	if err != nil {
		return runResult{usage: res.Usage.Add(failedRetro(ctx, rc, s, prep, res, err))}, err
	}

	oc := prep.after(ctx, s, res)
	usage := res.Usage

	if oc.retro {
		usage = usage.Add(s.retroEpilogue(
			ctx, j, spec.repo, spec.number, role, res.Session, oc.key, oc.err, spec.retro,
		))
	}

	return runResult{usage: usage, code: oc.code}, oc.err
}

// failedRetro runs the retro of a run whose agent failed, for a role that keeps one then.
func failedRetro(
	ctx context.Context, rc runCtx, s session, prep prepared, res opencode.Result, err error,
) opencode.Usage {
	if prep.failKey == nil {
		return opencode.Usage{}
	}

	return s.retroEpilogue(
		ctx, rc.j, rc.spec.repo, rc.spec.number, rc.role, res.Session, prep.failKey(ctx, s), err, rc.spec.retro,
	)
}

// headKeyOf is the clone's commit as a retro key.
func headKeyOf(ctx context.Context, s session) string { return s.headKey(ctx) }

// rejected is the outcome of a run whose output file was rejected: nothing is applied and no retro
// runs.
func rejected(ctx context.Context, rc runCtx, err error) outcome {
	return outcome{err: rejectOutput(ctx, rc.j.c, rc.spec.repo, rc.spec.number, rc.role, err)}
}

// readOutput reads a role's output file and parses it strictly. A file foreman cannot read is an
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

// newRunPaths clears and returns the paths of the role's run for an issue.
func newRunPaths(root string, role workflow.Role, number int) (runPaths, error) {
	names, ok := pathNames(role)
	if !ok {
		return runPaths{}, fmt.Errorf("no run paths for agent role %q", role)
	}

	workdir, err := freshPath(root, fmt.Sprintf("%s-%d", names.workdir, number))
	if err != nil {
		return runPaths{}, err
	}

	scratch, err := scratchDir(root, fmt.Sprintf("%s-%d", names.scratch, number))
	if err != nil {
		return runPaths{}, err
	}

	return runPaths{workdir: workdir, scratch: scratch, out: filepath.Join(scratch, names.file)}, nil
}

// roleNames are the names a role's run directories and output file carry.
type roleNames struct {
	workdir, scratch, file string
}

// pathNames is how a role's run paths are named.
func pathNames(role workflow.Role) (roleNames, bool) {
	switch role {
	case workflow.Planner:
		return roleNames{"plan", "planner", workflow.PlannerOutputFile}, true
	case workflow.Implementer:
		return roleNames{"issue", "implementer", workflow.ImplementerOutputFile}, true
	case workflow.Researcher:
		return roleNames{"research", "researcher", workflow.ResearcherOutputFile}, true
	case workflow.Reviewer:
		return roleNames{"review", "reviewer", workflow.ReviewerOutputFile}, true
	case workflow.Curator:
		return roleNames{"curator", "curator-out", workflow.CuratorOutputFile}, true
	}

	return roleNames{}, false
}
