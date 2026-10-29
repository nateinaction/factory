// Package github is foreman's client for GitHub's REST API: one token per client, bounded pages and
// responses, and errors that name the call but never the token.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// GitHub API paging and size ceilings.
const (
	// IssuesPerPage is how many queued issues a poll looks at.
	IssuesPerPage = 20
	// PageSize is the page size of list endpoints.
	PageSize = 100
	// MaxPages bounds how many pages Paged fetches.
	MaxPages = 5
	// HTTPTimeout bounds one API call.
	HTTPTimeout = 20 * time.Second
	// maxResponseBytes bounds one API response.
	maxResponseBytes = 16 << 20
	// userAgent identifies foreman to GitHub.
	userAgent = "foreman"
	// apiVersion pins GitHub's REST API version.
	apiVersion = "2022-11-28"
	// StateOpen is the query value that lists open issues only.
	StateOpen = "open"
	// StateAll is the query value that lists issues in any state.
	StateAll = "all"
	// stateParam is the query parameter that filters by issue state.
	stateParam = "state"
	// sortParam is the query parameter that picks the sort field.
	sortParam = "sort"
	// directionParam is the query parameter that picks the sort direction.
	directionParam = "direction"
	// sortCreated sorts by creation time.
	sortCreated = "created"
	// Oldest is the oldest-first sort direction of an issue listing.
	Oldest = "asc"
	// Newest is the newest-first sort direction of an issue listing.
	Newest = "desc"
)

// Client calls GitHub's REST API with one token.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// NewClient returns a client for the API at base.
func NewClient(base, token string, hc *http.Client) Client {
	return Client{base: strings.TrimRight(base, "/"), token: token, hc: hc}
}

// HTTPError is a non-2xx answer. It names the call, never the token.
type HTTPError struct {
	Method string
	Path   string
	Code   int
}

// Error describes the failed call.
func (e *HTTPError) Error() string {
	return fmt.Sprintf("github %s %s: HTTP %d", e.Method, e.Path, e.Code)
}

// isStatus reports whether err is an HTTPError with the given code.
func isStatus(err error, code int) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Code == code
}

// Call makes one API request; in is sent as JSON and the answer is decoded into out, when both are given.
func (c Client) Call(ctx context.Context, method, path string, in, out any) error {
	raw, err := c.send(ctx, method, path, in)
	if err != nil {
		return err
	}

	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("github %s %s: decode: %w", method, path, err)
	}

	return nil
}

// send is the one request path: it sends the request, bounds the answer's body and checks its status,
// then returns the body. Call decodes that body as JSON and JobLog reads it as text, so every answer
// is closed, capped and status-checked in this one place, whatever its content type.
func (c Client) send(ctx context.Context, method, path string, in any) ([]byte, error) {
	req, err := c.newRequest(ctx, method, path, in)
	if err != nil {
		return nil, err
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github %s %s: %w", method, path, err)
	}

	defer func() {
		// The body is fully read or abandoned on error; a close failure changes nothing for the caller.
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))

	switch {
	case err != nil:
		return nil, fmt.Errorf("github %s %s: read: %w", method, path, err)
	case len(raw) > maxResponseBytes:
		return nil, fmt.Errorf("github %s %s: response over %d bytes", method, path, maxResponseBytes)
	case resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices:
		return nil, &HTTPError{Method: method, Path: path, Code: resp.StatusCode}
	}

	return raw, nil
}

// newRequest builds an authenticated request with in, when given, as its JSON body.
func (c Client) newRequest(ctx context.Context, method, path string, in any) (*http.Request, error) {
	var body io.Reader

	if in != nil {
		var buf bytes.Buffer

		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)

		if err := enc.Encode(in); err != nil {
			return nil, fmt.Errorf("encode %s %s: %w", method, path, err)
		}

		body = &buf
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("build %s %s: %w", method, path, err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")

	return req, nil
}

// apiRoot is the segment every repository's API path starts below.
const apiRoot = "repos"

// repoPath builds the API path of a repository, then of each part under it.
func repoPath(repo workflow.Repo, parts ...string) string {
	return joinPath(append([]string{apiRoot, repo.Owner(), repo.Name()}, parts...)...)
}

// issuePath builds the API path of an issue, then of each part under it. ref.Number names the issue.
func issuePath(ref workflow.IssueRef, parts ...string) string {
	return joinPath(append([]string{
		apiRoot, ref.Repo.Owner(), ref.Repo.Name(), "issues", strconv.Itoa(ref.Number),
	}, parts...)...)
}

// pullPath builds the API path of a pull request, then of each part under it. ref.Number names the
// pull request.
func pullPath(ref workflow.IssueRef, parts ...string) string {
	return joinPath(append([]string{
		apiRoot, ref.Repo.Owner(), ref.Repo.Name(), "pulls", strconv.Itoa(ref.Number),
	}, parts...)...)
}

// commentPath builds the API path of one issue comment, which the rest of the issue's path does not
// name: a comment is addressed by its own id.
func commentPath(repo workflow.Repo, id int64) string {
	return repoPath(repo, "issues", "comments", strconv.FormatInt(id, 10))
}

// joinPath builds a rooted API path, escaping each segment so no segment can leave the path it
// belongs to.
func joinPath(segments ...string) string {
	escaped := make([]string, 0, len(segments)+1)

	for _, segment := range segments {
		escaped = append(escaped, url.PathEscape(segment))
	}

	return "/" + strings.Join(escaped, "/")
}

// Request bodies foreman sends.
type (
	// bodyRequest sets a comment's body.
	bodyRequest struct {
		Body string `json:"body"`
	}
	// labelsRequest adds labels to an issue.
	labelsRequest struct {
		Labels []string `json:"labels"`
	}
	// labelRequest creates a label in a repository.
	labelRequest struct {
		Name string `json:"name"`
	}
	// createIssueRequest opens an issue.
	createIssueRequest struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels,omitempty"`
	}
	// issueBodyRequest replaces an issue's body.
	issueBodyRequest struct {
		Body string `json:"body"`
	}
	// issueStateRequest changes an issue's state.
	issueStateRequest struct {
		State string `json:"state"`
	}
	// subIssueRequest links an existing issue as a sub-issue.
	subIssueRequest struct {
		SubIssueID int64 `json:"sub_issue_id"`
	}
	// dependencyRequest blocks one issue on another. GitHub names the blocker's database id.
	dependencyRequest struct {
		IssueID int64 `json:"issue_id"`
	}
	// pullRequestRequest opens a pull request.
	pullRequestRequest struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
	}
	// pullRequestBody updates a pull request's text.
	pullRequestBody struct {
		Title string `json:"title,omitempty"`
		Body  string `json:"body,omitempty"`
	}
	// reviewersRequest asks people to review a pull request.
	reviewersRequest struct {
		Reviewers []string `json:"reviewers"`
	}
	// reviewRequest posts a pull request review on a commit.
	reviewRequest struct {
		CommitID string `json:"commit_id"`
		Body     string `json:"body"`
		Event    string `json:"event"`
	}
)

// Paged returns every item of a list endpoint, up to MaxPages pages.
func Paged[T any](ctx context.Context, c Client, path string) ([]T, error) {
	var items []T

	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}

	for page := 1; page <= MaxPages; page++ {
		var got []T

		p := fmt.Sprintf("%s%sper_page=%d&page=%d", path, sep, PageSize, page)
		if err := c.Call(ctx, http.MethodGet, p, nil, &got); err != nil {
			return nil, err
		}

		items = append(items, got...)
		if len(got) < PageSize {
			break
		}
	}

	return items, nil
}

// ListLabeled lists the oldest open issues carrying label.
func (c Client) ListLabeled(ctx context.Context, repo workflow.Repo, label string) ([]workflow.Issue, error) {
	q := url.Values{
		stateParam: {StateOpen}, "labels": {label}, sortParam: {sortCreated},
		directionParam: {Oldest}, "per_page": {fmt.Sprint(IssuesPerPage)},
	}

	var issues []workflow.Issue

	err := c.Call(ctx, http.MethodGet, repoPath(repo, "issues")+"?"+q.Encode(), nil, &issues)

	return issues, err
}

// ListIssues lists a repository's issues in state ("open", "closed" or StateAll), ordered by creation
// in direction (Newest or Oldest). Oldest first keeps an issue that has been there since the
// repository was assigned in reach on a repository with more issues than one scan holds. Pull
// requests are included in the answer and the caller skips them.
func (c Client) ListIssues(
	ctx context.Context, repo workflow.Repo, state, direction string,
) ([]workflow.Issue, error) {
	q := url.Values{stateParam: {state}, sortParam: {sortCreated}, directionParam: {direction}}

	return Paged[workflow.Issue](ctx, c, repoPath(repo, "issues")+"?"+q.Encode())
}

// Label is the part of a repository label foreman reads.
type Label struct {
	// Name is the label's text.
	Name string `json:"name"`
}

// EnsureLabels creates each label the repository lacks. A label another poll created between the
// read and the write is already there, so its 422 is not an error.
func (c Client) EnsureLabels(ctx context.Context, repo workflow.Repo, labels []string) error {
	existing, err := Paged[Label](ctx, c, repoPath(repo, "labels"))
	if err != nil {
		return err
	}

	have := make(map[string]bool, len(existing))
	for _, l := range existing {
		have[l.Name] = true
	}

	for _, name := range labels {
		if have[name] {
			continue
		}

		err := c.Call(ctx, http.MethodPost, repoPath(repo, "labels"), labelRequest{Name: name}, nil)
		if err != nil && !isStatus(err, http.StatusUnprocessableEntity) {
			return err
		}
	}

	return nil
}

// ListComments lists an issue's comments, oldest first.
func (c Client) ListComments(ctx context.Context, ref workflow.IssueRef) ([]workflow.Item, error) {
	return Paged[workflow.Item](ctx, c, issuePath(ref, "comments"))
}

// CollaboratorPermission reads a user's permission on the repository. A user who is not a
// collaborator has none; any other failure is returned, so the caller can fail closed.
func (c Client) CollaboratorPermission(ctx context.Context, repo workflow.Repo, login string) (string, error) {
	var got struct {
		Permission string `json:"permission"`
	}

	path := repoPath(repo, "collaborators", login, "permission")

	err := c.Call(ctx, http.MethodGet, path, nil, &got)
	if isStatus(err, http.StatusNotFound) {
		return "none", nil
	}

	if err != nil {
		return "", err
	}

	return got.Permission, nil
}

// OpenBlockers lists the open issues blocking number. supported is false when the repository has no
// issue dependencies, so the caller falls back to the issue's "Blocked by:" line.
func (c Client) OpenBlockers(ctx context.Context, ref workflow.IssueRef) ([]workflow.Issue, bool, error) {
	var blocking []workflow.Issue

	path := issuePath(ref, "dependencies", "blocked_by")

	if supported, err := endpointSupported(c.Call(ctx, http.MethodGet, path, nil, &blocking)); !supported {
		return nil, false, err
	}

	open := make([]workflow.Issue, 0, len(blocking))

	for _, b := range blocking {
		if b.State != string(workflow.Closed) {
			open = append(open, b)
		}
	}

	return open, true, nil
}

// GetIssue reads one issue.
func (c Client) GetIssue(ctx context.Context, ref workflow.IssueRef) (workflow.Issue, error) {
	var issue workflow.Issue

	err := c.Call(ctx, http.MethodGet, issuePath(ref), nil, &issue)

	return issue, err
}

// CreateIssue opens an issue with the labels foreman derived and returns it.
func (c Client) CreateIssue(
	ctx context.Context, repo workflow.Repo, title, body string, labels []string,
) (workflow.Issue, error) {
	var issue workflow.Issue

	err := c.Call(ctx, http.MethodPost, repoPath(repo, "issues"),
		createIssueRequest{Title: title, Body: body, Labels: labels}, &issue)

	return issue, err
}

// UpdateIssueBody replaces an issue's body.
func (c Client) UpdateIssueBody(ctx context.Context, ref workflow.IssueRef, body string) error {
	return c.Call(ctx, http.MethodPatch, issuePath(ref),
		issueBodyRequest{Body: body}, nil)
}

// CloseIssue closes an issue.
func (c Client) CloseIssue(ctx context.Context, ref workflow.IssueRef) error {
	return c.Call(ctx, http.MethodPatch, issuePath(ref),
		issueStateRequest{State: string(workflow.Closed)}, nil)
}

// endpointSupported reads the outcome of a call to an endpoint a repository may lack. A 404 means the
// repository does not have the feature: unsupported, no error, so the caller falls back to the body's
// convention. Any other failure is returned unsupported, so the caller fails closed.
func endpointSupported(err error) (bool, error) {
	if isStatus(err, http.StatusNotFound) {
		return false, nil
	}

	return err == nil, err
}

// SubIssueNumbers lists the issues linked as sub-issues of parent. supported is false when the
// repository has no sub-issues, so the caller can fall back to a "Part of #N" line.
func (c Client) SubIssueNumbers(ctx context.Context, ref workflow.IssueRef) ([]int, bool, error) {
	var children []workflow.Issue

	path := issuePath(ref, "sub_issues")

	if supported, err := endpointSupported(c.Call(ctx, http.MethodGet, path, nil, &children)); !supported {
		return nil, false, err
	}

	numbers := make([]int, 0, len(children))
	for _, child := range children {
		numbers = append(numbers, child.Number)
	}

	return numbers, true, nil
}

// AddSubIssue links child, by database id, as a sub-issue of parent. GitHub's 422 for an existing
// link is not an error: applying the same proposal again changes nothing.
func (c Client) AddSubIssue(ctx context.Context, ref workflow.IssueRef, childID int64) error {
	path := issuePath(ref, "sub_issues")

	err := c.Call(ctx, http.MethodPost, path, subIssueRequest{SubIssueID: childID}, nil)
	if isStatus(err, http.StatusUnprocessableEntity) {
		return nil
	}

	return err
}

// IssueID reads an issue's database id, which a dependency link names.
func (c Client) IssueID(ctx context.Context, ref workflow.IssueRef) (int64, error) {
	issue, err := c.GetIssue(ctx, ref)
	if err != nil {
		return 0, err
	}

	if issue.ID == 0 {
		return 0, fmt.Errorf("issue %s#%d has no database id", ref.Repo, ref.Number)
	}

	return issue.ID, nil
}

// AddBlockedBy blocks child, by database id, on blocker. GitHub's 422 for an existing edge is not an
// error: applying the same proposal again changes nothing.
func (c Client) AddBlockedBy(ctx context.Context, ref workflow.IssueRef, blockerID int64) error {
	path := issuePath(ref, "dependencies", "blocked_by")

	err := c.Call(ctx, http.MethodPost, path, dependencyRequest{IssueID: blockerID}, nil)
	if isStatus(err, http.StatusUnprocessableEntity) {
		return nil
	}

	return err
}

// BlockedByNumbers lists every issue blocking number, open or closed, for an idempotent apply.
// supported is false when the repository has no issue dependencies.
func (c Client) BlockedByNumbers(ctx context.Context, ref workflow.IssueRef) ([]int, bool, error) {
	var blockers []workflow.Issue

	path := issuePath(ref, "dependencies", "blocked_by")

	if supported, err := endpointSupported(c.Call(ctx, http.MethodGet, path, nil, &blockers)); !supported {
		return nil, false, err
	}

	numbers := make([]int, 0, len(blockers))
	for _, blocker := range blockers {
		numbers = append(numbers, blocker.Number)
	}

	return numbers, true, nil
}

// Parent reads the issue's parent, when the repository supports sub-issues. supported is false when
// the endpoint is missing, so the caller can fall back to the body's "Part of #N" convention.
func (c Client) Parent(ctx context.Context, ref workflow.IssueRef) (workflow.Issue, bool, error) {
	var parent workflow.Issue

	path := issuePath(ref, "parent")

	if supported, err := endpointSupported(c.Call(ctx, http.MethodGet, path, nil, &parent)); !supported {
		return workflow.Issue{}, false, err
	}

	return parent, true, nil
}

// UpsertComment writes text as the one comment on ref that starts with marker: it edits that comment
// when one exists and creates it otherwise, so writing the same marked comment again changes nothing.
func (c Client) UpsertComment(ctx context.Context, ref workflow.IssueRef, marker, text string) error {
	comments, err := c.ListComments(ctx, ref)
	if err != nil {
		return err
	}

	if mine := workflow.StartsWith(comments, marker); len(mine) > 0 {
		return c.Call(ctx, http.MethodPatch, commentPath(ref.Repo, mine[0].ID), bodyRequest{Body: text}, nil)
	}

	return c.Comment(ctx, ref, text)
}

// CloseResolved records a resolved ticket's final transition, releases its claim and closes it. A
// closed ticket carries no factory label. A transition already recorded is not recorded again.
func (c Client) CloseResolved(ctx context.Context, ref workflow.IssueRef, t workflow.Transition) error {
	marker, err := workflow.RenderTransition(t)
	if err != nil {
		return err
	}

	mine, err := c.recordedMarker(ctx, ref, marker)
	if err != nil {
		return err
	}

	if mine == 0 {
		if err := c.Comment(ctx, ref, marker); err != nil {
			return err
		}
	}

	if err := c.removeLabel(ctx, ref, string(t.From)); err != nil && !isStatus(err, http.StatusNotFound) {
		// A 404 means a person already removed it: fine, still close the ticket.
		return err
	}

	return c.Call(ctx, http.MethodPatch, issuePath(ref),
		map[string]string{stateParam: string(workflow.Closed)}, nil)
}

// Comment posts a comment on an issue.
func (c Client) Comment(ctx context.Context, ref workflow.IssueRef, text string) error {
	return c.Call(ctx, http.MethodPost, issuePath(ref, "comments"),
		bodyRequest{Body: text}, nil)
}

// AddLabel adds one label to an issue.
func (c Client) AddLabel(ctx context.Context, ref workflow.IssueRef, label string) error {
	return c.Call(ctx, http.MethodPost, issuePath(ref, "labels"), labelsRequest{Labels: []string{label}}, nil)
}

// RemoveLabel takes one label off an issue. A label already gone is not an error, so the removal is
// safe to repeat.
func (c Client) RemoveLabel(ctx context.Context, ref workflow.IssueRef, label string) error {
	if err := c.removeLabel(ctx, ref, label); err != nil && !isStatus(err, http.StatusNotFound) {
		return err
	}

	return nil
}

// removeLabel removes one label from an issue.
func (c Client) removeLabel(ctx context.Context, ref workflow.IssueRef, label string) error {
	path := issuePath(ref, "labels", label)

	return c.Call(ctx, http.MethodDelete, path, nil, nil)
}

// Claim posts the transition marker, then takes the queued label off, adds the running label, and
// reads the issue back to check the claim stuck. It is false when the queued label is already gone:
// another poll got there first, so this claim fails closed.
func (c Client) Claim(ctx context.Context, ref workflow.IssueRef, t workflow.Transition) (bool, error) {
	marker, err := workflow.RenderTransition(t)
	if err != nil {
		return false, err
	}

	if err := c.Comment(ctx, ref, marker); err != nil {
		return false, err
	}

	if err := c.removeLabel(ctx, ref, string(t.From)); err != nil {
		if isStatus(err, http.StatusNotFound) {
			return false, nil
		}

		return false, err
	}

	if err := c.AddLabel(ctx, ref, string(t.To)); err != nil {
		return false, err
	}

	issue, err := c.GetIssue(ctx, ref)
	if err != nil {
		return false, err
	}

	if !workflow.HasLabel(issue, string(t.To)) {
		return false, fmt.Errorf("claim of %s#%d did not stick", ref.Repo, ref.Number)
	}

	return true, nil
}

// Finish posts the transition marker and its outcome, swaps src for dst, and tolerates a label a
// person already moved. It returns the comment's id, which a Planner retro is keyed by. A transition
// already recorded is not recorded again, so a run can settle before its retro and `start` can still
// apply the same transition afterwards.
func (c Client) Finish(
	ctx context.Context, ref workflow.IssueRef, t workflow.Transition, text string,
) (int64, error) {
	marker, err := workflow.RenderTransition(t)
	if err != nil {
		return 0, err
	}

	id, err := c.recordedMarker(ctx, ref, marker)
	if err != nil {
		return 0, err
	}

	if id == 0 {
		body := marker
		if text != "" {
			body += "\n\n" + text
		}

		if id, err = c.CommentReturningID(ctx, ref, body); err != nil {
			return 0, err
		}
	}

	if err := c.removeLabel(ctx, ref, string(t.From)); err != nil && !isStatus(err, http.StatusNotFound) {
		// A 404 means a person already removed it: fine, still record the outcome.
		return 0, err
	}

	return id, c.AddLabel(ctx, ref, string(t.To))
}

// recordedMarker is the id of the comment that already carries marker, or 0 when none does. A
// transition found already recorded still has its labels applied, so a crash between the comment and
// the labels is repaired rather than stranded.
func (c Client) recordedMarker(ctx context.Context, ref workflow.IssueRef, marker string) (int64, error) {
	comments, err := c.ListComments(ctx, ref)
	if err != nil {
		// Fail closed: when authorship cannot be read, no comment counts as recorded.
		return 0, err
	}

	if mine := workflow.FactoryStartsWith(comments, marker, ref.Repo.Owner()); len(mine) > 0 {
		return mine[0].ID, nil
	}

	return 0, nil
}

// CommentReturningID posts a comment and returns its id.
func (c Client) CommentReturningID(ctx context.Context, ref workflow.IssueRef, text string) (int64, error) {
	var comment workflow.Item

	err := c.Call(ctx, http.MethodPost, issuePath(ref, "comments"),
		bodyRequest{Body: text}, &comment)

	return comment.ID, err
}

// Timeline lists an issue's timeline events, oldest first.
func (c Client) Timeline(ctx context.Context, ref workflow.IssueRef) ([]workflow.TimelineEvent, error) {
	return Paged[workflow.TimelineEvent](ctx, c, issuePath(ref, "timeline"))
}

// PullRequest is the part of a pull request foreman reads.
type PullRequest struct {
	// Number is the pull request number.
	Number int `json:"number"`
	// HTMLURL is the advertised web URL.
	HTMLURL string `json:"html_url"`
	// Head is the branch the pull request comes from, including the commit a review judges.
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// ExistingPR finds the open pull request for a branch, or nil.
func (c Client) ExistingPR(ctx context.Context, repo workflow.Repo, branch string) (*PullRequest, error) {
	owner, _, _ := strings.Cut(repo.String(), "/")
	q := url.Values{stateParam: {StateOpen}, "head": {owner + ":" + branch}}

	var pulls []PullRequest
	if err := c.Call(ctx, http.MethodGet, repoPath(repo, "pulls")+"?"+q.Encode(), nil, &pulls); err != nil {
		return nil, err
	}

	if len(pulls) == 0 {
		return nil, nil //nolint:nilnil // nil without error means "no open pull request"
	}

	return &pulls[0], nil
}

// CreatePR opens a pull request.
func (c Client) CreatePR(ctx context.Context, repo workflow.Repo, title, head, base, body string) (PullRequest, error) {
	var pr PullRequest

	err := c.Call(ctx, http.MethodPost, repoPath(repo, "pulls"),
		pullRequestRequest{Title: title, Head: head, Base: base, Body: body}, &pr)

	return pr, err
}

// Repository is the part of a repository foreman reads.
type Repository struct {
	// Private is whether only its collaborators can see it.
	Private bool `json:"private"`
	// DefaultBranch is the branch a clone starts from.
	DefaultBranch string `json:"default_branch"`
}

// Repository reads a repository's visibility and default branch.
func (c Client) Repository(ctx context.Context, repo workflow.Repo) (Repository, error) {
	var r Repository

	if err := c.Call(ctx, http.MethodGet, repoPath(repo), nil, &r); err != nil {
		return Repository{}, err
	}

	return r, nil
}

// DefaultBranch reads the repository's default branch.
func (c Client) DefaultBranch(ctx context.Context, repo workflow.Repo) (string, error) {
	r, err := c.Repository(ctx, repo)
	if err != nil {
		return "", err
	}

	if r.DefaultBranch == "" {
		return "", errors.New("repository has no default branch")
	}

	return r.DefaultBranch, nil
}

// CheckRuns lists the check runs GitHub reports for a commit, across up to MaxPages pages. The list
// endpoint wraps its items in an envelope, so it is paged by hand rather than by Paged.
func (c Client) CheckRuns(ctx context.Context, repo workflow.Repo, ref string) ([]workflow.CheckRun, error) {
	var out []workflow.CheckRun

	for page := 1; page <= MaxPages; page++ {
		var got struct {
			CheckRuns []workflow.CheckRun `json:"check_runs"`
		}

		path := fmt.Sprintf("%s?per_page=%d&page=%d", repoPath(repo, "commits", ref, "check-runs"), PageSize, page)

		if err := c.Call(ctx, http.MethodGet, path, nil, &got); err != nil {
			return nil, err
		}

		out = append(out, got.CheckRuns...)
		if len(got.CheckRuns) < PageSize {
			break
		}
	}

	return out, nil
}

// CommitStatuses lists the commit statuses GitHub reports for a commit.
func (c Client) CommitStatuses(ctx context.Context, repo workflow.Repo, ref string) ([]workflow.CommitStatus, error) {
	return Paged[workflow.CommitStatus](ctx, c, repoPath(repo, "commits", ref, "statuses"))
}

// JobLog reads the plain-text log of one Actions job. The endpoint redirects to a download at another
// host, which the HTTP client follows without the token.
func (c Client) JobLog(ctx context.Context, repo workflow.Repo, id int64) (string, error) {
	path := repoPath(repo, "actions", "jobs", strconv.FormatInt(id, 10), "logs")

	raw, err := c.send(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}

	return string(raw), nil
}

// Reviews lists a pull request's reviews.
func (c Client) Reviews(ctx context.Context, ref workflow.IssueRef) ([]workflow.Item, error) {
	return Paged[workflow.Item](ctx, c, pullPath(ref, "reviews"))
}

// LatestReviewBody is the body of the last review foreman posted, or empty.
func (c Client) LatestReviewBody(ctx context.Context, ref workflow.IssueRef) (string, error) {
	reviews, err := c.Reviews(ctx, ref)
	if err != nil {
		return "", err
	}

	ours := workflow.StartsWith(reviews, workflow.ReviewMarkerPrefix)
	if len(ours) == 0 {
		return "", nil
	}

	return ours[len(ours)-1].Body, nil
}

// PostReview posts a native pull request review on a commit, carrying the verdict. A review GitHub
// refuses because the App authored the pull request is posted as a comment review instead, so the
// findings still reach the pull request.
func (c Client) PostReview(
	ctx context.Context, ref workflow.IssueRef, sha, body string, verdict workflow.Verdict,
) error {
	path := pullPath(ref, "reviews")

	err := c.Call(ctx, http.MethodPost, path,
		reviewRequest{CommitID: sha, Body: body, Event: verdict.Event()}, nil)
	if err == nil {
		return nil
	}

	if !isStatus(err, http.StatusUnprocessableEntity) {
		return err
	}

	body += "\n\n_GitHub rejected the " + verdict.Event() + " review because the review agent is " +
		"also the pull request's author, so it is posted as a comment._"

	return c.Call(ctx, http.MethodPost, path, reviewRequest{CommitID: sha, Body: body, Event: "COMMENT"}, nil)
}

// UpdatePR replaces a pull request's title and body.
func (c Client) UpdatePR(ctx context.Context, ref workflow.IssueRef, title, text string) error {
	return c.Call(ctx, http.MethodPatch, pullPath(ref), pullRequestBody{Title: title, Body: text}, nil)
}

// RequestReviewers asks people to review a pull request. An empty list asks nobody, so foreman never
// calls GitHub with nothing to do.
func (c Client) RequestReviewers(ctx context.Context, ref workflow.IssueRef, reviewers []string) error {
	if len(reviewers) == 0 {
		return nil
	}

	return c.Call(ctx, http.MethodPost, pullPath(ref, "requested_reviewers"),
		reviewersRequest{Reviewers: reviewers}, nil)
}
