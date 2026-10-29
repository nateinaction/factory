package github_test

import (
	"context"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/fakegh"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("Client", func() {
	var (
		f   *fakegh.Server
		c   github.Client
		ctx context.Context
	)

	BeforeEach(func() {
		f = fakegh.New()

		DeferCleanup(func() {
			Expect(f.Unexpected()).To(BeEmpty())
			f.Close()
		})

		c = github.NewClient(f.URL(), "secret-token", f.Client())
		ctx = context.Background()
	})

	Describe("every request", func() {
		It("carries foreman's user agent, the token and the pinned API version", func() {
			f.JSON("GET /repos/o/r", map[string]string{"default_branch": "main"})
			Expect(c.DefaultBranch(ctx, "o/r")).To(Equal("main"))

			h := f.Calls()[0].Header
			Expect(h.Get("User-Agent")).To(Equal("foreman"))
			Expect(h.Get("Authorization")).To(Equal("Bearer secret-token"))
			Expect(h.Get("Accept")).To(Equal("application/vnd.github+json"))
			Expect(h.Get("X-GitHub-Api-Version")).To(Equal("2022-11-28"))
		})

		It("fails with an error that names the call but never the token", func() {
			f.Status("GET /repos/o/r", http.StatusForbidden)

			_, err := c.DefaultBranch(ctx, "o/r")
			Expect(err).To(MatchError(ContainSubstring("403")))
			Expect(err.Error()).NotTo(ContainSubstring("secret-token"))
		})
	})

	Describe("Paged", func() {
		It("stops at MaxPages when every page is full", func() {
			f.JSON("GET /full", make([]workflow.Item, github.PageSize))

			items, err := github.Paged[workflow.Item](ctx, c, "/full")
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(github.MaxPages * github.PageSize))
			Expect(f.Calls()).To(HaveLen(github.MaxPages))
		})

		It("stops at a short page and appends to an existing query", func() {
			f.JSON("GET /short", []workflow.Item{{ID: 1}})

			items, err := github.Paged[workflow.Item](ctx, c, "/short?x=1")
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(1))
			Expect(f.Calls()).To(HaveLen(1))
			Expect(f.Calls()[0].Query).To(Equal("x=1&per_page=100&page=1"))
		})
	})

	Describe("ListLabeled", func() {
		It("asks for open issues with the label, oldest first", func() {
			f.JSON("GET /repos/o/r/issues", []map[string]any{{"number": 3, "title": "t", "body": nil}})

			issues, err := c.ListLabeled(ctx, "o/r", "ready-for-agent")
			Expect(err).NotTo(HaveOccurred())
			Expect(issues).To(HaveLen(1))
			Expect(issues[0].Number).To(Equal(3))
			Expect(f.Calls()[0].Query).To(Equal("direction=asc&labels=ready-for-agent&per_page=20&sort=created&state=open"))
		})
	})

	Describe("Claim", func() {
		It("swaps the labels", func() {
			f.Status("DELETE /repos/o/r/issues/5/labels/a", http.StatusOK)
			f.Status("POST /repos/o/r/issues/5/labels", http.StatusOK)
			Expect(c.Claim(ctx, "o/r", 5, "a", "b")).To(BeTrue())
			Expect(f.Bodies("POST", "/repos/o/r/issues/5/labels")).To(Equal([]string{`{"labels":["b"]}` + "\n"}))
		})

		It("fails closed when someone else got there first", func() {
			f.Status("DELETE /repos/o/r/issues/5/labels/a", http.StatusNotFound)
			Expect(c.Claim(ctx, "o/r", 5, "a", "b")).To(BeFalse())
			Expect(f.Summary()).To(HaveLen(1), "added a label after losing the claim")
		})

		It("passes on any other error", func() {
			f.Status("DELETE /repos/o/r/issues/5/labels/a", http.StatusInternalServerError)

			won, err := c.Claim(ctx, "o/r", 5, "a", "b")
			Expect(err).To(HaveOccurred())
			Expect(won).To(BeFalse())
		})
	})

	Describe("Finish", func() {
		It("tolerates a label already removed, then labels and comments", func() {
			f.Status("DELETE /repos/o/r/issues/5/labels/a", http.StatusNotFound)
			f.Status("POST /repos/o/r/issues/5/labels", http.StatusOK)
			f.Status("POST /repos/o/r/issues/5/comments", http.StatusCreated)
			Expect(c.Finish(ctx, "o/r", 5, "a", "b", "done")).To(Succeed())
			Expect(f.Summary()).To(Equal([]string{
				"DELETE /repos/o/r/issues/5/labels/a", "POST /repos/o/r/issues/5/labels", "POST /repos/o/r/issues/5/comments",
			}))
		})

		It("passes on any other delete error", func() {
			f.Status("DELETE /repos/o/r/issues/5/labels/a", http.StatusBadGateway)
			Expect(c.Finish(ctx, "o/r", 5, "a", "b", "x")).NotTo(Succeed())
		})
	})

	Describe("ExistingPR", func() {
		It("finds the open pull request for the owner's branch", func() {
			f.JSON("GET /repos/o/r/pulls", []map[string]any{{"number": 8, "html_url": "http://pr/8"}})

			pr, err := c.ExistingPR(ctx, "o/r", "agent/issue-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(pr).To(Equal(&github.PullRequest{Number: 8, HTMLURL: "http://pr/8"}))
			Expect(f.Calls()[0].Query).To(Equal("head=o%3Aagent%2Fissue-1&state=open"))
		})

		It("is nil without an error when there is none", func() {
			f.JSON("GET /repos/o/r/pulls", []map[string]any{})
			Expect(c.ExistingPR(ctx, "o/r", "b")).To(BeNil())
		})
	})

	Describe("LatestReviewBody", func() {
		It("is the last review foreman posted", func() {
			f.JSON("GET /repos/o/r/pulls/8/reviews", []workflow.Item{
				{Body: workflow.ReviewMarker("a") + " first"},
				{Body: "human"},
				{Body: workflow.ReviewMarker("b") + " second"},
				{Body: "human 2"},
			})
			Expect(c.LatestReviewBody(ctx, "o/r", 8)).To(Equal(workflow.ReviewMarker("b") + " second"))
		})

		It("is empty when foreman has posted none", func() {
			f.JSON("GET /repos/o/r/pulls/8/reviews", []workflow.Item{{Body: "human"}})
			Expect(c.LatestReviewBody(ctx, "o/r", 8)).To(BeEmpty())
		})
	})

	Describe("RetroTicket", func() {
		It("reuses the open one, skipping pull requests", func() {
			f.JSON("GET /repos/o/r/issues", []map[string]any{{"number": 40, "pull_request": map[string]any{}}, {"number": 41}})
			Expect(c.RetroTicket(ctx, "o/r")).To(Equal(41))
		})

		It("creates it on first use", func() {
			f.JSON("GET /repos/o/r/issues", []map[string]any{})
			f.Handle("POST /repos/o/r/issues", func(w http.ResponseWriter, _ *http.Request) {
				fakegh.WriteJSON(w, map[string]any{"number": 77})
			})
			Expect(c.RetroTicket(ctx, "o/r")).To(Equal(77))
			Expect(f.Bodies("POST", "/repos/o/r/issues")[0]).To(And(
				ContainSubstring(`"title":"Agent retros"`), ContainSubstring(`"labels":["retro"]`),
			))
		})
	})

	Describe("PostRetro", func() {
		BeforeEach(func() {
			f.JSON("GET /repos/o/r/issues", []map[string]any{{"number": 41}})
		})

		It("creates a comment keyed by issue and commit", func() {
			f.JSON("GET /repos/o/r/issues/41/comments", []workflow.Item{})
			f.Status("POST /repos/o/r/issues/41/comments", http.StatusCreated)
			Expect(c.PostRetro(ctx, "o/r", 5, "abc", "lessons")).To(Succeed())
			Expect(f.Bodies("POST", "/repos/o/r/issues/41/comments")[0]).To(
				ContainSubstring(`<!-- agent-retro:5:abc -->\n## Retro: #5\n\nlessons`))
		})

		It("updates the keyed comment on a repeat", func() {
			f.JSON("GET /repos/o/r/issues/41/comments", []workflow.Item{{ID: 9, Body: workflow.RetroMarker(5, "abc") + "\nold"}})
			f.Status("PATCH /repos/o/r/issues/comments/9", http.StatusOK)
			Expect(c.PostRetro(ctx, "o/r", 5, "abc", "again")).To(Succeed())
			Expect(f.Bodies("POST", "/repos/o/r/issues/41/comments")).To(BeEmpty(), "posted a duplicate")
			Expect(f.Bodies("PATCH", "/repos/o/r/issues/comments/9")).To(HaveLen(1))
		})
	})

	Describe("PostReview", func() {
		It("approves a clean review", func() {
			f.Status("POST /repos/o/r/pulls/8/reviews", http.StatusOK)
			Expect(c.PostReview(ctx, "o/r", 8, "sha", "body", true)).To(Succeed())
			Expect(f.Bodies("POST", "/repos/o/r/pulls/8/reviews")).To(ConsistOf(ContainSubstring(`"event":"APPROVE"`)))
		})

		It("falls back to a comment review when GitHub refuses the approval", func() {
			n := 0

			f.Handle("POST /repos/o/r/pulls/8/reviews", func(w http.ResponseWriter, _ *http.Request) {
				n++
				if n == 1 {
					w.WriteHeader(http.StatusUnprocessableEntity)
				}
			})
			Expect(c.PostReview(ctx, "o/r", 8, "sha", "body", true)).To(Succeed())

			got := f.Bodies("POST", "/repos/o/r/pulls/8/reviews")
			Expect(got).To(HaveLen(2))
			Expect(got[1]).To(And(ContainSubstring(`"event":"COMMENT"`), ContainSubstring("also the pull request's author")))
		})

		It("posts findings as a comment review", func() {
			f.Status("POST /repos/o/r/pulls/8/reviews", http.StatusOK)
			Expect(c.PostReview(ctx, "o/r", 8, "sha", "body", false)).To(Succeed())
			Expect(f.Bodies("POST", "/repos/o/r/pulls/8/reviews")).To(ConsistOf(ContainSubstring(`"event":"COMMENT"`)))
		})

		It("passes on any other error", func() {
			f.Status("POST /repos/o/r/pulls/8/reviews", http.StatusInternalServerError)
			Expect(c.PostReview(ctx, "o/r", 8, "sha", "b", true)).NotTo(Succeed())
		})
	})
})
