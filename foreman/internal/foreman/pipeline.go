package foreman

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"

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

// roleRunFor is the pipeline parameters of an agent role.
func roleRunFor(role workflow.Role) (roleRun, bool) {
	switch role {
	case workflow.Planner:
		return roleRun{retro: true, prepare: preparePlanner}, true
	case workflow.Implementer:
		return roleRun{identity: committerIdentity(), retro: true, prepare: prepareImplementer}, true
	case workflow.Researcher:
		return roleRun{identity: committerIdentity(), retro: true, prepare: prepareResearcher}, true
	case workflow.Reviewer:
		return roleRun{retro: true, prepare: prepareReviewer}, true
	case workflow.Curator:
		return roleRun{prepare: prepareCurator}, true
	}

	return roleRun{}, false
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
