package ghtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Emulator settings.
const (
	// AdminToken owns the test repositories and drives setup through the emulator's API.
	AdminToken = "admin-token"
	// defaultBranch is the branch auto_init gives every test repository.
	defaultBranch = "main"
	// readyTimeout bounds how long a start waits for the emulator to answer.
	readyTimeout = 15 * time.Second
	// readyPoll is the pause between readiness probes.
	readyPoll = 50 * time.Millisecond
	// maxResponseBytes bounds one setup response.
	maxResponseBytes = 1 << 20
	// bodyField is the JSON key for a comment, review or pull request body.
	bodyField = "body"
	// titleField is the JSON key for an issue or pull request title.
	titleField = "title"
)

// StatusError is a non-2xx answer from the emulator.
type StatusError struct {
	// Method and Path name the call.
	Method, Path string
	// Code is the HTTP status.
	Code int
	// Body is the answer, for diagnosis.
	Body string
}

// Error describes the failed call and the answer.
func (e *StatusError) Error() string {
	return fmt.Sprintf("emulate %s %s: HTTP %d: %s", e.Method, e.Path, e.Code, strings.TrimSpace(e.Body))
}

// PullRequest is the part of an emulator pull request the tests use.
type PullRequest struct {
	// Number is the pull request number, which shares the repository's issue sequence.
	Number int
	// HTMLURL is the advertised web URL.
	HTMLURL string `json:"html_url"`
}

// Issue is the part of an emulator issue the tests read back.
type Issue struct {
	// Number is the issue number.
	Number int `json:"number"`
	// State is "open" or "closed".
	State string `json:"state"`
	// Title is the issue title.
	Title string `json:"title"`
	// Labels are the label names the issue carries.
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	// User is the issue's author.
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// Emulate is a running GitHub emulator.
type Emulate struct {
	base   string
	cmd    *exec.Cmd
	output *logBuffer
	hc     *http.Client
}

// StartEmulate starts the GitHub emulator seeded from seedPath and waits for it to answer.
func StartEmulate(ctx context.Context, seedPath string) (*Emulate, error) {
	port, err := freePort(ctx)
	if err != nil {
		return nil, err
	}

	base := "http://127.0.0.1:" + strconv.Itoa(port)
	output := &logBuffer{}

	cmd := exec.CommandContext(ctx, "emulate", "start", //nolint:gosec // fixed command; only the port and seed path vary
		"--service", "github", "--port", strconv.Itoa(port), "--seed", seedPath, "--base-url", "http://github")
	cmd.Stdout = output
	cmd.Stderr = output

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start emulate (run the tests from `nix develop`): %w", err)
	}

	e := &Emulate{base: base, cmd: cmd, output: output, hc: &http.Client{Timeout: proxyTimeout}}
	if err := e.waitReady(ctx, base); err != nil {
		e.Close()

		return nil, err
	}

	return e, nil
}

// URL is the emulator's API root.
func (e *Emulate) URL() string { return e.base }

// Close stops the emulator. It is safe to call after a failed start.
func (e *Emulate) Close() {
	if e.cmd.Process != nil {
		// A failed kill means the process already exited; Wait below reaps it either way.
		_ = e.cmd.Process.Kill()
	}

	// The emulator is gone; its exit status carries nothing the tests need.
	_ = e.cmd.Wait()
}

// waitReady probes the emulator until it answers or the deadline passes.
func (e *Emulate) waitReady(ctx context.Context, base string) error {
	deadline := time.Now().Add(readyTimeout)

	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
		if err != nil {
			return fmt.Errorf("probe emulate: %w", err)
		}

		resp, err := e.hc.Do(req)
		if err == nil {
			// The answer itself does not matter: something is listening.
			_ = resp.Body.Close()

			return nil
		}

		time.Sleep(readyPoll)
	}

	return fmt.Errorf("emulate did not answer within %s: %s", readyTimeout, e.output.String())
}

// Do calls the emulator's API directly with token. Setup bypasses the proxy, so it is not recorded.
func (e *Emulate) Do(ctx context.Context, method, path, token string, in, out any) error {
	req, err := e.request(ctx, method, path, token, in)
	if err != nil {
		return err
	}

	resp, err := e.hc.Do(req)
	if err != nil {
		return fmt.Errorf("emulate %s %s: %w", method, path, err)
	}

	defer func() {
		// The answer is fully read or abandoned; a close failure changes nothing for the caller.
		_ = resp.Body.Close()
	}()

	return decodeResponse(resp, method, path, out)
}

// request builds an authenticated emulator call with in, when given, as its JSON body.
func (e *Emulate) request(ctx context.Context, method, path, token string, in any) (*http.Request, error) {
	var body io.Reader

	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("encode %s %s: %w", method, path, err)
		}

		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, e.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("build %s %s: %w", method, path, err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	return req, nil
}

// decodeResponse checks the status and decodes the answer into out, when given.
func decodeResponse(resp *http.Response, method, path string, out any) error {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("emulate %s %s: read: %w", method, path, err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return &StatusError{Method: method, Path: path, Code: resp.StatusCode, Body: string(raw)}
	}

	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("emulate %s %s: decode: %w", method, path, err)
	}

	return nil
}

// ResetRepo replaces repo with an empty, auto-initialized repository.
func (e *Emulate) ResetRepo(ctx context.Context, repo string) error {
	if err := e.DeleteRepo(ctx, repo); err != nil {
		return err
	}

	return e.CreateRepo(ctx, repo)
}

// CreateRepo creates repo under the admin token's user.
func (e *Emulate) CreateRepo(ctx context.Context, repo string) error {
	return e.Do(ctx, http.MethodPost, "/user/repos", AdminToken,
		map[string]any{"name": repoName(repo), "auto_init": true}, nil)
}

// EnsureRepo creates repo only when it does not exist.
func (e *Emulate) EnsureRepo(ctx context.Context, repo string) error {
	err := e.Do(ctx, http.MethodGet, "/repos/"+repo, AdminToken, nil, nil)
	if err == nil {
		return nil
	}

	var status *StatusError
	if errors.As(err, &status) && status.Code == http.StatusNotFound {
		return e.CreateRepo(ctx, repo)
	}

	return err
}

// DeleteRepo deletes repo; a repository that is already gone is not an error.
func (e *Emulate) DeleteRepo(ctx context.Context, repo string) error {
	err := e.Do(ctx, http.MethodDelete, "/repos/"+repo, AdminToken, nil, nil)

	var status *StatusError
	if errors.As(err, &status) && status.Code == http.StatusNotFound {
		return nil
	}

	return err
}

// CreateIssue creates an issue with labels, returning its number. Missing labels are created.
func (e *Emulate) CreateIssue(ctx context.Context, repo string, labels []string, title, text string) (int, error) {
	var created struct {
		Number int
	}

	in := map[string]any{titleField: title, bodyField: text}
	if len(labels) > 0 {
		in["labels"] = labels
	}

	if err := e.Do(ctx, http.MethodPost, "/repos/"+repo+"/issues", AdminToken, in, &created); err != nil {
		return 0, err
	}

	return created.Number, nil
}

// CreateIssueAs creates an issue authored by the user behind token, returning its number.
func (e *Emulate) CreateIssueAs(
	ctx context.Context, repo, token string, labels []string, title, text string,
) (int, error) {
	var created struct {
		Number int
	}

	in := map[string]any{titleField: title, bodyField: text}
	if len(labels) > 0 {
		in["labels"] = labels
	}

	if err := e.Do(ctx, http.MethodPost, "/repos/"+repo+"/issues", token, in, &created); err != nil {
		return 0, err
	}

	return created.Number, nil
}

// CommentAs posts a comment on an issue as the user behind token.
func (e *Emulate) CommentAs(ctx context.Context, repo string, number int, token, text string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number)

	return e.Do(ctx, http.MethodPost, path, token, map[string]string{bodyField: text}, nil)
}

// SetPermission grants a user a permission on repo, as the repository's admin.
func (e *Emulate) SetPermission(ctx context.Context, repo, login, permission string) error {
	path := "/repos/" + repo + "/collaborators/" + url.PathEscape(login)

	return e.Do(ctx, http.MethodPut, path, AdminToken, map[string]string{"permission": permission}, nil)
}

// RemoveLabel removes one label from an issue.
func (e *Emulate) RemoveLabel(ctx context.Context, repo string, number int, label string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/labels/%s", repo, number, url.PathEscape(label))

	return e.Do(ctx, http.MethodDelete, path, AdminToken, nil, nil)
}

// CloseIssue closes an issue.
func (e *Emulate) CloseIssue(ctx context.Context, repo string, number int) error {
	return e.Do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", repo, number),
		AdminToken, map[string]string{"state": "closed"}, nil)
}

// Issue reads an issue back.
func (e *Emulate) Issue(ctx context.Context, repo string, number int) (Issue, error) {
	var issue Issue

	err := e.Do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repo, number), AdminToken, nil, &issue)

	return issue, err
}

// Labels lists the repository's label names.
func (e *Emulate) Labels(ctx context.Context, repo string) ([]string, error) {
	var labels []struct {
		Name string `json:"name"`
	}

	if err := e.Do(ctx, http.MethodGet, "/repos/"+repo+"/labels", AdminToken, nil, &labels); err != nil {
		return nil, err
	}

	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, l.Name)
	}

	return out, nil
}

// HasIssue reports whether issue number exists in repo.
func (e *Emulate) HasIssue(ctx context.Context, repo string, number int) (bool, error) {
	err := e.Do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repo, number), AdminToken, nil, nil)
	if err == nil {
		return true, nil
	}

	var status *StatusError
	if errors.As(err, &status) && status.Code == http.StatusNotFound {
		return false, nil
	}

	return false, err
}

// FindIssueByLabel returns the first open issue carrying label, or 0 when there is none.
func (e *Emulate) FindIssueByLabel(ctx context.Context, repo, label string) (int, error) {
	var issues []struct {
		Number int
	}

	path := "/repos/" + repo + "/issues?state=open&labels=" + url.QueryEscape(label)
	if err := e.Do(ctx, http.MethodGet, path, AdminToken, nil, &issues); err != nil {
		return 0, err
	}

	if len(issues) == 0 {
		return 0, nil
	}

	return issues[0].Number, nil
}

// PullRequests lists the open pull requests of repo.
func (e *Emulate) PullRequests(ctx context.Context, repo string) ([]PullRequest, error) {
	var prs []PullRequest
	if err := e.Do(ctx, http.MethodGet, "/repos/"+repo+"/pulls?state=open", AdminToken, nil, &prs); err != nil {
		return nil, err
	}

	return prs, nil
}

// CreatePR opens a pull request from head onto the default branch, returning its number and web URL.
func (e *Emulate) CreatePR(ctx context.Context, repo, head, title, text string) (PullRequest, error) {
	var created PullRequest

	in := map[string]any{titleField: title, "head": head, "base": defaultBranch, bodyField: text}
	if err := e.Do(ctx, http.MethodPost, "/repos/"+repo+"/pulls", AdminToken, in, &created); err != nil {
		return PullRequest{}, err
	}

	return created, nil
}

// CreateReview adds a comment review to pr, for seeding earlier rounds.
func (e *Emulate) CreateReview(ctx context.Context, repo string, pr int, text string) error {
	in := map[string]any{bodyField: text, "event": "COMMENT"}
	path := fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, pr)

	return e.Do(ctx, http.MethodPost, path, AdminToken, in, nil)
}

// repoName is the final path element of owner/name.
func repoName(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}

	return repo
}

// freePort finds an unused loopback port.
func freePort(ctx context.Context) (int, error) {
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find a port for emulate: %w", err)
	}

	defer func() {
		// The port is free again either way; a close failure leaves nothing to check.
		_ = l.Close()
	}()

	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("find a port for emulate: not a TCP address")
	}

	return addr.Port, nil
}

// logBuffer is a concurrency-safe command output buffer.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Write appends p to the buffer.
func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	n, err := l.b.Write(p)
	if err != nil {
		return n, fmt.Errorf("buffer: %w", err)
	}

	return n, nil
}

// String is the output so far.
func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.String()
}
