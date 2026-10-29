package github_test

import (
	"context"
	"net/http"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/ghtest"
	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("Client", func() {
	var (
		p   *ghtest.Proxy
		c   github.Client
		ctx context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		Expect(emulator.ResetRepo(ctx, "o/r")).To(Succeed())

		p = ghtest.NewProxy(emulator.URL())

		DeferCleanup(func() {
			Expect(p.Unexpected()).To(BeEmpty())

			p.Close()
		})

		c = github.NewClient(p.URL(), "secret-token", p.Client())
	})

	// issue creates an issue on o/r with labels and returns its number.
	issue := func(labels ...string) int {
		GinkgoHelper()

		number, err := emulator.CreateIssue(ctx, "o/r", labels, "Add x", "Do x")
		Expect(err).NotTo(HaveOccurred())

		return number
	}

	// pullRequest opens a pull request on o/r and returns it.
	pullRequest := func() ghtest.PullRequest {
		GinkgoHelper()

		pr, err := emulator.CreatePR(ctx, "o/r", "agent/issue-1", "Add x", "Do x")
		Expect(err).NotTo(HaveOccurred())

		return pr
	}

	Describe("every request", func() {
		It("carries foreman's user agent, the token and the pinned API version", func() {
			p.JSON("GET /repos/o/r", map[string]string{"default_branch": "main"})
			Expect(c.DefaultBranch(ctx, "o/r")).To(Equal("main"))

			h := p.Calls()[0].Header
			Expect(h.Get("User-Agent")).To(Equal("foreman"))
			Expect(h.Get("Authorization")).To(Equal("Bearer secret-token"))
			Expect(h.Get("Accept")).To(Equal("application/vnd.github+json"))
			Expect(h.Get("X-GitHub-Api-Version")).To(Equal("2022-11-28"))
		})

		It("fails with an error that names the call but never the token", func() {
			p.Status("GET /repos/o/r", http.StatusForbidden)

			_, err := c.DefaultBranch(ctx, "o/r")
			Expect(err).To(MatchError(ContainSubstring("403")))
			Expect(err.Error()).NotTo(ContainSubstring("secret-token"))
		})
	})

	Describe("Paged", func() {
		It("stops at MaxPages when every page is full", func() {
			p.JSON("GET /full", make([]workflow.Item, github.PageSize))

			items, err := github.Paged[workflow.Item](ctx, c, "/full")
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(github.MaxPages * github.PageSize))
			Expect(p.Calls()).To(HaveLen(github.MaxPages))
		})

		It("stops at a short page and appends to an existing query", func() {
			p.JSON("GET /short", []workflow.Item{{ID: 1}})

			items, err := github.Paged[workflow.Item](ctx, c, "/short?x=1")
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(1))
			Expect(p.Calls()).To(HaveLen(1))
			Expect(p.Calls()[0].Query).To(Equal("x=1&per_page=100&page=1"))
		})
	})

	Describe("ListLabeled", func() {
		It("asks for open issues with the label, oldest first", func() {
			first := issue(workflow.LabelReady)

			issues, err := c.ListLabeled(ctx, "o/r", workflow.LabelReady)
			Expect(err).NotTo(HaveOccurred())
			Expect(issues).To(HaveLen(1))
			Expect(issues[0].Number).To(Equal(first))
			Expect(p.Calls()[0].Query).To(Equal("direction=asc&labels=ready-for-agent&per_page=20&sort=created&state=open"))
		})
	})

	Describe("Claim", func() {
		It("swaps the labels", func() {
			number := issue("a")

			Expect(c.Claim(ctx, "o/r", number, "a", "b")).To(BeTrue())
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/labels")).To(Equal([]string{`{"labels":["b"]}` + "\n"}))
		})

		It("fails closed when someone else got there first", func() {
			number := issue()

			Expect(c.Claim(ctx, "o/r", number, "a", "b")).To(BeFalse())
			Expect(p.Summary()).To(HaveLen(1), "added a label after losing the claim")
		})

		It("passes on any other error", func() {
			number := issue("a")
			p.Status("DELETE /repos/o/r/issues/"+itoa(number)+"/labels/a", http.StatusInternalServerError)

			won, err := c.Claim(ctx, "o/r", number, "a", "b")
			Expect(err).To(HaveOccurred())
			Expect(won).To(BeFalse())
		})
	})

	Describe("Finish", func() {
		It("tolerates a label already removed, then labels and comments", func() {
			number := issue()

			Expect(c.Finish(ctx, "o/r", number, "a", "b", "done")).To(Succeed())
			Expect(p.Summary()).To(Equal([]string{
				"DELETE /repos/o/r/issues/" + itoa(number) + "/labels/a",
				"POST /repos/o/r/issues/" + itoa(number) + "/labels",
				"POST /repos/o/r/issues/" + itoa(number) + "/comments",
			}))
		})

		It("passes on any other delete error", func() {
			number := issue("a")
			p.Status("DELETE /repos/o/r/issues/"+itoa(number)+"/labels/a", http.StatusBadGateway)

			Expect(c.Finish(ctx, "o/r", number, "a", "b", "x")).NotTo(Succeed())
		})
	})

	Describe("ExistingPR", func() {
		It("finds the open pull request for the owner's branch", func() {
			pr := pullRequest()

			found, err := c.ExistingPR(ctx, "o/r", "agent/issue-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(Equal(&github.PullRequest{Number: pr.Number, HTMLURL: pr.HTMLURL}))
			Expect(p.Calls()[0].Query).To(Equal("head=o%3Aagent%2Fissue-1&state=open"))
		})

		It("is nil without an error when there is none", func() {
			Expect(c.ExistingPR(ctx, "o/r", "b")).To(BeNil())
		})
	})

	Describe("LatestReviewBody", func() {
		It("is the last review foreman posted", func() {
			pr := pullRequest()

			for _, body := range []string{
				workflow.ReviewMarker("a") + " first",
				"human",
				workflow.ReviewMarker("b") + " second",
				"human 2",
			} {
				Expect(emulator.CreateReview(ctx, "o/r", pr.Number, body)).To(Succeed())
			}

			Expect(c.LatestReviewBody(ctx, "o/r", pr.Number)).To(Equal(workflow.ReviewMarker("b") + " second"))
		})

		It("is empty when foreman has posted none", func() {
			pr := pullRequest()
			Expect(emulator.CreateReview(ctx, "o/r", pr.Number, "human")).To(Succeed())

			Expect(c.LatestReviewBody(ctx, "o/r", pr.Number)).To(BeEmpty())
		})
	})

	Describe("RetroTicket", func() {
		It("reuses the open one", func() {
			number := issue(workflow.LabelRetro)

			Expect(c.RetroTicket(ctx, "o/r")).To(Equal(number))
		})

		It("creates it on first use", func() {
			number, err := c.RetroTicket(ctx, "o/r")
			Expect(err).NotTo(HaveOccurred())
			Expect(number).NotTo(BeZero())
			Expect(p.Bodies("POST", "/repos/o/r/issues")[0]).To(And(
				ContainSubstring(`"title":"Agent retros"`), ContainSubstring(`"labels":["retro"]`),
			))
		})
	})

	Describe("PostRetro", func() {
		var ticket int

		BeforeEach(func() {
			ticket = issue(workflow.LabelRetro)
		})

		It("creates a comment keyed by issue and commit", func() {
			Expect(c.PostRetro(ctx, "o/r", 5, "abc", "lessons")).To(Succeed())
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(ticket)+"/comments")[0]).To(
				ContainSubstring(`<!-- agent-retro:5:abc -->\n## Retro: #5\n\nlessons`))
		})

		It("updates the keyed comment on a repeat", func() {
			body := workflow.RetroMarker(5, "abc") + "\nold"
			Expect(emulator.Do(ctx, http.MethodPost, "/repos/o/r/issues/"+itoa(ticket)+"/comments",
				ghtest.AdminToken, map[string]string{"body": body}, nil)).To(Succeed())

			Expect(c.PostRetro(ctx, "o/r", 5, "abc", "again")).To(Succeed())
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(ticket)+"/comments")).To(BeEmpty(), "posted a duplicate")
			Expect(p.Summary()).To(ContainElement(ContainSubstring("PATCH /repos/o/r/issues/comments/")))
		})
	})

	Describe("PostReview", func() {
		var pr ghtest.PullRequest

		BeforeEach(func() {
			pr = pullRequest()
		})

		It("approves a clean review", func() {
			Expect(c.PostReview(ctx, "o/r", pr.Number, "sha", "body", true)).To(Succeed())
			Expect(p.Bodies("POST", reviewPath(pr))).To(ConsistOf(ContainSubstring(`"event":"APPROVE"`)))
		})

		It("falls back to a comment review when GitHub refuses the approval", func() {
			n := 0

			p.Handle("POST /repos/o/r/pulls/"+itoa(pr.Number)+"/reviews", func(w http.ResponseWriter, r *http.Request) {
				n++
				if n == 1 {
					w.WriteHeader(http.StatusUnprocessableEntity)

					return
				}

				p.Forward(w, r)
			})
			Expect(c.PostReview(ctx, "o/r", pr.Number, "sha", "body", true)).To(Succeed())

			got := p.Bodies("POST", reviewPath(pr))
			Expect(got).To(HaveLen(2))
			Expect(got[1]).To(And(ContainSubstring(`"event":"COMMENT"`), ContainSubstring("also the pull request's author")))
		})

		It("posts findings as a comment review", func() {
			Expect(c.PostReview(ctx, "o/r", pr.Number, "sha", "body", false)).To(Succeed())
			Expect(p.Bodies("POST", reviewPath(pr))).To(ConsistOf(ContainSubstring(`"event":"COMMENT"`)))
		})

		It("passes on any other error", func() {
			p.Status("POST /repos/o/r/pulls/"+itoa(pr.Number)+"/reviews", http.StatusInternalServerError)
			Expect(c.PostReview(ctx, "o/r", pr.Number, "sha", "b", true)).NotTo(Succeed())
		})
	})
})

// reviewPath is a pull request's reviews endpoint.
func reviewPath(pr ghtest.PullRequest) string {
	return "/repos/o/r/pulls/" + itoa(pr.Number) + "/reviews"
}

// itoa formats a number for a URL.
func itoa(n int) string { return strconv.Itoa(n) }
