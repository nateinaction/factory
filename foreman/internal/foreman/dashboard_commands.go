package foreman

import (
	"context"
	"fmt"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// commandRun is one poll's pass over the writer commands on a repository's dashboard and on the issues
// its retro groups were promoted as.
type commandRun struct {
	d     Deps
	cfg   config.Config
	c     github.Client
	perms *permissions
	repo  workflow.Repo
	// role is the App that polls here: it files a promoted issue in another repository too.
	role workflow.Role
	// issues is every issue in the repository, newest first.
	issues []workflow.Issue
	// threads are the promoted issues' comments, by issue number.
	threads map[int]promotedThread
	// reports is the retro log the promotions are drawn from.
	reports []workflow.DashboardReport
	// private is the repository's visibility, read once when a command needs it.
	private *bool
}

// run applies every command on the dashboard and on the promoted issues, and returns the state to draw
// and the comments to settle. Only a writer's command has an effect; a non-writer's is settled with none.
func (r *commandRun) run(
	ctx context.Context, state workflow.DashboardState, dashboard int, found bool,
) (workflow.DashboardState, []workflow.Item) {
	if !found {
		return state, nil
	}

	comments, err := r.c.ListComments(ctx, workflow.IssueRef{Repo: r.repo, Number: dashboard})
	if err != nil {
		r.d.log().Warn("cannot read the dashboard's comments", "repo", r.repo, "issue", dashboard, "error", err)

		return state, nil
	}

	state, settle := r.onComments(ctx, state, nil, comments)
	published := r.publish(ctx, state)

	return published.state, append(settle, published.settle...)
}

// applied is what a pass over commands left behind.
type applied struct {
	// state is the dashboard state after the commands.
	state workflow.DashboardState
	// settle are the comments to minimize or delete, oldest first.
	settle []workflow.Item
}

// onComments applies the commands in one thread. promoted is the group the thread's issue was promoted
// as, or nil for the dashboard.
func (r *commandRun) onComments(
	ctx context.Context, state workflow.DashboardState, promoted *workflow.GroupID, comments []workflow.Item,
) (workflow.DashboardState, []workflow.Item) {
	var settle []workflow.Item

	for _, comment := range comments {
		if state.HasHandled(comment.ID) {
			continue
		}

		next, done := r.onComment(ctx, state, promoted, comment)
		if !done {
			continue
		}

		state = next

		settle = append(settle, comment)
	}

	return state, settle
}

// onComment applies one comment. done is true when the comment was dealt with, so it is recorded as
// handled and then minimized or deleted; a comment no one acted on is left where it is.
func (r *commandRun) onComment(
	ctx context.Context, state workflow.DashboardState, promoted *workflow.GroupID, comment workflow.Item,
) (workflow.DashboardState, bool) {
	cmd, ok, err := workflow.ParseCommand(comment.Body)
	if err != nil {
		r.d.log().Warn("cannot read a dashboard command", "repo", r.repo, "comment", comment.ID, "error", err)

		return state, false
	}

	if !ok {
		return state, false
	}

	if !r.perms.isWriter(ctx, r.d, r.c, r.repo, comment.Author) {
		r.d.log().Info("a non-writer's dashboard command is minimized and takes no action",
			"repo", r.repo, "comment", comment.ID, "by", comment.Author, "command", cmd.Kind)

		next, ok := state.WithHandled(comment.ID)
		if !ok {
			r.d.log().Warn("a command comment could not be recorded as handled",
				"repo", r.repo, "comment", comment.ID)

			return state, false
		}

		return next, true
	}

	next, applied := r.apply(ctx, state, promoted, comment, cmd)
	if !applied {
		return state, false
	}

	next, recorded := next.WithHandled(comment.ID)
	if !recorded {
		// Not recording the command would let the next poll apply it again, so it is not settled.
		r.d.log().Warn("a command comment could not be recorded as handled",
			"repo", r.repo, "comment", comment.ID)

		return state, false
	}

	return next, true
}

// apply carries out one writer's command. applied is false when the command could not be carried out, so
// it is retried on the next poll rather than silently dropped.
func (r *commandRun) apply(
	ctx context.Context, state workflow.DashboardState, promoted *workflow.GroupID,
	comment workflow.Item, cmd workflow.Command,
) (workflow.DashboardState, bool) {
	switch cmd.Kind {
	case workflow.CommandPromote:
		return r.promote(ctx, state, comment, cmd.Groups[0])
	case workflow.CommandMerge:
		return r.merge(state, comment, cmd.Groups)
	case workflow.CommandIgnore:
		return state.WithAction(r.action(workflow.ActionIgnore, comment, cmd.Groups)), true
	case workflow.CommandReject:
		return r.reject(state, comment, cmd.Proposal)
	case workflow.CommandPublish:
		return r.publishOne(ctx, state, promoted, comment)
	case workflow.CommandCurate:
		return r.curate(state, comment, promoted)
	}

	return state, false
}

// action records one writer action, linked to the command comment.
func (r *commandRun) action(
	kind workflow.DashboardActionKind, comment workflow.Item, groups []workflow.GroupID,
) workflow.DashboardAction {
	return workflow.DashboardAction{
		Kind: kind, Groups: groups, By: comment.Author, At: r.d.now(), URL: comment.HTMLURL,
	}
}

// merge records a merge, dropping the proposals it makes stale. Merging a group with itself changes
// nothing, so it is treated as a command foreman could not carry out; so is a merge across targets,
// because reports of different targets are never one group.
func (r *commandRun) merge(
	state workflow.DashboardState, comment workflow.Item, groups []workflow.GroupID,
) (workflow.DashboardState, bool) {
	if groups[0] == groups[1] {
		r.d.log().Warn("a merge names one group twice", "repo", r.repo, "group", groups[0])

		return state, false
	}

	if groups[0].Target != groups[1].Target {
		r.d.log().Warn("a merge names groups of different targets; only a link may cross targets",
			"repo", r.repo, "groups", groups)

		return state, false
	}

	state = state.WithAction(r.action(workflow.ActionMerge, comment, groups))

	return state.DropProposals(groups), true
}

// reject refuses a Curator proposal for good, so the pair is never shown again.
func (r *commandRun) reject(
	state workflow.DashboardState, comment workflow.Item, id string,
) (workflow.DashboardState, bool) {
	proposal, ok := state.Proposal(id)
	if !ok {
		r.d.log().Warn("a rejected proposal is not on the dashboard", "repo", r.repo, "proposal", id)

		return state, false
	}

	r.d.log().Info("a writer rejected a Curator proposal", "repo", r.repo, "proposal", id)

	return state.WithRejection(workflow.DashboardRejection{
		ID: proposal.ID, Groups: proposal.Groups, By: comment.Author, At: r.d.now(), URL: comment.HTMLURL,
	}), true
}

// curate records a writer's request for a Curator run. The poll starts the run; a request on a
// promoted issue is not a dashboard command, so it is settled with no action.
func (r *commandRun) curate(
	state workflow.DashboardState, comment workflow.Item, promoted *workflow.GroupID,
) (workflow.DashboardState, bool) {
	if promoted != nil {
		r.d.log().Info("curate belongs on the dashboard, not a promoted issue", "repo", r.repo)

		return state, true
	}

	r.d.log().Info("a writer asked for a Curator run", "repo", r.repo, "by", comment.Author)

	return state.WithAction(r.action(workflow.ActionCurate, comment, nil)), true
}

// promote files a ready group's issue where the group belongs, once per group key.
func (r *commandRun) promote(
	ctx context.Context, state workflow.DashboardState, comment workflow.Item, id workflow.GroupID,
) (workflow.DashboardState, bool) {
	group, ok := workflow.FindGroup(r.reports, state, id)
	if !ok {
		r.d.log().Warn("a promoted group has no reports", "repo", r.repo, "group", id)

		return state, false
	}

	private, err := r.isPrivate(ctx)
	if err != nil {
		r.d.log().Warn("cannot read the repository's visibility; not promoting", "repo", r.repo, "error", err)

		return state, false
	}

	dest, err := promotionRepo(group.ID, r.repo, private, r.cfg.FactoryRepo)
	if err != nil {
		r.d.log().Warn("cannot file a promoted group", "repo", r.repo, "group", group.ID, "error", err)

		return state, false
	}

	issue, err := r.fileGroup(ctx, dest, group, private)
	if err != nil {
		r.d.log().Warn("cannot file a promoted group", "repo", r.repo, "group", group.ID, "dest", dest, "error", err)

		return state, false
	}

	r.d.log().Info("promoted a retro group", "repo", r.repo, "group", group.ID, "dest", dest, "issue", issue.Number)

	return state.WithAction(r.action(workflow.ActionPromote, comment, []workflow.GroupID{group.ID})), true
}

// fileGroup files a group's issue in dest, reusing the one an earlier promotion filed.
func (r *commandRun) fileGroup(
	ctx context.Context, dest workflow.Repo, group workflow.DashboardGroup, private bool,
) (workflow.Issue, error) {
	c, err := r.clientFor(ctx, dest)
	if err != nil {
		return workflow.Issue{}, err
	}

	existing, ok, err := listedPromoted(ctx, c, dest, group.ID)
	if err != nil || ok {
		return existing, err
	}

	title, body, err := workflow.RenderPromotion(group.ID, group.Reports, private)
	if err != nil {
		return workflow.Issue{}, err
	}

	return c.CreateIssue(ctx, dest, title, body, nil)
}

// publish applies the publish commands on the repository's promoted issues.
func (r *commandRun) publish(ctx context.Context, state workflow.DashboardState) applied {
	out := applied{state: state}

	for _, issue := range r.issues {
		thread, ok := r.threads[issue.Number]
		if !ok {
			continue
		}

		group := thread.group
		state, settle := r.onComments(ctx, out.state, &group, thread.comments)
		out.state, out.settle = state, append(out.settle, settle...)
	}

	return out
}

// publishOne copies a promoted private repository's lesson to the factory repo. A publish anywhere else
// takes no action.
func (r *commandRun) publishOne(
	ctx context.Context, state workflow.DashboardState, promoted *workflow.GroupID, comment workflow.Item,
) (workflow.DashboardState, bool) {
	if promoted == nil {
		r.d.log().Info("publish belongs on a promoted issue, not the dashboard", "repo", r.repo)

		return state, true
	}

	private, err := r.isPrivate(ctx)
	if err != nil {
		r.d.log().Warn("cannot read the repository's visibility; not publishing", "repo", r.repo, "error", err)

		return state, false
	}

	if !private {
		r.d.log().Info("publish only copies a private repository's lesson out", "repo", r.repo, "group", *promoted)

		return state, true
	}

	issue, ok := r.promotedIssue(*promoted)
	if !ok {
		r.d.log().Warn("no issue carries a promoted group's lesson", "repo", r.repo, "group", *promoted)

		return state, false
	}

	lesson, ok := workflow.LessonOf(issue.Body)
	if !ok {
		r.d.log().Warn("a promoted issue has no lesson to publish", "repo", r.repo, "group", *promoted)

		return state, false
	}

	published, err := r.fileLesson(ctx, *promoted, lesson, comment)
	if err != nil {
		r.d.log().Warn("cannot publish a lesson", "repo", r.repo, "group", *promoted, "error", err)

		return state, false
	}

	r.d.log().Info("published a private lesson", "repo", r.repo, "group", *promoted, "issue", published.Number)

	return state.WithPublication(workflow.DashboardPublication{
		Group: *promoted, By: comment.Author, At: r.d.now(), URL: published.HTMLURL,
	}), true
}

// promotedIssue finds the issue a group was promoted as in this repository.
func (r *commandRun) promotedIssue(group workflow.GroupID) (workflow.Issue, bool) {
	return findMarked(r.issues, group, workflow.ParsePromoted)
}

// fileLesson files a private repository's lesson in the factory repo, reusing the issue an earlier
// publish filed.
func (r *commandRun) fileLesson(
	ctx context.Context, group workflow.GroupID, lesson string, comment workflow.Item,
) (workflow.Issue, error) {
	if r.cfg.FactoryRepo == "" {
		return workflow.Issue{}, fmt.Errorf("no factory repository is configured")
	}

	factory, err := workflow.ParseRepo(r.cfg.FactoryRepo)
	if err != nil {
		return workflow.Issue{}, err
	}

	c, err := r.clientFor(ctx, factory)
	if err != nil {
		return workflow.Issue{}, err
	}

	if existing, ok, err := listedPublished(ctx, c, factory, group); err != nil || ok {
		return existing, err
	}

	title, body, err := workflow.RenderPublished(group, lesson, comment.Author)
	if err != nil {
		return workflow.Issue{}, err
	}

	return c.CreateIssue(ctx, factory, title, body, nil)
}

// clientFor returns a client for repo: this poll's client where the command stays in the repository, and
// one minted for the polling App where the promotion leaves it.
func (r *commandRun) clientFor(ctx context.Context, repo workflow.Repo) (github.Client, error) {
	if repo == r.repo {
		return r.c, nil
	}

	token, err := r.d.mintToken(ctx, r.role, repo)
	if err != nil {
		return github.Client{}, fmt.Errorf("mint a token for %s: %w", repo, err)
	}

	return r.d.client(token), nil
}

// isPrivate reads the repository's visibility once.
func (r *commandRun) isPrivate(ctx context.Context) (bool, error) {
	if r.private != nil {
		return *r.private, nil
	}

	repo, err := r.c.Repository(ctx, r.repo)
	if err != nil {
		return false, fmt.Errorf("read the repository: %w", err)
	}

	r.private = &repo.Private

	return repo.Private, nil
}

// listedPromoted finds the issue a group was promoted as, in another repository.
func listedPromoted(
	ctx context.Context, c github.Client, repo workflow.Repo, group workflow.GroupID,
) (workflow.Issue, bool, error) {
	issues, err := c.ListIssues(ctx, repo, github.StateAll)
	if err != nil {
		return workflow.Issue{}, false, fmt.Errorf("list the issues of %s: %w", repo, err)
	}

	issue, ok := findMarked(issues, group, workflow.ParsePromoted)

	return issue, ok, nil
}

// listedPublished finds the issue a group's lesson was published as, in another repository.
func listedPublished(
	ctx context.Context, c github.Client, repo workflow.Repo, group workflow.GroupID,
) (workflow.Issue, bool, error) {
	issues, err := c.ListIssues(ctx, repo, github.StateAll)
	if err != nil {
		return workflow.Issue{}, false, fmt.Errorf("list the issues of %s: %w", repo, err)
	}

	issue, ok := findMarked(issues, group, workflow.ParsePublished)

	return issue, ok, nil
}

// findMarked finds the issue whose body carries the marker parse reads for group. A body whose marker is
// unreadable belongs to no group, so it is skipped.
func findMarked(
	issues []workflow.Issue, group workflow.GroupID, parse func(string) (workflow.GroupID, bool, error),
) (workflow.Issue, bool) {
	for _, issue := range issues {
		found, ok, err := parse(issue.Body)
		if err != nil || !ok {
			continue
		}

		if found == group {
			return issue, true
		}
	}

	return workflow.Issue{}, false
}

// settleCommand minimizes a dealt-with command as resolved, and deletes it when the App may not minimize
// it. Whether the App may is read live; a check that cannot be read falls back to deleting too.
func settleCommand(ctx context.Context, d Deps, c github.Client, repo workflow.Repo, comment workflow.Item) error {
	if comment.NodeID != "" && minimizeComment(ctx, d, c, comment) {
		return nil
	}

	return c.DeleteComment(ctx, repo, comment.ID)
}

// minimizeComment minimizes one comment when the App may, reporting whether it managed to.
func minimizeComment(ctx context.Context, d Deps, c github.Client, comment workflow.Item) bool {
	can, err := c.CanMinimizeComment(ctx, comment.NodeID)
	if err != nil {
		d.log().Warn("cannot read whether a command comment can be minimized; deleting it instead",
			"comment", comment.ID, "error", err)

		return false
	}

	if !can {
		return false
	}

	if err := c.MinimizeComment(ctx, comment.NodeID); err != nil {
		d.log().Warn("cannot minimize a command comment; deleting it instead",
			"comment", comment.ID, "error", err)

		return false
	}

	return true
}
