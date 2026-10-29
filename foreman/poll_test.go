package foreman

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// queue registers the issues listing: each label maps to the issues the fake returns for it.
func (h *harness) queue(byLabel map[string][]map[string]any) {
	h.gh.handle("GET /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		label := r.URL.Query().Get("labels")
		list := byLabel[label]
		if list == nil {
			list = []map[string]any{}
		}
		writeJSON(w, list)
	})
}

// acceptWrites lets label and comment writes on issue n succeed.
func (h *harness) acceptWrites(n string) {
	h.gh.status("DELETE /repos/o/r/issues/"+n+"/labels/{label}", http.StatusOK)
	h.gh.status("POST /repos/o/r/issues/"+n+"/labels", http.StatusOK)
	h.gh.status("POST /repos/o/r/issues/"+n+"/comments", http.StatusCreated)
}

// TestPollClaimsReviewsBeforeWork pins: poll claims reviews before work.
func TestPollClaimsReviewsBeforeWork(t *testing.T) {
	h := newHarness(t)
	h.queue(map[string][]map[string]any{
		LabelReviewReady: {{"number": 12}},
		LabelReady:       {{"number": 3}},
	})
	h.acceptWrites("12")
	code, err := Poll(context.Background(), h.deps, "o/r")
	if err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	want := []string{
		"GET /repos/o/r/issues",
		"DELETE /repos/o/r/issues/12/labels/ready-for-agent-review",
		"POST /repos/o/r/issues/12/labels",
		"POST /repos/o/r/issues/12/comments",
	}
	if got := h.gh.summary(); !slices.Equal(got, want) {
		t.Errorf("got %v", got)
	}
	if got := h.gh.bodies("POST", "/repos/o/r/issues/12/labels"); !strings.Contains(got[0], LabelReviewing) {
		t.Errorf("labels %v", got)
	}
	if got := h.gh.bodies("POST", "/repos/o/r/issues/12/comments"); !strings.Contains(got[0], "Picked up for review by the factory agent.") {
		t.Errorf("comment %v", got)
	}
	enq := h.runner.find("dagu", "enqueue")
	if len(enq) != 1 || !slices.Equal(enq[0].Argv, []string{
		"/usr/local/bin/dagu", "enqueue", "factory-reviewer", "--", "REPO=o/r", "ISSUE=12",
	}) {
		t.Errorf("enqueue %v", enq)
	}
}

// TestPollClaimsWorkWhenNoReviewWaits pins: poll claims work when no review waits.
func TestPollClaimsWorkWhenNoReviewWaits(t *testing.T) {
	h := newHarness(t)
	h.queue(map[string][]map[string]any{
		LabelReady: {{"number": 4, "pull_request": map[string]any{}}, {"number": 5}},
	})
	h.acceptWrites("5")
	if code, err := Poll(context.Background(), h.deps, "o/r"); err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	if got := h.gh.bodies("POST", "/repos/o/r/issues/5/labels"); !strings.Contains(got[0], LabelWorking) {
		t.Errorf("labels %v", got)
	}
	if got := h.gh.bodies("POST", "/repos/o/r/issues/5/comments"); !strings.Contains(got[0], "Picked up by the factory agent.") {
		t.Errorf("comment %v", got)
	}
	enq := h.runner.find("enqueue")
	if len(enq) != 1 || !slices.Contains(enq[0].Argv, "factory-implementer") || !slices.Contains(enq[0].Argv, "ISSUE=5") {
		t.Errorf("enqueue %v", enq)
	}
}

// TestPollWithNothingReadyWritesNothing pins: poll with nothing ready writes nothing.
func TestPollWithNothingReadyWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.queue(nil)
	code, err := Poll(context.Background(), h.deps, "o/r")
	if err != nil || code != 0 || h.out.String() != "nothing ready\n" {
		t.Fatalf("%d %v %q", code, err, h.out.String())
	}
	for _, s := range h.gh.summary() {
		if !strings.HasPrefix(s, "GET ") {
			t.Errorf("wrote %s", s)
		}
	}
	if len(h.runner.find("enqueue")) != 0 {
		t.Error("enqueued with nothing ready")
	}
}

// TestPollLosingTheClaimStopsQuietly pins: poll losing the claim stops quietly.
func TestPollLosingTheClaimStopsQuietly(t *testing.T) {
	h := newHarness(t)
	h.queue(map[string][]map[string]any{LabelReady: {{"number": 5}}})
	h.gh.status("DELETE /repos/o/r/issues/5/labels/{label}", http.StatusNotFound)
	code, err := Poll(context.Background(), h.deps, "o/r")
	if err != nil || code != 0 || h.out.String() != "issue 5 was claimed by someone else\n" {
		t.Fatalf("%d %v %q", code, err, h.out.String())
	}
	if len(h.runner.find("enqueue")) != 0 || len(h.gh.bodies("POST", "/repos/o/r/issues/5/comments")) != 0 {
		t.Error("acted on an issue it did not claim")
	}
}

// TestPollEnqueueFailureFailsTheIssue pins: poll enqueue failure fails the issue.
func TestPollEnqueueFailureFailsTheIssue(t *testing.T) {
	h := newHarness(t)
	h.queue(map[string][]map[string]any{LabelReady: {{"number": 5}}})
	h.acceptWrites("5")
	base := h.runner.fn
	h.runner.fn = func(c Cmd) (string, error) {
		if strings.Contains(joined(c), "enqueue") {
			return "", errors.New("dagu down")
		}
		return base(c)
	}
	code, err := Poll(context.Background(), h.deps, "o/r")
	if err != nil || code != 1 {
		t.Fatalf("%d %v", code, err)
	}
	if !strings.Contains(h.errb.String(), "dagu down") {
		t.Errorf("stderr %q", h.errb.String())
	}
	labels := h.gh.bodies("POST", "/repos/o/r/issues/5/labels")
	if len(labels) != 2 || !strings.Contains(labels[1], LabelFailed) {
		t.Errorf("labels %v", labels)
	}
	if got := h.gh.summary(); !slices.Contains(got, "DELETE /repos/o/r/issues/5/labels/agent-working") {
		t.Errorf("did not release the claim: %v", got)
	}
	if got := h.gh.bodies("POST", "/repos/o/r/issues/5/comments"); !strings.Contains(got[1], "Could not queue the agent run") {
		t.Errorf("comments %v", got)
	}
}

// TestPollMintsTheRepoScopedToken pins: poll mints the repo scoped token.
func TestPollMintsTheRepoScopedToken(t *testing.T) {
	h := newHarness(t)
	h.queue(nil)
	if _, err := Poll(context.Background(), h.deps, "o/r"); err != nil {
		t.Fatal(err)
	}
	mint := h.runner.find("github-app-token")
	if len(mint) != 1 || !slices.Equal(mint[0].Argv, []string{"/usr/local/bin/github-app-token", "token", "--repo", "r"}) || !mint[0].Capture {
		t.Errorf("mint %v", mint)
	}
	if got := h.gh.calls()[0].Header.Get("Authorization"); got != "Bearer minted-token" {
		t.Errorf("auth %q", got)
	}
}

// TestPollListsQueuesWithEscapedLabels pins: poll lists queues with escaped labels.
func TestPollListsQueuesWithEscapedLabels(t *testing.T) {
	h := newHarness(t)
	h.queue(nil)
	if _, err := Poll(context.Background(), h.deps, "o/r"); err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, r := range h.gh.calls() {
		v, _ := url.ParseQuery(r.Query)
		labels = append(labels, v.Get("labels"))
	}
	if !slices.Equal(labels, []string{LabelReviewReady, LabelReady}) {
		t.Errorf("labels %v", labels)
	}
}
