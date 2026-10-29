package foreman

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/nateinaction/factory/foreman/internal/fakegh"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// queue registers the issues listing: each label maps to the issues the fake returns for it.
func (h *harness) queue(byLabel map[string][]map[string]any) {
	h.gh.Handle("GET /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		label := r.URL.Query().Get("labels")
		list := byLabel[label]
		if list == nil {
			list = []map[string]any{}
		}
		fakegh.WriteJSON(w, list)
	})
}

// roles are the agent roles the harness's config lists.
func (h *harness) roles() []workflow.Role {
	return []workflow.Role{workflow.Implementer, workflow.Reviewer}
}

// queueRepo registers the issues listing of one repository.
func (h *harness) queueRepo(repo string, byLabel map[string][]map[string]any) {
	h.gh.Handle("GET /repos/"+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
		list := byLabel[r.URL.Query().Get("labels")]
		if list == nil {
			list = []map[string]any{}
		}
		fakegh.WriteJSON(w, list)
	})
}

// acceptWrites lets label and comment writes on issue n succeed.
func (h *harness) acceptWrites(n string) {
	h.gh.Status("DELETE /repos/o/r/issues/"+n+"/labels/{label}", http.StatusOK)
	h.gh.Status("POST /repos/o/r/issues/"+n+"/labels", http.StatusOK)
	h.gh.Status("POST /repos/o/r/issues/"+n+"/comments", http.StatusCreated)
}

// TestPollClaimsReviewsBeforeWork pins: poll claims reviews before work.
func TestPollClaimsReviewsBeforeWork(t *testing.T) {
	h := newHarness(t)
	h.queue(map[string][]map[string]any{
		workflow.LabelReviewReady: {{"number": 12}},
		workflow.LabelReady:       {{"number": 3}},
	})
	h.acceptWrites("12")
	code, err := Poll(context.Background(), h.deps, h.roles())
	if err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	want := []string{
		"GET /repos/o/r/issues",
		"DELETE /repos/o/r/issues/12/labels/ready-for-agent-review",
		"POST /repos/o/r/issues/12/labels",
		"POST /repos/o/r/issues/12/comments",
	}
	if got := h.gh.Summary(); !slices.Equal(got, want) {
		t.Errorf("got %v", got)
	}
	if got := h.gh.Bodies("POST", "/repos/o/r/issues/12/labels"); !strings.Contains(got[0], workflow.LabelReviewing) {
		t.Errorf("labels %v", got)
	}
	if got := h.gh.Bodies("POST", "/repos/o/r/issues/12/comments"); !strings.Contains(got[0], "Picked up for review by the factory agent.") {
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
		workflow.LabelReady: {{"number": 4, "pull_request": map[string]any{}}, {"number": 5}},
	})
	h.acceptWrites("5")
	if code, err := Poll(context.Background(), h.deps, h.roles()); err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	if got := h.gh.Bodies("POST", "/repos/o/r/issues/5/labels"); !strings.Contains(got[0], workflow.LabelWorking) {
		t.Errorf("labels %v", got)
	}
	if got := h.gh.Bodies("POST", "/repos/o/r/issues/5/comments"); !strings.Contains(got[0], "Picked up by the factory agent.") {
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
	code, err := Poll(context.Background(), h.deps, h.roles())
	if err != nil || code != 0 || h.out.String() != "nothing ready\n" {
		t.Fatalf("%d %v %q", code, err, h.out.String())
	}
	for _, s := range h.gh.Summary() {
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
	h.queue(map[string][]map[string]any{workflow.LabelReady: {{"number": 5}}})
	h.gh.Status("DELETE /repos/o/r/issues/5/labels/{label}", http.StatusNotFound)
	code, err := Poll(context.Background(), h.deps, h.roles())
	if err != nil || code != 0 || h.out.String() != "issue 5 was claimed by someone else\n" {
		t.Fatalf("%d %v %q", code, err, h.out.String())
	}
	if len(h.runner.find("enqueue")) != 0 || len(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")) != 0 {
		t.Error("acted on an issue it did not claim")
	}
}

// TestPollEnqueueFailureFailsTheIssue pins: poll enqueue failure fails the issue.
func TestPollEnqueueFailureFailsTheIssue(t *testing.T) {
	h := newHarness(t)
	h.queue(map[string][]map[string]any{workflow.LabelReady: {{"number": 5}}})
	h.acceptWrites("5")
	base := h.runner.fn
	h.runner.fn = func(c proc.Cmd) (string, error) {
		if strings.Contains(joined(c), "enqueue") {
			return "", errors.New("dagu down")
		}
		return base(c)
	}
	code, err := Poll(context.Background(), h.deps, h.roles())
	if err != nil || code != 1 {
		t.Fatalf("%d %v", code, err)
	}
	if !strings.Contains(h.errb.String(), "dagu down") {
		t.Errorf("stderr %q", h.errb.String())
	}
	labels := h.gh.Bodies("POST", "/repos/o/r/issues/5/labels")
	if len(labels) != 2 || !strings.Contains(labels[1], workflow.LabelFailed) {
		t.Errorf("labels %v", labels)
	}
	if got := h.gh.Summary(); !slices.Contains(got, "DELETE /repos/o/r/issues/5/labels/agent-working") {
		t.Errorf("did not release the claim: %v", got)
	}
	if got := h.gh.Bodies("POST", "/repos/o/r/issues/5/comments"); !strings.Contains(got[1], "Could not queue the agent run") {
		t.Errorf("comments %v", got)
	}
}

// TestPollMintsEachTokenFromTheAppThatOwnsIt pins: a claim is made with the owning agent role's repo-scoped token.
func TestPollMintsEachTokenFromTheAppThatOwnsIt(t *testing.T) {
	h := newHarness(t)
	h.queue(map[string][]map[string]any{workflow.LabelReviewReady: {{"number": 12}}})
	h.acceptWrites("12")
	if _, err := Poll(context.Background(), h.deps, h.roles()); err != nil {
		t.Fatal(err)
	}
	mint := h.runner.find("app-token", " token ")
	if len(mint) != 1 || !slices.Equal(mint[0].Argv, []string{
		"/usr/local/bin/github-app-token", "token", "--app", "reviewer", "--repo", "o/r",
	}) || !mint[0].Capture {
		t.Errorf("mint %v", mint)
	}
	for _, r := range h.gh.Calls() {
		if got := r.Header.Get("Authorization"); got != "Bearer minted-token-reviewer" {
			t.Errorf("%s %s used %q", r.Method, r.Path, got)
		}
	}
}

// TestPollListsEachAppsInstallations pins: the poll asks every configured App where it is installed.
func TestPollListsEachAppsInstallations(t *testing.T) {
	h := newHarness(t)
	h.queue(nil)
	if _, err := Poll(context.Background(), h.deps, h.roles()); err != nil {
		t.Fatal(err)
	}
	var apps []string
	for _, c := range h.runner.find("app-token", " repositories ") {
		apps = append(apps, appOf(c))
	}
	if !slices.Equal(apps, []string{"implementer", "reviewer"}) {
		t.Errorf("apps %v", apps)
	}
}

// TestPollIgnoresARepositoryWithNoAppInstalled pins: a repository no factory App is installed on is not polled.
func TestPollIgnoresARepositoryWithNoAppInstalled(t *testing.T) {
	h := newHarness(t)
	h.installed = map[workflow.Role][]string{}
	code, err := Poll(context.Background(), h.deps, h.roles())
	if err != nil || code != 0 || h.out.String() != "nothing ready\n" {
		t.Fatalf("%d %v %q", code, err, h.out.String())
	}
	if len(h.gh.Calls()) != 0 || len(h.runner.find(" token ")) != 0 {
		t.Errorf("worked an unassigned repository: %v", h.gh.Summary())
	}
}

// TestPollAnyInstalledAppAssignsTheRepository pins: each repository is polled for the agent roles installed on it.
func TestPollAnyInstalledAppAssignsTheRepository(t *testing.T) {
	h := newHarness(t)
	h.installed = map[workflow.Role][]string{workflow.Implementer: {"o/a"}, workflow.Reviewer: {"o/b", "o/a"}}
	h.queueRepo("o/a", map[string][]map[string]any{})
	h.queueRepo("o/b", map[string][]map[string]any{})
	if _, err := Poll(context.Background(), h.deps, h.roles()); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /repos/o/a/issues", "GET /repos/o/b/issues", "GET /repos/o/a/issues"}
	if got := h.gh.Summary(); !slices.Equal(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

// TestPollWorksOnlyTheAgentRolesInstalledOnARepository pins: others' issues wait, silently.
func TestPollWorksOnlyTheAgentRolesInstalledOnARepository(t *testing.T) {
	h := newHarness(t)
	h.installed = map[workflow.Role][]string{workflow.Implementer: {"o/r"}}
	h.queue(map[string][]map[string]any{
		workflow.LabelReviewReady: {{"number": 12}},
		workflow.LabelReady:       {{"number": 3}},
	})
	h.acceptWrites("3")
	if code, err := Poll(context.Background(), h.deps, h.roles()); err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	want := []string{
		"GET /repos/o/r/issues",
		"DELETE /repos/o/r/issues/3/labels/ready-for-agent",
		"POST /repos/o/r/issues/3/labels",
		"POST /repos/o/r/issues/3/comments",
	}
	if got := h.gh.Summary(); !slices.Equal(got, want) {
		t.Errorf("got %v", got)
	}
}

// TestPollWaitsQuietlyWhenTheOwningAppIsMissing pins: no comment and no label for a missing agent role.
func TestPollWaitsQuietlyWhenTheOwningAppIsMissing(t *testing.T) {
	h := newHarness(t)
	h.installed = map[workflow.Role][]string{workflow.Implementer: {"o/r"}}
	h.queue(map[string][]map[string]any{workflow.LabelReviewReady: {{"number": 12}}})
	code, err := Poll(context.Background(), h.deps, h.roles())
	if err != nil || code != 0 || h.out.String() != "nothing ready\n" {
		t.Fatalf("%d %v %q", code, err, h.out.String())
	}
	for _, s := range h.gh.Summary() {
		if !strings.HasPrefix(s, "GET ") {
			t.Errorf("wrote %s", s)
		}
	}
}

// failMint makes minting fail for the App of role, passing every other command on.
func (h *harness) failMint(role workflow.Role, onlyToken bool) {
	base := h.runner.fn
	h.runner.fn = func(c proc.Cmd) (string, error) {
		if strings.Contains(joined(c), "app-token") && appOf(c) == string(role) &&
			(!onlyToken || strings.Contains(joined(c), " token ")) {
			return "", &proc.ExitError{Program: "github-app-token", Code: 1}
		}
		return base(c)
	}
}

// TestPollTreatsARepositoryWhoseTokenCannotBeMintedAsUnassigned pins: fail closed per agent role and repository.
func TestPollTreatsARepositoryWhoseTokenCannotBeMintedAsUnassigned(t *testing.T) {
	h := newHarness(t)
	h.failMint(workflow.Reviewer, true)
	h.queue(map[string][]map[string]any{
		workflow.LabelReviewReady: {{"number": 12}},
		workflow.LabelReady:       {{"number": 3}},
	})
	h.acceptWrites("3")
	if code, err := Poll(context.Background(), h.deps, h.roles()); err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	if got := h.gh.Summary(); !slices.Contains(got, "DELETE /repos/o/r/issues/3/labels/ready-for-agent") || len(got) != 4 {
		t.Errorf("got %v", got)
	}
	if !strings.Contains(h.errb.String(), "reviewer") {
		t.Errorf("stderr %q", h.errb.String())
	}
}

// TestPollTreatsAnAppThatCannotListItsInstallationsAsInstalledNowhere pins: fail closed per App.
func TestPollTreatsAnAppThatCannotListItsInstallationsAsInstalledNowhere(t *testing.T) {
	h := newHarness(t)
	h.installed = map[workflow.Role][]string{workflow.Reviewer: {"o/r"}}
	h.failMint(workflow.Reviewer, false)
	h.queue(map[string][]map[string]any{workflow.LabelReviewReady: {{"number": 12}}})
	code, err := Poll(context.Background(), h.deps, h.roles())
	if err != nil || code != 0 || h.out.String() != "nothing ready\n" {
		t.Fatalf("%d %v %q", code, err, h.out.String())
	}
	if len(h.gh.Calls()) != 0 || len(h.runner.find("--app reviewer", " token ")) != 0 {
		t.Errorf("acted for an App that could not list its installations: %v", h.gh.Summary())
	}
}

// TestPollIgnoresMalformedRepositoryNames pins: only owner/name lines count as installations.
func TestPollIgnoresMalformedRepositoryNames(t *testing.T) {
	h := newHarness(t)
	h.installed = map[workflow.Role][]string{workflow.Implementer: {"../etc", "not a repo", ""}}
	if code, err := Poll(context.Background(), h.deps, h.roles()); err != nil || code != 0 || len(h.gh.Calls()) != 0 {
		t.Fatalf("%d %v %v", code, err, h.gh.Summary())
	}
}

// TestPollListsQueuesWithEscapedLabels pins: poll lists queues with escaped labels.
func TestPollListsQueuesWithEscapedLabels(t *testing.T) {
	h := newHarness(t)
	h.queue(nil)
	if _, err := Poll(context.Background(), h.deps, h.roles()); err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, r := range h.gh.Calls() {
		v, _ := url.ParseQuery(r.Query)
		labels = append(labels, v.Get("labels"))
	}
	if !slices.Equal(labels, []string{workflow.LabelReviewReady, workflow.LabelReady}) {
		t.Errorf("labels %v", labels)
	}
}
