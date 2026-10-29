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
	var body io.Reader
	if in != nil {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(in); err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		body = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer func() {
		// The body is fully read or abandoned on error; a close failure changes nothing for the caller.
		_ = resp.Body.Close()
	}()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("github %s %s: read: %w", method, path, err)
	}
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("github %s %s: response over %d bytes", method, path, maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &HTTPError{Method: method, Path: path, Code: resp.StatusCode}
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("github %s %s: decode: %w", method, path, err)
	}
	return nil
}

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
		"state": {"open"}, "labels": {label}, "sort": {"created"},
		"direction": {"asc"}, "per_page": {fmt.Sprint(IssuesPerPage)},
	}
	var issues []workflow.Issue
	err := c.Call(ctx, http.MethodGet, "/repos/"+repo+"/issues?"+q.Encode(), nil, &issues)
	return issues, err
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
		map[string]string{"body": text}, nil)
}

// addLabel adds one label to an issue.
func (c Client) addLabel(ctx context.Context, repo string, number int, label string) error {
	return c.Call(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/labels", repo, number),
		map[string][]string{"labels": {label}}, nil)
}

// removeLabel removes one label from an issue.
func (c Client) removeLabel(ctx context.Context, repo string, number int, label string) error {
	return c.Call(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/issues/%d/labels/%s", repo, number, url.PathEscape(label)), nil, nil)
}

// Claim swaps src for dst. It is false when src is already gone: someone else claimed the issue.
func (c Client) Claim(ctx context.Context, repo string, number int, src, dst string) (bool, error) {
	if err := c.removeLabel(ctx, repo, number, src); err != nil {
		if isStatus(err, http.StatusNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, c.addLabel(ctx, repo, number, dst)
}

// Finish swaps src for dst and records the outcome in a comment.
func (c Client) Finish(ctx context.Context, repo string, number int, src, dst, text string) error {
	if err := c.removeLabel(ctx, repo, number, src); err != nil && !isStatus(err, http.StatusNotFound) {
		// A 404 means a person already removed it: fine, still record the outcome.
		return err
	}
	if err := c.addLabel(ctx, repo, number, dst); err != nil {
		return err
	}
	return c.Comment(ctx, repo, number, text)
}

// PullRequest is the part of a pull request foreman reads.
type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

// ExistingPR finds the open pull request for a branch, or nil.
func (c Client) ExistingPR(ctx context.Context, repo, branch string) (*PullRequest, error) {
	owner, _, _ := strings.Cut(repo, "/")
	q := url.Values{"state": {"open"}, "head": {owner + ":" + branch}}
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
	err := c.Call(ctx, http.MethodPost, "/repos/"+repo+"/pulls", map[string]string{
		"title": title, "head": head, "base": base, "body": body,
	}, &pr)
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
	err = c.Call(ctx, http.MethodPost, "/repos/"+repo+"/issues", map[string]any{
		"title": workflow.RetroTitle, "labels": []string{workflow.LabelRetro},
		"body": "One comment per agent implementation run, from the retro skill.",
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
	body := workflow.Truncate(fmt.Sprintf("%s\n## Retro: #%d\n\n%s", marker, number, text), workflow.CommentLimitChars)
	comments, err := Paged[workflow.Item](ctx, c, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, ticket))
	if err != nil {
		return err
	}
	if mine := workflow.StartsWith(comments, marker); len(mine) > 0 {
		return c.Call(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/comments/%d", repo, mine[0].ID),
			map[string]string{"body": body}, nil)
	}
	return c.Comment(ctx, repo, ticket, body)
}

// PostReview posts a native pull request review on a commit. An approval GitHub refuses (the App
// authored the pull request) is posted as a comment review instead.
func (c Client) PostReview(ctx context.Context, repo string, pr int, sha, body string, approve bool) error {
	path := fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, pr)
	if approve {
		err := c.Call(ctx, http.MethodPost, path, map[string]string{"commit_id": sha, "body": body, "event": "APPROVE"}, nil)
		if err == nil {
			return nil
		}
		if !isStatus(err, http.StatusUnprocessableEntity) {
			return err
		}
		body += "\n\n_No high or medium findings. GitHub rejected the approval because " +
			"the review agent is also the pull request's author._"
	}
	return c.Call(ctx, http.MethodPost, path, map[string]string{"commit_id": sha, "body": body, "event": "COMMENT"}, nil)
}
