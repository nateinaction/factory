package foreman

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/prompt"
	"github.com/nateinaction/factory/foreman/internal/sandbox"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// runPlanner works the issue in a fresh sandbox directory and logs the run's one usage line.
func runPlanner(
	ctx context.Context, d Deps, repo string, agent config.AgentConfig, retro config.Budget, number int,
) (err error) {
	var (
		usage   opencode.Usage
		domains []string
	)

	files, err := newSandboxFiles(d.Settings.WorkRoot, workflow.Planner, number)
	if err != nil {
		return err
	}

	defer func() { err = errors.Join(err, files.remove()) }()

	usage, domains, err = plan(ctx, d, repo, agent, retro, number, files.db(workflow.Planner), files)
	d.logUsage(opencode.NewUsageReport(workflow.Planner, repo, number, d.Host, agent, usage).
		WithDomains(domains))

	return err
}

// plan reads the Planner's secrets and runs its work behind the run's proxies.
func plan(
	ctx context.Context, d Deps, repo string, agent config.AgentConfig, retro config.Budget, number int,
	db string, files sandboxFiles,
) (opencode.Usage, []string, error) {
	network, err := sandbox.ParseNetwork(agent.Egress.AllowedDomains)
	if err != nil {
		return opencode.Usage{}, nil, err
	}

	sec, err := readSecrets(d.Settings, workflow.Planner)
	if err != nil {
		return opencode.Usage{}, nil, err
	}

	return d.withProxy(ctx, network, sec.openrouterKey, func(baseURL, proxyAddr string) (opencode.Usage, error) {
		return planRun(ctx, d, sec, baseURL, proxyAddr, network, repo, agent, retro, number, db, files)
	})
}

// planRun is the Planner's work, with its proxies already up. It reads the issue thread, runs the
// model under the sandbox and applies one round. The retro is the epilogue of the final-round run
// only, keyed by the id of the comment that stored the proposal.
func planRun(
	ctx context.Context, d Deps, sec secrets, baseURL, proxyAddr string, network sandbox.Network,
	repo string, agent config.AgentConfig, retro config.Budget, number int, db string, files sandboxFiles,
) (opencode.Usage, error) {
	j, err := loadJob(ctx, d, sec, baseURL, repo, workflow.Planner, agent, number, db)
	if err != nil {
		return opencode.Usage{}, err
	}

	scratch, outPath, err := plannerPaths(d.Settings.WorkRoot, number)
	if err != nil {
		return opencode.Usage{}, err
	}

	comments, err := j.c.ListComments(ctx, repo, number)
	if err != nil {
		return opencode.Usage{}, err
	}

	s := newSession(d, j, scratch, nil)

	if s, err = d.sandboxedWithToolchain(
		ctx, s, j, sec, repo, "", files, workflow.Planner, "", outPath,
		[]string{retroPath(d.Settings.WorkRoot, number, workflow.Planner)}, network, proxyAddr,
	); err != nil {
		return opencode.Usage{}, err
	}

	res, err := s.agent(ctx, opencode.Run{
		Role: workflow.Planner, Prompt: prompt.Planner(nil, repo, j.issue, comments, outPath),
		Timeout: agent.Deadline(), Steps: agent.Steps,
	})
	if err != nil {
		return res.Usage, err
	}

	out, err := readPlannerOutput(outPath)
	if err != nil {
		return res.Usage, rejectOutput(ctx, j.c, repo, number, workflow.Planner, err)
	}

	commentID, final, err := applyPlannerRound(ctx, j, d.runID(), repo, number, out)
	if err != nil {
		return res.Usage, err
	}

	if !final {
		return res.Usage, nil
	}

	return res.Usage.Add(s.retroEpilogue(
		ctx, j, repo, number, workflow.Planner, res.Session, fmt.Sprint(commentID), nil, retro,
	)), nil
}

// plannerPaths clears and returns a Planner run's scratch directory and output path. The Planner has
// no clone: it reads the issue thread.
func plannerPaths(root string, number int) (string, string, error) {
	scratch, err := scratchDir(root, fmt.Sprintf("planner-%d", number))
	if err != nil {
		return "", "", err
	}

	return scratch, filepath.Join(scratch, workflow.PlannerOutputFile), nil
}

// readPlannerOutput reads and validates the Planner's output file.
func readPlannerOutput(path string) (workflow.PlannerOutput, error) {
	raw, err := readScratch(path)
	if err != nil {
		return workflow.PlannerOutput{}, outputProblem(workflow.PlannerOutputFile, err)
	}

	return workflow.ParsePlannerOutput(string(raw))
}

// applyPlannerRound posts one round, stores a final round's proposal hidden in its comment, and moves
// the issue to waiting. It returns the comment's id and whether the round was final.
func applyPlannerRound(
	ctx context.Context, j job, runID, repo string, number int, out workflow.PlannerOutput,
) (int64, bool, error) {
	text := out.Comment
	final := out.Kind == workflow.PlannerFinalRound

	if final {
		block, err := workflow.ProposalBlock(*out.Proposal)
		if err != nil {
			return 0, false, err
		}

		text += "\n\n" + block
	}

	t := workflow.Transition{
		Role: workflow.Planner, RunID: runID, From: workflow.Planning, To: workflow.Waiting,
	}

	id, err := j.c.Finish(ctx, repo, number, t, text)

	return id, final, err
}

// approvePending applies the proposal stored in the latest final round when the last writer's reply
// approves it. It returns false when the latest round is not a stored final round or the reply is not
// approval, so the ordinary wake rule runs instead.
func approvePending(
	ctx context.Context, d Deps, perms *permissions, c github.Client, repo string, issue workflow.Issue,
	thread []workflow.Item, latest workflow.Marker,
) (bool, error) {
	if latest.Role != workflow.Planner || latest.To != workflow.Waiting {
		return false, nil
	}

	proposal, found, err := workflow.StoredProposal(latest.Body)
	if err != nil || !found {
		return false, err
	}

	reply, ok := lastWriterComment(ctx, d, perms, c, repo, thread, latest)
	if !ok || !workflow.Approves(reply.Body) {
		return false, nil
	}

	token, err := d.mintToken(ctx, workflow.Planner, repo)
	if err != nil {
		d.log().Warn("cannot mint the Planner's token; leaving the approved proposal unapplied",
			"repo", repo, "issue", issue.Number, "error", err)

		return false, nil
	}

	d.log().Info("applying an approved proposal", "repo", repo, "issue", issue.Number,
		"shape", proposal.Shape)

	return true, applyProposal(ctx, d, d.client(token), repo, issue.Number, latest.ID, proposal)
}

// lastWriterComment is the latest comment by a writer after marker, which the human gate reads.
func lastWriterComment(
	ctx context.Context, d Deps, perms *permissions, c github.Client, repo string,
	thread []workflow.Item, marker workflow.Marker,
) (workflow.Item, bool) {
	var (
		last workflow.Item
		ok   bool
	)

	for _, comment := range workflow.AfterMarker(thread, marker) {
		if perms.isWriter(ctx, d, c, repo, comment.Author) {
			last, ok = comment, true
		}
	}

	return last, ok
}

// applyProposal makes an approved proposal true. Every step is idempotent: applying the same
// proposal again reuses the issues it created and adds only what is missing.
func applyProposal(
	ctx context.Context, d Deps, c github.Client, repo string, from int, specComment int64,
	p workflow.PlannerProposal,
) error {
	switch p.Shape {
	case workflow.ShapeSingle:
		return applySingle(ctx, c, repo, from, d.runID())
	case workflow.ShapeSlices:
		return applySlices(ctx, c, repo, from, d.runID(), specComment, p)
	case workflow.ShapeMap, workflow.ShapeResolution:
		return fmt.Errorf("proposal shape %s is not implemented yet", p.Shape)
	default:
		return fmt.Errorf("unknown proposal shape %q", p.Shape)
	}
}

// applySingle hands the issue itself to the Implementer. The spec stays in the final-round comment.
func applySingle(ctx context.Context, c github.Client, repo string, from int, runID string) error {
	comments, err := c.ListComments(ctx, repo, from)
	if err != nil {
		return err
	}

	if hasMarker(comments, workflow.AppliedMarker(from)) {
		return nil
	}

	text := workflow.AppliedMarker(from) +
		"\n\nThe proposal is approved. The spec is in the final-round comment above; queued for the Implementer."

	t := workflow.Transition{
		Role: workflow.Planner, RunID: runID, From: workflow.Waiting, To: workflow.Implement,
	}
	_, err = c.Finish(ctx, repo, from, t, text)

	return err
}

// applySlices creates one sub-issue per slice in factory:implement, links the spec, adds the blocking
// edges, closes the original with a slice list, and keeps its slug map for a repeat to reuse.
func applySlices(
	ctx context.Context, c github.Client, repo string, from int, runID string, specComment int64,
	p workflow.PlannerProposal,
) error {
	existing, err := plannedIssues(ctx, c, repo, from)
	if err != nil {
		return err
	}

	refs := make([]workflow.SliceRef, 0, len(p.Issues))

	for _, planned := range p.Issues {
		issue, ok := existing[planned.Key]
		if !ok {
			issue, err = c.CreateIssue(ctx, repo, planned.Title,
				sliceBody(repo, from, specComment, planned),
				[]string{workflow.LabelImplement})
			if err != nil {
				return fmt.Errorf("create slice %s: %w", planned.Key, err)
			}

			existing[planned.Key] = issue
		}

		refs = append(refs, workflow.SliceRef{Key: planned.Key, Title: planned.Title, Number: issue.Number})

		if err := ensureSubIssue(ctx, c, repo, from, issue); err != nil {
			return err
		}
	}

	for _, planned := range p.Issues {
		child := existing[planned.Key]

		for _, blocker := range planned.BlockedBy {
			number, err := blockerNumber(blocker, existing)
			if err != nil {
				return err
			}

			if err := ensureBlockedBy(ctx, c, repo, child.Number, number); err != nil {
				return err
			}
		}
	}

	return finishSlices(ctx, c, repo, from, runID, refs)
}

// sliceBody is a slice's body: its idempotency marker, a link to the approved spec comment, then the
// slice the Planner wrote.
func sliceBody(repo string, from int, specComment int64, planned workflow.PlannedIssue) string {
	body := workflow.PlannedMarker(from, planned.Key)

	if specComment > 0 {
		body += fmt.Sprintf("\n\nSpec: https://github.com/%s/issues/%d#issuecomment-%d", repo, from, specComment)
	}

	return body + "\n\n" + planned.Body
}

// finishSlices posts the slice list, moves the original to done and closes it.
func finishSlices(
	ctx context.Context, c github.Client, repo string, from int, runID string, refs []workflow.SliceRef,
) error {
	comments, err := c.ListComments(ctx, repo, from)
	if err != nil {
		return err
	}

	if !hasMarker(comments, workflow.SlicesMarker(from)) {
		t := workflow.Transition{
			Role: workflow.Planner, RunID: runID, From: workflow.Waiting, To: workflow.Done,
		}

		if _, err := c.Finish(ctx, repo, from, t, workflow.RenderSlices(from, refs)); err != nil {
			return err
		}
	}

	return c.CloseIssue(ctx, repo, from)
}

// plannedIssues finds the issues already created from from, by their idempotency markers.
func plannedIssues(ctx context.Context, c github.Client, repo string, from int) (map[string]workflow.Issue, error) {
	issues, err := c.ListOpenIssues(ctx, repo)
	if err != nil {
		return nil, err
	}

	out := make(map[string]workflow.Issue, len(issues))

	for _, issue := range issues {
		plannedFrom, key, found := workflow.PlannedFrom(issue.Body)
		if !found || plannedFrom != from {
			continue
		}

		if _, seen := out[key]; !seen {
			out[key] = issue
		}
	}

	return out, nil
}

// blockerNumber resolves a blocked_by entry: another planned key, or #N for an existing issue.
func blockerNumber(blocker string, planned map[string]workflow.Issue) (int, error) {
	if digits, ok := strings.CutPrefix(blocker, "#"); ok {
		return workflow.ParseIssueNumber(digits)
	}

	issue, ok := planned[blocker]
	if !ok {
		return 0, fmt.Errorf("blocked_by %q names no issue", blocker)
	}

	return issue.Number, nil
}

// ensureSubIssue links child as a sub-issue of parent. A repository without sub-issues gets a
// "Part of #N" line at the top of the child's body instead.
func ensureSubIssue(ctx context.Context, c github.Client, repo string, parent int, child workflow.Issue) error {
	numbers, supported, err := c.SubIssueNumbers(ctx, repo, parent)
	if err != nil {
		return err
	}

	if !supported {
		return ensureBodyLine(ctx, c, repo, child.Number, fmt.Sprintf("Part of #%d", parent))
	}

	if slices.Contains(numbers, child.Number) {
		return nil
	}

	id, err := issueID(ctx, c, repo, child)
	if err != nil {
		return err
	}

	return c.AddSubIssue(ctx, repo, parent, id)
}

// ensureBlockedBy blocks child on blocker. A repository without dependencies gets a "Blocked by:"
// line at the top of the child's body instead.
func ensureBlockedBy(ctx context.Context, c github.Client, repo string, child, blocker int) error {
	numbers, supported, err := c.BlockedByNumbers(ctx, repo, child)
	if err != nil {
		return err
	}

	if !supported {
		return ensureBodyLine(ctx, c, repo, child, fmt.Sprintf("Blocked by: #%d", blocker))
	}

	if slices.Contains(numbers, blocker) {
		return nil
	}

	id, err := c.IssueID(ctx, repo, blocker)
	if err != nil {
		return err
	}

	return c.AddBlockedBy(ctx, repo, child, id)
}

// issueID reads an issue's database id, reading the issue again when the list did not carry one.
func issueID(ctx context.Context, c github.Client, repo string, issue workflow.Issue) (int64, error) {
	if issue.ID != 0 {
		return issue.ID, nil
	}

	return c.IssueID(ctx, repo, issue.Number)
}

// ensureBodyLine puts line at the top of an issue's body unless it is already there.
func ensureBodyLine(ctx context.Context, c github.Client, repo string, number int, line string) error {
	issue, err := c.GetIssue(ctx, repo, number)
	if err != nil {
		return err
	}

	if strings.Contains(issue.Body, line) {
		return nil
	}

	return c.UpdateIssueBody(ctx, repo, number, line+"\n\n"+issue.Body)
}

// hasMarker reports whether a comment starts with marker.
func hasMarker(comments []workflow.Item, marker string) bool {
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) {
			return true
		}
	}

	return false
}
