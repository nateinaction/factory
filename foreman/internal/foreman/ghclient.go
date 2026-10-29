package foreman

import (
	"context"

	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// ghClient adapts github.Client, whose methods take a checked workflow.Repo or workflow.IssueRef, to
// the repository strings and issue numbers the rest of this package still passes. Each method parses
// the name first, so an unchecked name fails closed before any request is built. The adapter is
// temporary: callers move onto workflow.Repo and workflow.IssueRef and it goes away.
type ghClient struct {
	github.Client
}

// onRepo parses repo and runs f on it.
func onRepo[T any](repo string, f func(workflow.Repo) (T, error)) (T, error) {
	r, err := workflow.ParseRepo(repo)
	if err != nil {
		var zero T

		return zero, err
	}

	return f(r)
}

// onRef parses repo, pairs it with number and runs f on the reference.
func onRef[T any](repo string, number int, f func(workflow.IssueRef) (T, error)) (T, error) {
	return onRepo(repo, func(r workflow.Repo) (T, error) {
		return f(workflow.IssueRef{Repo: r, Number: number})
	})
}

// onRefErr is onRef for a call that returns only an error.
func onRefErr(repo string, number int, f func(workflow.IssueRef) error) error {
	_, err := onRef(repo, number, func(ref workflow.IssueRef) (struct{}, error) {
		return struct{}{}, f(ref)
	})

	return err
}

// onRepoErr is onRepo for a call that returns only an error.
func onRepoErr(repo string, f func(workflow.Repo) error) error {
	_, err := onRepo(repo, func(r workflow.Repo) (struct{}, error) {
		return struct{}{}, f(r)
	})

	return err
}

// parseRef pairs a repository string with an issue number, for the calls that return more than one
// value besides the error.
func parseRef(repo string, number int) (workflow.IssueRef, error) {
	r, err := workflow.ParseRepo(repo)
	if err != nil {
		return workflow.IssueRef{}, err
	}

	return workflow.IssueRef{Repo: r, Number: number}, nil
}

// ListLabeled parses the names, then calls the checked client.
func (c ghClient) ListLabeled(ctx context.Context, repo, label string) ([]workflow.Issue, error) {
	return onRepo(repo, func(r workflow.Repo) ([]workflow.Issue, error) {
		return c.Client.ListLabeled(ctx, r, label)
	})
}

// ListOpenIssues parses the names, then calls the checked client.
func (c ghClient) ListOpenIssues(ctx context.Context, repo string) ([]workflow.Issue, error) {
	return onRepo(repo, func(r workflow.Repo) ([]workflow.Issue, error) {
		return c.Client.ListOpenIssues(ctx, r)
	})
}

// ListIssues parses the names, then calls the checked client.
func (c ghClient) ListIssues(ctx context.Context, repo, state string) ([]workflow.Issue, error) {
	return onRepo(repo, func(r workflow.Repo) ([]workflow.Issue, error) {
		return c.Client.ListIssues(ctx, r, state)
	})
}

// ListOldestIssues parses the names, then calls the checked client.
func (c ghClient) ListOldestIssues(ctx context.Context, repo, state string) ([]workflow.Issue, error) {
	return onRepo(repo, func(r workflow.Repo) ([]workflow.Issue, error) {
		return c.Client.ListOldestIssues(ctx, r, state)
	})
}

// EnsureLabels parses the names, then calls the checked client.
func (c ghClient) EnsureLabels(ctx context.Context, repo string, labels []string) error {
	return onRepoErr(repo, func(r workflow.Repo) error { return c.Client.EnsureLabels(ctx, r, labels) })
}

// ListComments parses the names, then calls the checked client.
func (c ghClient) ListComments(ctx context.Context, repo string, number int) ([]workflow.Item, error) {
	return onRef(repo, number, func(ref workflow.IssueRef) ([]workflow.Item, error) {
		return c.Client.ListComments(ctx, ref)
	})
}

// CollaboratorPermission parses the names, then calls the checked client.
func (c ghClient) CollaboratorPermission(ctx context.Context, repo, login string) (string, error) {
	return onRepo(repo, func(r workflow.Repo) (string, error) {
		return c.Client.CollaboratorPermission(ctx, r, login)
	})
}

// OpenBlockers parses the names, then calls the checked client.
func (c ghClient) OpenBlockers(ctx context.Context, repo string, number int) ([]workflow.Issue, bool, error) {
	ref, err := parseRef(repo, number)
	if err != nil {
		return nil, false, err
	}

	return c.Client.OpenBlockers(ctx, ref)
}

// GetIssue parses the names, then calls the checked client.
func (c ghClient) GetIssue(ctx context.Context, repo string, number int) (workflow.Issue, error) {
	return onRef(repo, number, func(ref workflow.IssueRef) (workflow.Issue, error) {
		return c.Client.GetIssue(ctx, ref)
	})
}

// CreateIssue parses the names, then calls the checked client.
func (c ghClient) CreateIssue(
	ctx context.Context, repo, title, body string, labels []string,
) (workflow.Issue, error) {
	return onRepo(repo, func(r workflow.Repo) (workflow.Issue, error) {
		return c.Client.CreateIssue(ctx, r, title, body, labels)
	})
}

// UpdateIssueBody parses the names, then calls the checked client.
func (c ghClient) UpdateIssueBody(ctx context.Context, repo string, number int, body string) error {
	return onRefErr(repo, number, func(ref workflow.IssueRef) error {
		return c.Client.UpdateIssueBody(ctx, ref, body)
	})
}

// CloseIssue parses the names, then calls the checked client.
func (c ghClient) CloseIssue(ctx context.Context, repo string, number int) error {
	return onRefErr(repo, number, func(ref workflow.IssueRef) error { return c.Client.CloseIssue(ctx, ref) })
}

// SubIssueNumbers parses the names, then calls the checked client.
func (c ghClient) SubIssueNumbers(ctx context.Context, repo string, parent int) ([]int, bool, error) {
	ref, err := parseRef(repo, parent)
	if err != nil {
		return nil, false, err
	}

	return c.Client.SubIssueNumbers(ctx, ref)
}

// AddSubIssue parses the names, then calls the checked client.
func (c ghClient) AddSubIssue(ctx context.Context, repo string, parent int, childID int64) error {
	return onRefErr(repo, parent, func(ref workflow.IssueRef) error {
		return c.Client.AddSubIssue(ctx, ref, childID)
	})
}

// IssueID parses the names, then calls the checked client.
func (c ghClient) IssueID(ctx context.Context, repo string, number int) (int64, error) {
	return onRef(repo, number, func(ref workflow.IssueRef) (int64, error) { return c.Client.IssueID(ctx, ref) })
}

// AddBlockedBy parses the names, then calls the checked client.
func (c ghClient) AddBlockedBy(ctx context.Context, repo string, child int, blockerID int64) error {
	return onRefErr(repo, child, func(ref workflow.IssueRef) error {
		return c.Client.AddBlockedBy(ctx, ref, blockerID)
	})
}

// BlockedByNumbers parses the names, then calls the checked client.
func (c ghClient) BlockedByNumbers(ctx context.Context, repo string, number int) ([]int, bool, error) {
	ref, err := parseRef(repo, number)
	if err != nil {
		return nil, false, err
	}

	return c.Client.BlockedByNumbers(ctx, ref)
}

// Parent parses the names, then calls the checked client.
func (c ghClient) Parent(ctx context.Context, repo string, number int) (workflow.Issue, bool, error) {
	ref, err := parseRef(repo, number)
	if err != nil {
		return workflow.Issue{}, false, err
	}

	return c.Client.Parent(ctx, ref)
}

// PostResolution parses the names, then calls the checked client.
func (c ghClient) PostResolution(ctx context.Context, repo string, number int, branch, text string) error {
	return onRefErr(repo, number, func(ref workflow.IssueRef) error {
		return c.Client.PostResolution(ctx, ref, branch, text)
	})
}

// CloseResolved parses the names, then calls the checked client.
func (c ghClient) CloseResolved(ctx context.Context, repo string, number int, t workflow.Transition) error {
	return onRefErr(repo, number, func(ref workflow.IssueRef) error {
		return c.Client.CloseResolved(ctx, ref, t)
	})
}

// Comment parses the names, then calls the checked client.
func (c ghClient) Comment(ctx context.Context, repo string, number int, text string) error {
	return onRefErr(repo, number, func(ref workflow.IssueRef) error { return c.Client.Comment(ctx, ref, text) })
}

// AddLabel parses the names, then calls the checked client.
func (c ghClient) AddLabel(ctx context.Context, repo string, number int, label string) error {
	return onRefErr(repo, number, func(ref workflow.IssueRef) error { return c.Client.AddLabel(ctx, ref, label) })
}

// Claim parses the names, then calls the checked client.
func (c ghClient) Claim(ctx context.Context, repo string, number int, t workflow.Transition) (bool, error) {
	return onRef(repo, number, func(ref workflow.IssueRef) (bool, error) { return c.Client.Claim(ctx, ref, t) })
}

// Finish parses the names, then calls the checked client.
func (c ghClient) Finish(
	ctx context.Context, repo string, number int, t workflow.Transition, text string,
) (int64, error) {
	return onRef(repo, number, func(ref workflow.IssueRef) (int64, error) {
		return c.Client.Finish(ctx, ref, t, text)
	})
}

// CommentReturningID parses the names, then calls the checked client.
func (c ghClient) CommentReturningID(ctx context.Context, repo string, number int, text string) (int64, error) {
	return onRef(repo, number, func(ref workflow.IssueRef) (int64, error) {
		return c.Client.CommentReturningID(ctx, ref, text)
	})
}

// Timeline parses the names, then calls the checked client.
func (c ghClient) Timeline(ctx context.Context, repo string, number int) ([]workflow.TimelineEvent, error) {
	return onRef(repo, number, func(ref workflow.IssueRef) ([]workflow.TimelineEvent, error) {
		return c.Client.Timeline(ctx, ref)
	})
}

// ExistingPR parses the names, then calls the checked client.
func (c ghClient) ExistingPR(ctx context.Context, repo, branch string) (*github.PullRequest, error) {
	return onRepo(repo, func(r workflow.Repo) (*github.PullRequest, error) {
		return c.Client.ExistingPR(ctx, r, branch)
	})
}

// CreatePR parses the names, then calls the checked client.
func (c ghClient) CreatePR(ctx context.Context, repo, title, head, base, body string) (github.PullRequest, error) {
	return onRepo(repo, func(r workflow.Repo) (github.PullRequest, error) {
		return c.Client.CreatePR(ctx, r, title, head, base, body)
	})
}

// Repository parses the names, then calls the checked client.
func (c ghClient) Repository(ctx context.Context, repo string) (github.Repository, error) {
	return onRepo(repo, func(r workflow.Repo) (github.Repository, error) { return c.Client.Repository(ctx, r) })
}

// DefaultBranch parses the names, then calls the checked client.
func (c ghClient) DefaultBranch(ctx context.Context, repo string) (string, error) {
	return onRepo(repo, func(r workflow.Repo) (string, error) { return c.Client.DefaultBranch(ctx, r) })
}

// CheckRuns parses the names, then calls the checked client.
func (c ghClient) CheckRuns(ctx context.Context, repo, ref string) ([]workflow.CheckRun, error) {
	return onRepo(repo, func(r workflow.Repo) ([]workflow.CheckRun, error) {
		return c.Client.CheckRuns(ctx, r, ref)
	})
}

// CommitStatuses parses the names, then calls the checked client.
func (c ghClient) CommitStatuses(ctx context.Context, repo, ref string) ([]workflow.CommitStatus, error) {
	return onRepo(repo, func(r workflow.Repo) ([]workflow.CommitStatus, error) {
		return c.Client.CommitStatuses(ctx, r, ref)
	})
}

// JobLog parses the names, then calls the checked client.
func (c ghClient) JobLog(ctx context.Context, repo string, id int64) (string, error) {
	return onRepo(repo, func(r workflow.Repo) (string, error) { return c.Client.JobLog(ctx, r, id) })
}

// Reviews parses the names, then calls the checked client.
func (c ghClient) Reviews(ctx context.Context, repo string, pr int) ([]workflow.Item, error) {
	return onRef(repo, pr, func(ref workflow.IssueRef) ([]workflow.Item, error) { return c.Client.Reviews(ctx, ref) })
}

// LatestReviewBody parses the names, then calls the checked client.
func (c ghClient) LatestReviewBody(ctx context.Context, repo string, pr int) (string, error) {
	return onRef(repo, pr, func(ref workflow.IssueRef) (string, error) {
		return c.Client.LatestReviewBody(ctx, ref)
	})
}

// PostRetro parses the names, then calls the checked client.
func (c ghClient) PostRetro(
	ctx context.Context, repo string, number int, role workflow.Role, key string, body string,
) error {
	return onRefErr(repo, number, func(ref workflow.IssueRef) error {
		return c.Client.PostRetro(ctx, ref, role, key, body)
	})
}

// PostReview parses the names, then calls the checked client.
func (c ghClient) PostReview(
	ctx context.Context, repo string, pr int, sha, body string, verdict workflow.Verdict,
) error {
	return onRefErr(repo, pr, func(ref workflow.IssueRef) error {
		return c.Client.PostReview(ctx, ref, sha, body, verdict)
	})
}

// UpdatePR parses the names, then calls the checked client.
func (c ghClient) UpdatePR(ctx context.Context, repo string, pr int, title, text string) error {
	return onRefErr(repo, pr, func(ref workflow.IssueRef) error { return c.Client.UpdatePR(ctx, ref, title, text) })
}

// RequestReviewers parses the names, then calls the checked client.
func (c ghClient) RequestReviewers(ctx context.Context, repo string, pr int, reviewers []string) error {
	return onRefErr(repo, pr, func(ref workflow.IssueRef) error {
		return c.Client.RequestReviewers(ctx, ref, reviewers)
	})
}

// PostReplies parses the names, then calls the checked client.
func (c ghClient) PostReplies(ctx context.Context, repo string, pr int, sha, text string) error {
	return onRefErr(repo, pr, func(ref workflow.IssueRef) error {
		return c.Client.PostReplies(ctx, ref, sha, text)
	})
}

// DeleteComment parses the names, then calls the checked client.
func (c ghClient) DeleteComment(ctx context.Context, repo string, id int64) error {
	return onRepoErr(repo, func(r workflow.Repo) error { return c.Client.DeleteComment(ctx, r, id) })
}
