package foreman

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// applyPlannerRound posts one round, stores a final round's proposal hidden in its comment, and moves
// the issue to waiting. It returns the comment's id and whether the round was final.
func applyPlannerRound(
	ctx context.Context, j job, runID string, repo workflow.Repo, number int, out workflow.PlannerOutput,
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

	id, err := j.c.Finish(ctx, workflow.IssueRef{Repo: repo, Number: number}, t, text)

	return id, final, err
}

// approvePending applies the proposal stored in the latest final round when the last writer's reply
// approves it. It returns false when the latest round is not a stored final round or the reply is not
// approval, so the ordinary wake rule runs instead.
func approvePending(
	ctx context.Context, d Deps, perms *permissions, c github.Client, repo workflow.Repo, issue workflow.Issue,
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
	ctx context.Context, d Deps, perms *permissions, c github.Client, repo workflow.Repo,
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
	ctx context.Context, d Deps, c github.Client, repo workflow.Repo, from int, specComment int64,
	p workflow.PlannerProposal,
) error {
	switch p.Shape {
	case workflow.ShapeSingle:
		return applySingle(ctx, c, repo, from, d.runID())
	case workflow.ShapeSlices:
		return applySlices(ctx, c, repo, from, d.runID(), specComment, p)
	case workflow.ShapeMap:
		return applyMap(ctx, c, repo, from, d.runID(), p)
	case workflow.ShapeResolution:
		return applyResolution(ctx, c, repo, from, d.runID(), p)
	default:
		return fmt.Errorf("unknown proposal shape %q", p.Shape)
	}
}

// closedState is how GitHub answers an issue that is closed.
const closedState = "closed"

// applySingle hands the issue itself to the Implementer. The spec stays in the final-round comment.
func applySingle(ctx context.Context, c github.Client, repo workflow.Repo, from int, runID string) error {
	comments, err := c.ListComments(ctx, workflow.IssueRef{Repo: repo, Number: from})
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
	_, err = c.Finish(ctx, workflow.IssueRef{Repo: repo, Number: from}, t, text)

	return err
}

// applySlices creates one sub-issue per slice in factory:implement, links the spec, adds the blocking
// edges, closes the original with a slice list, and keeps its slug map for a repeat to reuse.
func applySlices(
	ctx context.Context, c github.Client, repo workflow.Repo, from int, runID string, specComment int64,
	p workflow.PlannerProposal,
) error {
	existing, err := planTickets(ctx, c, repo, from, p.Issues,
		func(planned workflow.PlannedIssue) string { return sliceBody(repo, from, specComment, planned) },
		implementLabels)
	if err != nil {
		return err
	}

	refs := make([]workflow.SliceRef, 0, len(p.Issues))

	for _, planned := range p.Issues {
		issue := existing[planned.Key]
		refs = append(refs, workflow.SliceRef{Key: planned.Key, Title: planned.Title, Number: issue.Number})
	}

	if err := linkTickets(ctx, c, repo, from, existing, p.Issues); err != nil {
		return err
	}

	return finishSlices(ctx, c, repo, from, runID, refs)
}

// applyMap rewrites an approved map's issue in the wayfinder layout, routes its tickets under it and
// moves the issue to factory:done. A repeat reuses every issue, link and comment it made.
func applyMap(
	ctx context.Context, c github.Client, repo workflow.Repo, from int, runID string, p workflow.PlannerProposal,
) error {
	if p.Map == nil {
		return errors.New("a map proposal carries no map")
	}

	issue, err := c.GetIssue(ctx, workflow.IssueRef{Repo: repo, Number: from})
	if err != nil {
		return err
	}

	if err := ensureTicketLabels(ctx, c, repo, p.Issues); err != nil {
		return err
	}

	existing, err := planTickets(ctx, c, repo, from, p.Issues, ticketBodyFor(from), ticketLabels)
	if err != nil {
		return err
	}

	if err := linkTickets(ctx, c, repo, from, existing, p.Issues); err != nil {
		return err
	}

	if err := rewriteMapBody(ctx, c, repo, issue, *p.Map); err != nil {
		return err
	}

	if err := c.AddLabel(ctx, workflow.IssueRef{Repo: repo, Number: from}, workflow.LabelMap); err != nil {
		return err
	}

	return finishMap(ctx, c, repo, from, runID)
}

// rewriteMapBody lays the map out over the writer's text, once. It also creates the wayfinder:map
// label the issue is about to carry.
func rewriteMapBody(
	ctx context.Context, c github.Client, repo workflow.Repo, issue workflow.Issue, m workflow.PlannerMap,
) error {
	if !isMapBody(issue) {
		ref := workflow.IssueRef{Repo: repo, Number: issue.Number}

		if err := c.UpdateIssueBody(ctx, ref, workflow.RenderMap(issue.Body, m)); err != nil {
			return fmt.Errorf("rewrite #%d as a map: %w", issue.Number, err)
		}
	}

	return c.EnsureLabels(ctx, repo, []string{workflow.LabelMap})
}

// finishMap moves the map to factory:done once, keyed by its applied marker.
func finishMap(ctx context.Context, c github.Client, repo workflow.Repo, from int, runID string) error {
	comments, err := c.ListComments(ctx, workflow.IssueRef{Repo: repo, Number: from})
	if err != nil {
		return err
	}

	if hasMarker(comments, workflow.AppliedMarker(from)) {
		return nil
	}

	t := workflow.Transition{
		Role: workflow.Planner, RunID: runID, From: workflow.Waiting, To: workflow.Done,
	}

	text := workflow.AppliedMarker(from) +
		"\n\nThe map is approved. Its tickets are sub-issues of this map."

	_, err = c.Finish(ctx, workflow.IssueRef{Repo: repo, Number: from}, t, text)

	return err
}

// isMapBody reports whether the issue already carries the map layout, so an apply never wraps the
// writer's text in a second map.
func isMapBody(issue workflow.Issue) bool {
	return workflow.HasLabel(issue, workflow.LabelMap) || strings.Contains(issue.Body, "## Original request")
}

// applyResolution records an approved decision ticket: its tickets go under the ticket's map, its
// decision line joins the map's Decisions so far, and the ticket is closed. Every step is keyed, so
// applying the same resolution again changes nothing.
func applyResolution(
	ctx context.Context, c github.Client, repo workflow.Repo, from int, runID string, p workflow.PlannerProposal,
) error {
	if p.Resolution == nil {
		return errors.New("a resolution proposal carries no resolution")
	}

	issue, err := c.GetIssue(ctx, workflow.IssueRef{Repo: repo, Number: from})
	if err != nil {
		return err
	}

	parent, err := parentMap(ctx, c, repo, issue)
	if err != nil {
		return err
	}

	if err := ensureTicketLabels(ctx, c, repo, p.Issues); err != nil {
		return err
	}

	existing, err := planTickets(ctx, c, repo, from, p.Issues, ticketBodyFor(from), ticketLabels)
	if err != nil {
		return err
	}

	if err := linkTickets(ctx, c, repo, parent.Number, existing, p.Issues); err != nil {
		return err
	}

	if err := appendDecision(ctx, c, repo, parent, issue, p.Resolution.DecisionLine); err != nil {
		return err
	}

	if err := postResolution(ctx, c, repo, from, p.Resolution.Answer); err != nil {
		return err
	}

	if issue.State == closedState {
		return nil
	}

	return c.CloseResolved(ctx, workflow.IssueRef{Repo: repo, Number: from}, workflow.Transition{
		Role: workflow.Planner, RunID: runID, From: workflow.Waiting, To: workflow.Closed,
	})
}

// appendDecision adds the ticket's entry to its map's Decisions so far, writing only when the line
// is new.
func appendDecision(
	ctx context.Context, c github.Client, repo workflow.Repo, parent, ticket workflow.Issue, line string,
) error {
	ref := workflow.IssueRef{Repo: repo, Number: ticket.Number}

	entry := workflow.DecisionEntry(ticket.Title, workflow.IssueURL(ref), line)

	body, err := workflow.AppendDecision(parent.Body, entry)
	if err != nil {
		return fmt.Errorf("append the decision to #%d: %w", parent.Number, err)
	}

	if body == parent.Body {
		return nil
	}

	return c.UpdateIssueBody(ctx, workflow.IssueRef{Repo: repo, Number: parent.Number}, body)
}

// postResolution posts the resolution comment once, keyed to the ticket it resolves.
func postResolution(ctx context.Context, c github.Client, repo workflow.Repo, from int, answer string) error {
	comments, err := c.ListComments(ctx, workflow.IssueRef{Repo: repo, Number: from})
	if err != nil {
		return err
	}

	marker := workflow.AppliedMarker(from)
	if hasMarker(comments, marker) {
		return nil
	}

	return c.Comment(ctx, workflow.IssueRef{Repo: repo, Number: from}, marker+"\n\n"+answer)
}

// ticketBody is a wayfinder ticket's body: its idempotency marker, then the ticket the Planner wrote.
func ticketBody(from int, planned workflow.PlannedIssue) string {
	return workflow.PlannedMarker(from, planned.Key) + "\n\n" + planned.Body
}

// ticketBodyFor is ticketBody for one map or resolution, ready for planTickets.
func ticketBodyFor(from int) func(workflow.PlannedIssue) string {
	return func(planned workflow.PlannedIssue) string { return ticketBody(from, planned) }
}

// ticketLabels derives a wayfinder ticket's labels from its type. The model never picks them.
func ticketLabels(planned workflow.PlannedIssue) ([]string, error) {
	return workflow.TicketLabels(planned.Type)
}

// implementLabels queues a slice for the Implementer.
func implementLabels(workflow.PlannedIssue) ([]string, error) {
	return []string{workflow.LabelImplement}, nil
}

// ensureTicketLabels creates every label the proposal's tickets carry the first time it sees them.
func ensureTicketLabels(
	ctx context.Context, c github.Client, repo workflow.Repo, issues []workflow.PlannedIssue,
) error {
	var labels []string

	for _, planned := range issues {
		derived, err := ticketLabels(planned)
		if err != nil {
			return err
		}

		for _, label := range derived {
			if !slices.Contains(labels, label) {
				labels = append(labels, label)
			}
		}
	}

	if len(labels) == 0 {
		return nil
	}

	return c.EnsureLabels(ctx, repo, labels)
}

// planTickets creates each proposed issue that is not planned yet, returning every one keyed by its
// slug. body and labels are the shape's rules for an issue.
func planTickets(
	ctx context.Context, c github.Client, repo workflow.Repo, from int, issues []workflow.PlannedIssue,
	body func(workflow.PlannedIssue) string, labels func(workflow.PlannedIssue) ([]string, error),
) (map[string]workflow.Issue, error) {
	existing, err := plannedIssues(ctx, c, repo, from)
	if err != nil {
		return nil, err
	}

	for _, planned := range issues {
		if _, ok := existing[planned.Key]; ok {
			continue
		}

		issueLabels, err := labels(planned)
		if err != nil {
			return nil, err
		}

		issue, err := c.CreateIssue(ctx, repo, planned.Title, body(planned), issueLabels)
		if err != nil {
			return nil, fmt.Errorf("create issue %s: %w", planned.Key, err)
		}

		existing[planned.Key] = issue
	}

	return existing, nil
}

// linkTickets links every planned issue under parent and adds its blocking edges.
func linkTickets(
	ctx context.Context, c github.Client, repo workflow.Repo, parent int,
	planned map[string]workflow.Issue, issues []workflow.PlannedIssue,
) error {
	for _, p := range issues {
		if err := ensureSubIssue(ctx, c, repo, parent, planned[p.Key]); err != nil {
			return err
		}
	}

	for _, p := range issues {
		child := planned[p.Key]

		for _, blocker := range p.BlockedBy {
			number, err := blockerNumber(blocker, planned)
			if err != nil {
				return err
			}

			if err := ensureBlockedBy(ctx, c, repo, child.Number, number); err != nil {
				return err
			}
		}
	}

	return nil
}

// sliceBody is a slice's body: its idempotency marker, a link to the approved spec comment, then the
// slice the Planner wrote.
func sliceBody(repo workflow.Repo, from int, specComment int64, planned workflow.PlannedIssue) string {
	body := workflow.PlannedMarker(from, planned.Key)

	if specComment > 0 {
		body += fmt.Sprintf("\n\nSpec: https://github.com/%s/issues/%d#issuecomment-%d", repo, from, specComment)
	}

	return body + "\n\n" + planned.Body
}

// finishSlices posts the slice list, moves the original to done and closes it.
func finishSlices(
	ctx context.Context, c github.Client, repo workflow.Repo, from int, runID string, refs []workflow.SliceRef,
) error {
	comments, err := c.ListComments(ctx, workflow.IssueRef{Repo: repo, Number: from})
	if err != nil {
		return err
	}

	if !hasMarker(comments, workflow.SlicesMarker(from)) {
		t := workflow.Transition{
			Role: workflow.Planner, RunID: runID, From: workflow.Waiting, To: workflow.Done,
		}

		ref := workflow.IssueRef{Repo: repo, Number: from}

		if _, err := c.Finish(ctx, ref, t, workflow.RenderSlices(from, refs)); err != nil {
			return err
		}
	}

	return c.CloseIssue(ctx, workflow.IssueRef{Repo: repo, Number: from})
}

// plannedIssues finds the issues already created from from, by their idempotency markers.
func plannedIssues(
	ctx context.Context, c github.Client, repo workflow.Repo, from int,
) (map[string]workflow.Issue, error) {
	issues, err := c.ListIssues(ctx, repo, github.StateOpen, github.Oldest)
	if err != nil {
		return nil, err
	}

	out := make(map[string]workflow.Issue, len(issues))

	for _, issue := range issues {
		// Only a factory App's issue stands in for a planned slice; an outsider's marker is ignored.
		if !workflow.FactoryAuthored(issue.Author) {
			continue
		}

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
func ensureSubIssue(ctx context.Context, c github.Client, repo workflow.Repo, parent int, child workflow.Issue) error {
	numbers, supported, err := c.SubIssueNumbers(ctx, workflow.IssueRef{Repo: repo, Number: parent})
	if err != nil {
		return err
	}

	if !supported {
		return ensureBodyLine(ctx, c, workflow.IssueRef{Repo: repo, Number: child.Number}, fmt.Sprintf("Part of #%d", parent))
	}

	if slices.Contains(numbers, child.Number) {
		return nil
	}

	id, err := issueID(ctx, c, repo, child)
	if err != nil {
		return err
	}

	return c.AddSubIssue(ctx, workflow.IssueRef{Repo: repo, Number: parent}, id)
}

// ensureBlockedBy blocks child on blocker. A repository without dependencies gets a "Blocked by:"
// line at the top of the child's body instead.
func ensureBlockedBy(ctx context.Context, c github.Client, repo workflow.Repo, child, blocker int) error {
	numbers, supported, err := c.BlockedByNumbers(ctx, workflow.IssueRef{Repo: repo, Number: child})
	if err != nil {
		return err
	}

	if !supported {
		return ensureBodyLine(ctx, c, workflow.IssueRef{Repo: repo, Number: child}, fmt.Sprintf("Blocked by: #%d", blocker))
	}

	if slices.Contains(numbers, blocker) {
		return nil
	}

	id, err := c.IssueID(ctx, workflow.IssueRef{Repo: repo, Number: blocker})
	if err != nil {
		return err
	}

	return c.AddBlockedBy(ctx, workflow.IssueRef{Repo: repo, Number: child}, id)
}

// issueID reads an issue's database id, reading the issue again when the list did not carry one.
func issueID(ctx context.Context, c github.Client, repo workflow.Repo, issue workflow.Issue) (int64, error) {
	if issue.ID != 0 {
		return issue.ID, nil
	}

	return c.IssueID(ctx, workflow.IssueRef{Repo: repo, Number: issue.Number})
}

// ensureBodyLine puts line at the top of an issue's body unless it is already there.
func ensureBodyLine(ctx context.Context, c github.Client, ref workflow.IssueRef, line string) error {
	issue, err := c.GetIssue(ctx, ref)
	if err != nil {
		return err
	}

	if strings.Contains(issue.Body, line) {
		return nil
	}

	return c.UpdateIssueBody(ctx, ref, line+"\n\n"+issue.Body)
}

// hasMarker reports whether a factory App's comment starts with marker. Another author's marker is
// ignored, so an outsider can neither block an apply nor stand in for one.
func hasMarker(comments []workflow.Item, marker string) bool {
	for _, comment := range comments {
		if workflow.FactoryAuthored(comment.Author) && strings.Contains(comment.Body, marker) {
			return true
		}
	}

	return false
}
