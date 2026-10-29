package foreman

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/prompt"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// headKey is the clone's commit as a run key, or empty when git cannot read it. A key that cannot be
// read drops the retro; it never fails the run.
func (s session) headKey(ctx context.Context) string {
	sha, err := s.head(ctx)
	if err != nil {
		s.d.log().Warn("cannot read the commit for the retro's run key", "error", err)

		return ""
	}

	return sha
}

// retroPath is where a run's retro.json is written, in the work root outside the clone and the
// run directory.
func retroPath(root string, number int, role workflow.Role) string {
	return filepath.Join(root, fmt.Sprintf("issue-%d-%s.retro.json", number, role))
}

// retroEpilogue runs a run's retro after its work, in the same session. It is best effort: a skipped,
// dropped or invalid retro is logged and never changes the run's outcome. runErr is the run's own
// failure, whose kind can exclude the retro.
func (s session) retroEpilogue(
	ctx context.Context, j job, repo workflow.Repo, number int, role workflow.Role, sessionID, key string, runErr error,
	budget config.Budget,
) opencode.Usage {
	if !opencode.RetroAllowed(runErr) {
		s.d.log().Info("retro skipped for this failure", "repo", repo, "issue", number, "role", role,
			"error", runErr)

		return opencode.Usage{}
	}

	if sessionID == "" {
		if runErr != nil {
			s.d.log().Warn("retro failed", "repo", repo, "issue", number, "role", role,
				"error", errors.New("the run reported no session to resume"))
		}

		return opencode.Usage{}
	}

	if key == "" {
		s.d.log().Warn("retro failed", "repo", repo, "issue", number, "role", role,
			"error", errors.New("the run has no commit to key its retro by"))

		return opencode.Usage{}
	}

	usage, err := s.retro(ctx, j.c, repo, number, role, sessionID, key, budget)
	if err != nil {
		s.d.log().Warn("retro failed", "repo", repo, "issue", number, "role", role, "error", err)
	}

	return usage
}

// retro resumes the finished session, by its explicit id, for its retro. It writes retro.json, parses
// it strictly, and posts the whole retro on the issue it worked, keyed by issue, agent role and run
// key.
func (s session) retro(
	ctx context.Context, c github.Client, repo workflow.Repo, number int, role workflow.Role, sessionID, key string,
	budget config.Budget,
) (opencode.Usage, error) {
	path := retroPath(s.d.Settings.WorkRoot, number, role)

	out, err := freshPath(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return opencode.Usage{}, err
	}

	// A retro picks its group key from the target's groups across the whole repository, so a group two
	// issues report is one the dashboard can count. The log read here is the same factory-authored one
	// the dashboard draws from.
	issues, err := c.ListIssues(ctx, repo, github.StateAll, github.Newest)
	if err != nil {
		return opencode.Usage{}, fmt.Errorf("list the repository's issues: %w", err)
	}

	reports, _, err := retroLog(ctx, s.d, c, repo, issues)
	if err != nil {
		return opencode.Usage{}, fmt.Errorf("read the retro log: %w", err)
	}

	groups := workflow.RetroGroups(reports, role, key)

	res, err := s.agent(ctx, opencode.Run{
		Role: role,
		Prompt: prompt.Retro(prompt.RetroRequest{
			Role: role, Repo: repo.String(), Issue: number, OutPath: out, Groups: groups,
		}),
		Session: sessionID, Timeout: budget.Deadline(), Steps: budget.Steps,
	})
	if err != nil {
		return res.Usage, err
	}

	raw, err := readScratch(out)
	if err != nil {
		return res.Usage, err
	}

	findings, err := workflow.ParseRetro(string(raw))
	if err != nil {
		return res.Usage, err
	}

	marker := workflow.RetroMarker(number, role, key)

	body, err := workflow.RenderRetro(marker, findings)
	if err != nil {
		return res.Usage, err
	}

	return res.Usage, c.UpsertComment(ctx, workflow.IssueRef{Repo: repo, Number: number}, marker, body)
}
