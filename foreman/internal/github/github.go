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
	// stateOpen is the query value that lists open issues only.
	stateOpen = "open"
	// stateParam is the query parameter that filters by issue state.
	stateParam = "state"
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
	req, err := c.newRequest(ctx, method, path, in)
	if err != nil {
		return err
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}

	defer func() {
		// The body is fully read or abandoned on error; a close failure changes nothing for the caller.
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))

	switch {
	case err != nil:
		return fmt.Errorf("github %s %s: read: %w", method, path, err)
	case len(raw) > maxResponseBytes:
		return fmt.Errorf("github %s %s: response over %d bytes", method, path, maxResponseBytes)
	case resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices:
		return &HTTPError{Method: method, Path: path, Code: resp.StatusCode}
	case out == nil || len(bytes.TrimSpace(raw)) == 0:
		return nil
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("github %s %s: decode: %w", method, path, err)
	}

	return nil
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
	// issueRequest opens an issue.
	issueRequest struct {
		Title  string   `json:"title"`
		Labels []string `json:"labels"`
		Body   string   `json:"body"`
	}
	// pullRequestRequest opens a pull request.
	pullRequestRequest struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
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
func (c Client) ListLabeled(ctx context.Context, repo, label string) ([]workflow.Issue, error) {
	q := url.Values{
		stateParam: {stateOpen}, "labels": {label}, "sort": {"created"},
		"direction": {"asc"}, "per_page": {fmt.Sprint(IssuesPerPage)},
	}

	var issues []workflow.Issue

	err := c.Call(ctx, http.MethodGet, "/repos/"+repo+"/issues?"+q.Encode(), nil, &issues)

	return issues, err
}

// ListOpenIssues lists every open issue, oldest first. Pull requests are included in the answer and
// the caller skips them.
func (c Client) ListOpenIssues(ctx context.Context, repo string) ([]workflow.Issue, error) {
	q := url.Values{stateParam: {stateOpen}, "sort": {"created"}, "direction": {"asc"}}

	return Paged[workflow.Issue](ctx, c, "/repos/"+repo+"/issues?"+q.Encode())
}

// Label is the part of a repository label foreman reads.
type Label struct {
	// Name is the label's text.
	Name string `json:"name"`
}

// EnsureLabels creates each label the repository lacks. A label another poll created between the
// read and the write is already there, so its 422 is not an error.
func (c Client) EnsureLabels(ctx context.Context, repo string, labels []string) error {
	existing, err := Paged[Label](ctx, c, "/repos/"+repo+"/labels")
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

		err := c.Call(ctx, http.MethodPost, "/repos/"+repo+"/labels", labelRequest{Name: name}, nil)
		if err != nil && !isStatus(err, http.StatusUnprocessableEntity) {
			return err
		}
	}

	return nil
}

// ListComments lists an issue's comments, oldest first.
func (c Client) ListComments(ctx context.Context, repo string, number int) ([]workflow.Item, error) {
	return Paged[workflow.Item](ctx, c, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number))
}

// CollaboratorPermission reads a user's permission on the repository. A user who is not a
// collaborator has none; any other failure is returned, so the caller can fail closed.
func (c Client) CollaboratorPermission(ctx context.Context, repo, login string) (string, error) {
	var got struct {
		Permission string `json:"permission"`
	}

	path := fmt.Sprintf("/repos/%s/collaborators/%s/permission", repo, url.PathEscape(login))

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
func (c Client) OpenBlockers(ctx context.Context, repo string, number int) ([]workflow.Issue, bool, error) {
	var blocking []workflow.Issue

	path := fmt.Sprintf("/repos/%s/issues/%d/dependencies/blocked_by", repo, number)

	err := c.Call(ctx, http.MethodGet, path, nil, &blocking)
	if isStatus(err, http.StatusNotFound) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, err
	}

	open := make([]workflow.Issue, 0, len(blocking))

	for _, b := range blocking {
		if b.State != "closed" {
			open = append(open, b)
		}
	}

	return open, true, nil
}

// GetIssue reads one issue.
func (c Client) GetIssue(ctx context.Context, repo string, number int) (workflow.Issue, error) {
	var issue workflow.Issue

	err := c.Call(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repo, number), nil, &issue)

	return issue, err
}

// Comment posts a comment on an issue.
func (c Client) Comment(ctx context.Context, repo string, number int, text string) error {
	return c.Call(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number),
		bodyRequest{Body: text}, nil)
}

// AddLabel adds one label to an issue.
func (c Client) AddLabel(ctx context.Context, repo string, number int, label string) error {
	return c.addLabel(ctx, repo, number, label)
}

// addLabel adds one label to an issue.
func (c Client) addLabel(ctx context.Context, repo string, number int, label string) error {
	return c.Call(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/labels", repo, number),
		labelsRequest{Labels: []string{label}}, nil)
}

// removeLabel removes one label from an issue.
func (c Client) removeLabel(ctx context.Context, repo string, number int, label string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/labels/%s", repo, number, url.PathEscape(label))

	return c.Call(ctx, http.MethodDelete, path, nil, nil)
}

// Claim posts the transition marker, then takes the queued label off, adds the running label, and
// reads the issue back to check the claim stuck. It is false when the queued label is already gone:
// another poll got there first, so this claim fails closed.
func (c Client) Claim(ctx context.Context, repo string, number int, t workflow.Transition) (bool, error) {
	marker, err := workflow.RenderTransition(t)
	if err != nil {
		return false, err
	}

	if err := c.Comment(ctx, repo, number, marker); err != nil {
		return false, err
	}

	if err := c.removeLabel(ctx, repo, number, string(t.From)); err != nil {
		if isStatus(err, http.StatusNotFound) {
			return false, nil
		}

		return false, err
	}

	if err := c.addLabel(ctx, repo, number, string(t.To)); err != nil {
		return false, err
	}

	issue, err := c.GetIssue(ctx, repo, number)
	if err != nil {
		return false, err
	}

	if !workflow.HasLabel(issue, string(t.To)) {
		return false, fmt.Errorf("claim of %s#%d did not stick", repo, number)
	}

	return true, nil
}

// Finish posts the transition marker and its outcome, swaps src for dst, and tolerates a label a
// person already moved.
func (c Client) Finish(ctx context.Context, repo string, number int, t workflow.Transition, text string) error {
	marker, err := workflow.RenderTransition(t)
	if err != nil {
		return err
	}

	body := marker
	if text != "" {
		body += "\n\n" + text
	}

	if err := c.Comment(ctx, repo, number, body); err != nil {
		return err
	}

	if err := c.removeLabel(ctx, repo, number, string(t.From)); err != nil && !isStatus(err, http.StatusNotFound) {
		// A 404 means a person already removed it: fine, still record the outcome.
		return err
	}

	return c.addLabel(ctx, repo, number, string(t.To))
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
func (c Client) ExistingPR(ctx context.Context, repo, branch string) (*PullRequest, error) {
	owner, _, _ := strings.Cut(repo, "/")
	q := url.Values{stateParam: {stateOpen}, "head": {owner + ":" + branch}}

	var pulls []PullRequest
	if err := c.Call(ctx, http.MethodGet, "/repos/"+repo+"/pulls?"+q.Encode(), nil, &pulls); err != nil {
		return nil, err
	}

	if len(pulls) == 0 {
		return nil, nil //nolint:nilnil // nil without error means "no open pull request"
	}

	return &pulls[0], nil
}

// CreatePR opens a pull request.
func (c Client) CreatePR(ctx context.Context, repo, title, head, base, body string) (PullRequest, error) {
	var pr PullRequest

	err := c.Call(ctx, http.MethodPost, "/repos/"+repo+"/pulls",
		pullRequestRequest{Title: title, Head: head, Base: base, Body: body}, &pr)

	return pr, err
}

// DefaultBranch reads the repository's default branch.
func (c Client) DefaultBranch(ctx context.Context, repo string) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.Call(ctx, http.MethodGet, "/repos/"+repo, nil, &r); err != nil {
		return "", err
	}

	if r.DefaultBranch == "" {
		return "", errors.New("repository has no default branch")
	}

	return r.DefaultBranch, nil
}

// Reviews lists a pull request's reviews.
func (c Client) Reviews(ctx context.Context, repo string, pr int) ([]workflow.Item, error) {
	return Paged[workflow.Item](ctx, c, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, pr))
}

// LatestReviewBody is the body of the last review foreman posted, or empty.
func (c Client) LatestReviewBody(ctx context.Context, repo string, pr int) (string, error) {
	reviews, err := c.Reviews(ctx, repo, pr)
	if err != nil {
		return "", err
	}

	ours := workflow.StartsWith(reviews, workflow.ReviewMarkerPrefix)
	if len(ours) == 0 {
		return "", nil
	}

	return ours[len(ours)-1].Body, nil
}

// RetroTicket returns the number of the one open retro ticket, creating it on first use.
func (c Client) RetroTicket(ctx context.Context, repo string) (int, error) {
	issues, err := c.ListLabeled(ctx, repo, workflow.LabelRetro)
	if err != nil {
		return 0, err
	}

	if found, ok := workflow.PickIssue(issues); ok {
		return found.Number, nil
	}

	var created workflow.Issue

	err = c.Call(ctx, http.MethodPost, "/repos/"+repo+"/issues", issueRequest{
		Title: workflow.RetroTitle, Labels: []string{workflow.LabelRetro},
		Body: "One comment per agent implementation run, from the retro skill.",
	}, &created)

	return created.Number, err
}

// PostRetro comments on the retro ticket, keyed by issue and commit so a repeat updates the comment.
func (c Client) PostRetro(ctx context.Context, repo string, number int, sha, text string) error {
	ticket, err := c.RetroTicket(ctx, repo)
	if err != nil {
		return err
	}

	marker := workflow.RetroMarker(number, sha)
	body := workflow.Truncate(fmt.Sprintf("%s\n## Retro: #%d\n\n%s", marker, number, text),
		workflow.CommentLimitChars)

	comments, err := Paged[workflow.Item](ctx, c, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, ticket))
	if err != nil {
		return err
	}

	if mine := workflow.StartsWith(comments, marker); len(mine) > 0 {
		return c.Call(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/comments/%d", repo, mine[0].ID),
			bodyRequest{Body: body}, nil)
	}

	return c.Comment(ctx, repo, ticket, body)
}

// PostReview posts a native pull request review on a commit. An approval GitHub refuses (the App
// authored the pull request) is posted as a comment review instead.
func (c Client) PostReview(ctx context.Context, repo string, pr int, sha, body string, approve bool) error {
	path := fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, pr)
	if approve {
		err := c.Call(ctx, http.MethodPost, path, reviewRequest{CommitID: sha, Body: body, Event: "APPROVE"}, nil)
		if err == nil {
			return nil
		}

		if !isStatus(err, http.StatusUnprocessableEntity) {
			return err
		}

		body += "\n\n_No high or medium findings. GitHub rejected the approval because " +
			"the review agent is also the pull request's author._"
	}

	return c.Call(ctx, http.MethodPost, path, reviewRequest{CommitID: sha, Body: body, Event: "COMMENT"}, nil)
}
