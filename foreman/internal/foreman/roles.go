package foreman

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/prompt"
	"github.com/nateinaction/factory/foreman/internal/telemetry"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// prepareImplementer checks out the issue's branch, builds the prompt from the clone's docs and the
// review feedback, and applies the Implementer's output once the agent returns.
func prepareImplementer(ctx context.Context, rc runCtx) (prepared, error) {
	repo, number := rc.spec.ref.Repo, rc.spec.ref.Number
	branch := workflow.BranchName(number)

	pr, feedback, start, err := rc.s.prepare(ctx, rc.j.c, repo, branch)
	if err != nil {
		return prepared{}, err
	}

	text, err := buildImplementerPrompt(ctx, rc.j.c, repo, rc.j.issue, rc.paths.workdir, feedback, rc.paths.out)
	if err != nil {
		return prepared{}, err
	}

	return prepared{
		prompt:  text,
		failKey: headKeyOf,
		after: func(ctx context.Context, s session, _ opencode.Result) outcome {
			out, err := readOutput(ctx, rc.spans(), rc.paths.out, workflow.ImplementerOutputFile,
				func(text string) (workflow.ImplementerOutput, error) {
					return workflow.ParseImplementerOutput(text, pr != nil, feedback)
				})
			if err != nil {
				return rejected(ctx, rc, err, s.headKey(ctx))
			}
			// The retro is the run's epilogue whether or not the output applied.
			err = s.applyImplementer(ctx, rc.j, repo, branch, start, pr, out)

			return outcome{retroKey: s.headKey(ctx), err: err}
		},
	}, nil
}

// prepareResearcher clones the repository, reads its docs and the ticket's guidance, and publishes
// the Researcher's findings once the agent returns. The clone and the reads are foreman's, outside
// the sandbox; only the model and its retro run inside.
func prepareResearcher(ctx context.Context, rc runCtx) (prepared, error) {
	if err := rc.s.clone(ctx, rc.spec.ref.Repo); err != nil {
		return prepared{}, err
	}

	docs, err := prompt.ReadRepoDocs(rc.paths.workdir)
	if err != nil {
		return prepared{}, err
	}

	thread, err := guidance(ctx, rc.j.c, rc.spec.ref)
	if err != nil {
		return prepared{}, err
	}

	return prepared{
		prompt:  prompt.Researcher(docs, rc.spec.ref.Repo.String(), rc.j.issue, thread, rc.paths.out),
		failKey: headKeyOf,
		after: func(ctx context.Context, s session, _ opencode.Result) outcome {
			return researchAfter(ctx, rc, s)
		},
	}, nil
}

// researchAfter reads the Researcher's output and applies it.
func researchAfter(ctx context.Context, rc runCtx, s session) outcome {
	out, err := readOutput(ctx, rc.spans(), rc.paths.out, workflow.ResearcherOutputFile, workflow.ParseResearcherOutput)
	if err != nil {
		return rejected(ctx, rc, err, s.headKey(ctx))
	}

	branch, err := workflow.ResearchBranch(out.Branch)
	if err != nil {
		return rejected(ctx, rc,
			&workflow.OutputError{File: workflow.ResearcherOutputFile, Problems: []string{err.Error()}},
			s.headKey(ctx))
	}

	sha, err := s.applyResearch(ctx, rc.j, rc.spec, branch, out)
	if err != nil {
		return outcome{retroKey: s.headKey(ctx), err: err}
	}

	return outcome{retroKey: sha}
}

// reviewSpecFile is the issue spec the Reviewer reads, in its scratch directory beside its output.
const reviewSpecFile = "issue.md"

// prepareReviewer checks out the pull request's branch and writes the issue where the Reviewer reads
// it. The verdict is the output's, and the retro is keyed by the reviewed commit.
func prepareReviewer(ctx context.Context, rc runCtx) (prepared, error) {
	repo := rc.spec.ref.Repo
	branch := workflow.BranchName(rc.spec.ref.Number)

	pr, base, err := reviewTarget(ctx, rc.j.c, repo, branch)
	if err != nil {
		return prepared{}, err
	}

	sha, err := rc.s.checkoutForReview(ctx, repo, branch)
	if err != nil {
		return prepared{}, err
	}

	specPath := filepath.Join(rc.paths.scratch, reviewSpecFile)

	comments, err := rc.j.c.ListComments(ctx, workflow.IssueRef{Repo: repo, Number: rc.spec.ref.Number})
	if err != nil {
		return prepared{}, err
	}

	finalSpec, _, err := workflow.StoredSpec(comments, repo.Owner())
	if err != nil {
		return prepared{}, err
	}

	docs, err := writeSpec(specPath, rc.paths.workdir, rc.j.issue, finalSpec)
	if err != nil {
		return prepared{}, err
	}

	return prepared{
		prompt:          prompt.Reviewer(docs, base, specPath, rc.paths.out),
		toolchainBranch: base,
		extraWrite:      []string{specPath},
		// The retro is keyed by the reviewed PR head SHA, on every path that reached the model.
		failKey: func(context.Context, session) string { return sha },
		after: func(ctx context.Context, _ session, _ opencode.Result) outcome {
			out, err := readOutput(ctx, rc.spans(), rc.paths.out, workflow.ReviewerOutputFile, workflow.ParseReviewerOutput)
			if err != nil {
				return rejected(ctx, rc, err, sha)
			}

			ctx, span := rc.spans().start(ctx, telemetry.SpanPublishReview)

			code, err := postVerdict(ctx, rc.d, rc.j.c, repo, pr.Number, sha, out, rc.j.issue, rc.spec.limit)
			if err != nil {
				markFailureOf(span, err)
			}

			span.End()

			return outcome{code: code, retroKey: sha, err: err}
		},
	}, nil
}

// preparePlanner clones the default branch read-only, reads its docs and the issue thread, and applies
// one round once the agent returns. The retro is the epilogue of the final-round run only, keyed by
// the id of the comment that stored the proposal.
func preparePlanner(ctx context.Context, rc runCtx) (prepared, error) {
	branch, err := rc.j.c.DefaultBranch(ctx, rc.spec.ref.Repo)
	if err != nil {
		return prepared{}, err
	}

	if err := rc.s.cloneDefault(ctx, rc.spec.ref.Repo, branch); err != nil {
		return prepared{}, err
	}

	docs, err := prompt.ReadRepoDocs(rc.paths.workdir)
	if err != nil {
		return prepared{}, err
	}

	comments, err := rc.j.c.ListComments(ctx, workflow.IssueRef{Repo: rc.spec.ref.Repo, Number: rc.spec.ref.Number})
	if err != nil {
		return prepared{}, err
	}

	return prepared{
		prompt:          prompt.Planner(docs, rc.spec.ref.Repo.String(), rc.j.issue, comments, rc.paths.out),
		toolchainBranch: branch,
		after: func(ctx context.Context, _ session, _ opencode.Result) outcome {
			out, err := readOutput(ctx, rc.spans(), rc.paths.out, workflow.PlannerOutputFile, workflow.ParsePlannerOutput)
			if err != nil {
				return rejected(ctx, rc, err, "")
			}

			commentID, final, err := applyPlannerRound(ctx, rc.j, rc.d.runID(), rc.spec.ref.Repo, rc.spec.ref.Number, out)
			if err != nil || !final {
				return outcome{err: err}
			}

			return outcome{retroKey: strconv.FormatInt(commentID, 10)}
		},
	}, nil
}

// prepareCurator reads the retro log and the dashboard state, and applies the Curator's proposals once
// the agent returns. Its App holds no Contents permission, so it clones nothing and resolves no
// toolchain: it only creates the empty working directory the agent runs in, and its prompt carries no
// repository docs. It runs no retro.
func prepareCurator(ctx context.Context, rc runCtx) (prepared, error) {
	repo := rc.spec.ref.Repo

	if err := os.MkdirAll(rc.paths.workdir, privateDirMode); err != nil {
		return prepared{}, fmt.Errorf("create the Curator's empty working directory: %w", err)
	}

	reports, state, private, err := curatorInputs(ctx, rc.d, rc.j.c, repo, rc.j.issue)
	if err != nil {
		return prepared{}, err
	}

	view := workflow.BuildDashboard(reports, state, rc.d.now())
	ref := workflow.IssueRef{Repo: repo, Number: rc.spec.ref.Number}

	return prepared{
		prompt:      prompt.Curator(ref, private, reports, view, rc.paths.out),
		noToolchain: true,
		after: func(ctx context.Context, s session, _ opencode.Result) outcome {
			out, err := readOutput(ctx, rc.spans(), rc.paths.out, workflow.CuratorOutputFile, workflow.ParseCuratorOutput)
			if err != nil {
				// The dashboard is never commented on, so a rejected file is only logged; the claim ages
				// out and the next poll retries.
				return outcome{err: err}
			}

			return outcome{err: s.applyCurator(ctx, rc.j, repo, rc.spec.ref.Number, out, private)}
		},
	}, nil
}
