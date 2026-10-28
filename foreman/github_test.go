package foreman

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// testClient talks to the fake with a fixed token.
func testClient(f *fakeGH) Client {
	return NewClient(f.srv.URL, "secret-token", f.srv.Client())
}

// TestRequestsCarryForemanUserAgentAndAuth pins: requests carry foreman user agent and auth.
func TestRequestsCarryForemanUserAgentAndAuth(t *testing.T) {
	f := newFakeGH(t)
	f.json("GET /repos/o/r", map[string]string{"default_branch": "main"})
	if _, err := testClient(f).DefaultBranch(context.Background(), "o/r"); err != nil {
		t.Fatal(err)
	}
	h := f.calls()[0].Header
	if h.Get("User-Agent") != "foreman" {
		t.Errorf("User-Agent %q", h.Get("User-Agent"))
	}
	if h.Get("Authorization") != "Bearer secret-token" || h.Get("Accept") != "application/vnd.github+json" ||
		h.Get("X-GitHub-Api-Version") != "2022-11-28" {
		t.Errorf("headers %v", h)
	}
}

// TestErrorsNameTheCallButNeverTheToken pins: errors name the call but never the token.
func TestErrorsNameTheCallButNeverTheToken(t *testing.T) {
	f := newFakeGH(t)
	f.status("GET /repos/o/r", http.StatusForbidden)
	_, err := testClient(f).DefaultBranch(context.Background(), "o/r")
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("err %v", err)
	}
}

// TestPagedStopsAtShortPageAndAtMaxPages pins: paged stops at short page and at max pages.
func TestPagedStopsAtShortPageAndAtMaxPages(t *testing.T) {
	f := newFakeGH(t)
	full := make([]Item, PageSize)
	f.json("GET /full", full)
	items, err := Paged[Item](context.Background(), testClient(f), "/full")
	if err != nil || len(items) != MaxPages*PageSize {
		t.Fatalf("got %d items, %v", len(items), err)
	}
	if got := len(f.calls()); got != MaxPages {
		t.Errorf("fetched %d pages", got)
	}
	f2 := newFakeGH(t)
	f2.json("GET /short", []Item{{ID: 1}})
	items, err = Paged[Item](context.Background(), testClient(f2), "/short?x=1")
	if err != nil || len(items) != 1 || len(f2.calls()) != 1 {
		t.Errorf("got %v, %v", items, err)
	}
	if q := f2.calls()[0].Query; q != "x=1&per_page=100&page=1" {
		t.Errorf("query %q", q)
	}
}

// TestListLabeledAsksOldestFirst pins: list labeled asks oldest first.
func TestListLabeledAsksOldestFirst(t *testing.T) {
	f := newFakeGH(t)
	f.json("GET /repos/o/r/issues", []map[string]any{{"number": 3, "title": "t", "body": nil}})
	issues, err := testClient(f).ListLabeled(context.Background(), "o/r", "ready-for-agent")
	if err != nil || len(issues) != 1 || issues[0].Number != 3 {
		t.Fatalf("%v %v", issues, err)
	}
	if q := f.calls()[0].Query; q != "direction=asc&labels=ready-for-agent&per_page=20&sort=created&state=open" {
		t.Errorf("query %q", q)
	}
}

// TestClaimSwapsLabels pins: claim swaps labels.
func TestClaimSwapsLabels(t *testing.T) {
	f := newFakeGH(t)
	f.status("DELETE /repos/o/r/issues/5/labels/a", http.StatusOK)
	f.status("POST /repos/o/r/issues/5/labels", http.StatusOK)
	ok, err := testClient(f).Claim(context.Background(), "o/r", 5, "a", "b")
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if got := f.bodies("POST", "/repos/o/r/issues/5/labels"); !slices.Equal(got, []string{`{"labels":["b"]}` + "\n"}) && !slices.Equal(got, []string{`{"labels":["b"]}`}) {
		t.Errorf("bodies %q", got)
	}
}

// TestClaimFailsClosedWhenSomeoneElseGotThere pins: claim fails closed when someone else got there.
func TestClaimFailsClosedWhenSomeoneElseGotThere(t *testing.T) {
	f := newFakeGH(t)
	f.status("DELETE /repos/o/r/issues/5/labels/a", http.StatusNotFound)
	ok, err := testClient(f).Claim(context.Background(), "o/r", 5, "a", "b")
	if err != nil || ok {
		t.Fatalf("%v %v", ok, err)
	}
	if got := f.summary(); len(got) != 1 {
		t.Errorf("added a label after losing the claim: %v", got)
	}
}

// TestClaimPropagatesOtherErrors pins: claim propagates other errors.
func TestClaimPropagatesOtherErrors(t *testing.T) {
	f := newFakeGH(t)
	f.status("DELETE /repos/o/r/issues/5/labels/a", http.StatusInternalServerError)
	if ok, err := testClient(f).Claim(context.Background(), "o/r", 5, "a", "b"); err == nil || ok {
		t.Errorf("%v %v", ok, err)
	}
}

// TestFinishToleratesAlreadyRemovedLabelAndComments pins: finish tolerates already removed label and comments.
func TestFinishToleratesAlreadyRemovedLabelAndComments(t *testing.T) {
	f := newFakeGH(t)
	f.status("DELETE /repos/o/r/issues/5/labels/a", http.StatusNotFound)
	f.status("POST /repos/o/r/issues/5/labels", http.StatusOK)
	f.status("POST /repos/o/r/issues/5/comments", http.StatusCreated)
	if err := testClient(f).Finish(context.Background(), "o/r", 5, "a", "b", "done"); err != nil {
		t.Fatal(err)
	}
	want := []string{"DELETE /repos/o/r/issues/5/labels/a", "POST /repos/o/r/issues/5/labels", "POST /repos/o/r/issues/5/comments"}
	if got := f.summary(); !slices.Equal(got, want) {
		t.Errorf("got %v", got)
	}
}

// TestFinishPropagatesOtherDeleteErrors pins: finish propagates other delete errors.
func TestFinishPropagatesOtherDeleteErrors(t *testing.T) {
	f := newFakeGH(t)
	f.status("DELETE /repos/o/r/issues/5/labels/a", http.StatusBadGateway)
	if err := testClient(f).Finish(context.Background(), "o/r", 5, "a", "b", "x"); err == nil {
		t.Error("swallowed a 502")
	}
}

// TestExistingPR pins: existing p r.
func TestExistingPR(t *testing.T) {
	f := newFakeGH(t)
	f.json("GET /repos/o/r/pulls", []map[string]any{{"number": 8, "html_url": "http://pr/8"}})
	pr, err := testClient(f).ExistingPR(context.Background(), "o/r", "agent/issue-1")
	if err != nil || pr == nil || pr.Number != 8 {
		t.Fatalf("%v %v", pr, err)
	}
	if q := f.calls()[0].Query; q != "head=o%3Aagent%2Fissue-1&state=open" {
		t.Errorf("query %q", q)
	}
	f2 := newFakeGH(t)
	f2.json("GET /repos/o/r/pulls", []map[string]any{})
	if pr, err := testClient(f2).ExistingPR(context.Background(), "o/r", "b"); err != nil || pr != nil {
		t.Errorf("%v %v", pr, err)
	}
}

// TestLatestReviewBodyIsTheLastAgentReview pins: latest review body is the last agent review.
func TestLatestReviewBodyIsTheLastAgentReview(t *testing.T) {
	f := newFakeGH(t)
	f.json("GET /repos/o/r/pulls/8/reviews", []Item{
		{Body: ReviewMarker("a") + " first"}, {Body: "human"}, {Body: ReviewMarker("b") + " second"}, {Body: "human 2"},
	})
	got, err := testClient(f).LatestReviewBody(context.Background(), "o/r", 8)
	if err != nil || got != ReviewMarker("b")+" second" {
		t.Errorf("%q %v", got, err)
	}
	f2 := newFakeGH(t)
	f2.json("GET /repos/o/r/pulls/8/reviews", []Item{{Body: "human"}})
	if got, err := testClient(f2).LatestReviewBody(context.Background(), "o/r", 8); err != nil || got != "" {
		t.Errorf("%q %v", got, err)
	}
}

// TestRetroTicketReusesTheOpenOneOrCreatesIt pins: retro ticket reuses the open one or creates it.
func TestRetroTicketReusesTheOpenOneOrCreatesIt(t *testing.T) {
	f := newFakeGH(t)
	f.json("GET /repos/o/r/issues", []map[string]any{{"number": 40, "pull_request": map[string]any{}}, {"number": 41}})
	n, err := testClient(f).RetroTicket(context.Background(), "o/r")
	if err != nil || n != 41 {
		t.Fatalf("%d %v", n, err)
	}
	f2 := newFakeGH(t)
	f2.json("GET /repos/o/r/issues", []map[string]any{})
	f2.handle("POST /repos/o/r/issues", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"number": 77})
	})
	n, err = testClient(f2).RetroTicket(context.Background(), "o/r")
	if err != nil || n != 77 {
		t.Fatalf("%d %v", n, err)
	}
	body := f2.bodies("POST", "/repos/o/r/issues")[0]
	for _, want := range []string{`"title":"Agent retros"`, `"labels":["retro"]`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q lacks %s", body, want)
		}
	}
}

// TestPostRetroCreatesThenUpdatesByMarker pins: post retro creates then updates by marker.
func TestPostRetroCreatesThenUpdatesByMarker(t *testing.T) {
	f := newFakeGH(t)
	f.json("GET /repos/o/r/issues", []map[string]any{{"number": 41}})
	f.json("GET /repos/o/r/issues/41/comments", []Item{})
	f.status("POST /repos/o/r/issues/41/comments", http.StatusCreated)
	if err := testClient(f).PostRetro(context.Background(), "o/r", 5, "abc", "lessons"); err != nil {
		t.Fatal(err)
	}
	body := f.bodies("POST", "/repos/o/r/issues/41/comments")[0]
	if !strings.Contains(body, `<!-- agent-retro:5:abc -->\n## Retro: #5\n\nlessons`) {
		t.Errorf("body %q", body)
	}

	f2 := newFakeGH(t)
	f2.json("GET /repos/o/r/issues", []map[string]any{{"number": 41}})
	f2.json("GET /repos/o/r/issues/41/comments", []Item{{ID: 9, Body: RetroMarker(5, "abc") + "\nold"}})
	f2.status("PATCH /repos/o/r/issues/comments/9", http.StatusOK)
	if err := testClient(f2).PostRetro(context.Background(), "o/r", 5, "abc", "again"); err != nil {
		t.Fatal(err)
	}
	if got := f2.bodies("POST", "/repos/o/r/issues/41/comments"); len(got) != 0 {
		t.Errorf("posted a duplicate: %v", got)
	}
	if got := f2.bodies("PATCH", "/repos/o/r/issues/comments/9"); len(got) != 1 {
		t.Errorf("patches %v", got)
	}
}

// TestPostReviewApprovesAndFallsBackToCommentOn422 pins: post review approves and falls back to comment on422.
func TestPostReviewApprovesAndFallsBackToCommentOn422(t *testing.T) {
	f := newFakeGH(t)
	f.status("POST /repos/o/r/pulls/8/reviews", http.StatusOK)
	if err := testClient(f).PostReview(context.Background(), "o/r", 8, "sha", "body", true); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies("POST", "/repos/o/r/pulls/8/reviews"); len(got) != 1 || !strings.Contains(got[0], `"event":"APPROVE"`) {
		t.Errorf("got %v", got)
	}

	f2 := newFakeGH(t)
	n := 0
	f2.handle("POST /repos/o/r/pulls/8/reviews", func(w http.ResponseWriter, _ *http.Request) {
		n++
		if n == 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
		}
	})
	if err := testClient(f2).PostReview(context.Background(), "o/r", 8, "sha", "body", true); err != nil {
		t.Fatal(err)
	}
	got := f2.bodies("POST", "/repos/o/r/pulls/8/reviews")
	if len(got) != 2 || !strings.Contains(got[1], `"event":"COMMENT"`) || !strings.Contains(got[1], "also the pull request's author") {
		t.Errorf("got %v", got)
	}
}

// TestPostReviewWithFindingsIsAComment pins: post review with findings is a comment.
func TestPostReviewWithFindingsIsAComment(t *testing.T) {
	f := newFakeGH(t)
	f.status("POST /repos/o/r/pulls/8/reviews", http.StatusOK)
	if err := testClient(f).PostReview(context.Background(), "o/r", 8, "sha", "body", false); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies("POST", "/repos/o/r/pulls/8/reviews"); len(got) != 1 || !strings.Contains(got[0], `"event":"COMMENT"`) {
		t.Errorf("got %v", got)
	}
}

// TestPostReviewPropagatesOtherErrors pins: post review propagates other errors.
func TestPostReviewPropagatesOtherErrors(t *testing.T) {
	f := newFakeGH(t)
	f.status("POST /repos/o/r/pulls/8/reviews", http.StatusInternalServerError)
	if err := testClient(f).PostReview(context.Background(), "o/r", 8, "sha", "b", true); err == nil {
		t.Error("swallowed a 500")
	}
}
