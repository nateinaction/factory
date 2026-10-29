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
			first := issue(workflow.LabelImplement)

			issues, err := c.ListLabeled(ctx, "o/r", workflow.LabelImplement)
			Expect(err).NotTo(HaveOccurred())
			Expect(issues).To(HaveLen(1))
			Expect(issues[0].Number).To(Equal(first))
			Expect(p.Calls()[0].Query).To(Equal(
				"direction=asc&labels=factory%3Aimplement&per_page=20&sort=created&state=open"))
		})
	})

	Describe("ListOpenIssues", func() {
		It("lists every open issue with its author and labels", func() {
			number := issue(workflow.LabelTask)

			issues, err := c.ListOpenIssues(ctx, "o/r")
			Expect(err).NotTo(HaveOccurred())
			Expect(issues).To(HaveLen(1))
			Expect(issues[0].Number).To(Equal(number))
			Expect(issues[0].Author).To(Equal("o"))
			Expect(issues[0].State).To(Equal("open"))
			Expect(issues[0].Labels).To(ConsistOf(workflow.LabelTask))
		})
	})

	Describe("EnsureLabels", func() {
		It("creates only the labels the repository lacks, idempotently", func() {
			Expect(c.EnsureLabels(ctx, "o/r", workflow.FactoryLabels())).To(Succeed())
			Expect(p.Bodies("POST", "/repos/o/r/labels")).To(HaveLen(len(workflow.FactoryLabels())))
			Expect(p.Bodies("POST", "/repos/o/r/labels")[0]).To(ContainSubstring(`{"name":"factory:`))

			p2 := ghtest.NewProxy(emulator.URL())
			DeferCleanup(p2.Close)

			again := github.NewClient(p2.URL(), "secret-token", p2.Client())
			Expect(again.EnsureLabels(ctx, "o/r", workflow.FactoryLabels())).To(Succeed())
			Expect(p2.Bodies("POST", "/repos/o/r/labels")).To(BeEmpty())
		})
	})

	Describe("ListComments", func() {
		It("lists a comment's author", func() {
			number := issue()
			Expect(emulator.Do(ctx, http.MethodPost, "/repos/o/r/issues/"+itoa(number)+"/comments",
				ghtest.AdminToken, map[string]string{"body": "hello"}, nil)).To(Succeed())

			comments, err := c.ListComments(ctx, "o/r", number)
			Expect(err).NotTo(HaveOccurred())
			Expect(comments).To(HaveLen(1))
			Expect(comments[0].Body).To(Equal("hello"))
			Expect(comments[0].Author).To(Equal("o"))
		})
	})

	Describe("CollaboratorPermission", func() {
		It("reads the owner's permission", func() {
			permission, err := c.CollaboratorPermission(ctx, "o/r", "o")
			Expect(err).NotTo(HaveOccurred())
			Expect(permission).To(Equal("admin"))
		})

		It("gives a non-collaborator no permission", func() {
			permission, err := c.CollaboratorPermission(ctx, "o/r", "factory")
			Expect(err).NotTo(HaveOccurred())
			Expect(permission).To(Equal("none"))
		})

		It("passes on an unreadable permission", func() {
			p.Status("GET /repos/o/r/collaborators/o/permission", http.StatusInternalServerError)

			_, err := c.CollaboratorPermission(ctx, "o/r", "o")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("OpenBlockers", func() {
		It("reports that dependencies are unavailable when the endpoint is missing", func() {
			number := issue()

			blockers, supported, err := c.OpenBlockers(ctx, "o/r", number)
			Expect(err).NotTo(HaveOccurred())
			Expect(supported).To(BeFalse())
			Expect(blockers).To(BeEmpty())
		})
	})

	Describe("Claim", func() {
		transition := workflow.Transition{
			Role: workflow.Implementer, RunID: "run-1",
			From: workflow.Implement, To: workflow.Implementing, SHA: "abc",
		}

		It("posts the marker, swaps the labels and reads the claim back", func() {
			number := issue(workflow.LabelImplement)

			Expect(c.Claim(ctx, "o/r", number, transition)).To(BeTrue())
			Expect(p.Summary()).To(Equal([]string{
				"POST /repos/o/r/issues/" + itoa(number) + "/comments",
				"DELETE /repos/o/r/issues/" + itoa(number) + "/labels/factory:implement",
				"POST /repos/o/r/issues/" + itoa(number) + "/labels",
				"GET /repos/o/r/issues/" + itoa(number),
			}))
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/comments")[0]).To(
				ContainSubstring("<!-- factory:handoff role=implementer run=run-1 " +
					"from=factory:implement to=factory:implementing sha=abc -->"))
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/labels")).To(
				Equal([]string{`{"labels":["factory:implementing"]}` + "\n"}))
		})

		It("fails closed when someone else got there first", func() {
			number := issue()

			Expect(c.Claim(ctx, "o/r", number, transition)).To(BeFalse())
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/labels")).To(BeEmpty(),
				"added a label after losing the claim")
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/comments")).To(HaveLen(1),
				"want the marker the claim posts first")
		})

		It("passes on any other error", func() {
			number := issue(workflow.LabelImplement)
			p.Status("DELETE /repos/o/r/issues/"+itoa(number)+"/labels/factory:implement",
				http.StatusInternalServerError)

			won, err := c.Claim(ctx, "o/r", number, transition)
			Expect(err).To(HaveOccurred())
			Expect(won).To(BeFalse())
		})
	})

	Describe("Finish", func() {
		transition := workflow.Transition{
			Role: workflow.Reviewer, RunID: "run-2",
			From: workflow.Reviewing, To: workflow.Done, SHA: "abc",
		}

		It("posts the marker with its outcome, then swaps the labels", func() {
			number := issue(workflow.LabelReviewing)

			Expect(c.Finish(ctx, "o/r", number, transition, "done")).To(Succeed())
			Expect(p.Summary()).To(Equal([]string{
				"POST /repos/o/r/issues/" + itoa(number) + "/comments",
				"DELETE /repos/o/r/issues/" + itoa(number) + "/labels/factory:reviewing",
				"POST /repos/o/r/issues/" + itoa(number) + "/labels",
			}))
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/comments")[0]).To(And(
				ContainSubstring("<!-- factory:handoff role=reviewer run=run-2 "+
					"from=factory:reviewing to=factory:done sha=abc -->"),
				ContainSubstring(`\n\ndone`),
			))
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/labels")[0]).To(
				ContainSubstring(workflow.LabelDone))
		})

		It("tolerates a label already removed", func() {
			number := issue()

			Expect(c.Finish(ctx, "o/r", number, transition, "done")).To(Succeed())
		})

		It("passes on any other delete error", func() {
			number := issue(workflow.LabelReviewing)
			p.Status("DELETE /repos/o/r/issues/"+itoa(number)+"/labels/factory:reviewing",
				http.StatusBadGateway)

			Expect(c.Finish(ctx, "o/r", number, transition, "x")).NotTo(Succeed())
		})
	})

	Describe("ExistingPR", func() {
		It("finds the open pull request for the owner's branch", func() {
			pr := pullRequest()

			found, err := c.ExistingPR(ctx, "o/r", "agent/issue-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(found.Number).To(Equal(pr.Number))
			Expect(found.HTMLURL).To(Equal(pr.HTMLURL))
			Expect(found.Head.SHA).NotTo(BeEmpty())
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

		It("posts an approval as a native review", func() {
			Expect(c.PostReview(ctx, "o/r", pr.Number, "sha", "body", workflow.Approve)).To(Succeed())
			Expect(p.Bodies("POST", reviewPath(pr))).To(ConsistOf(And(
				ContainSubstring(`"event":"APPROVE"`), ContainSubstring(`"commit_id":"sha"`),
			)))
		})

		It("posts a request for changes as a native review", func() {
			Expect(c.PostReview(ctx, "o/r", pr.Number, "sha", "body", workflow.RequestChanges)).To(Succeed())
			Expect(p.Bodies("POST", reviewPath(pr))).To(ConsistOf(And(
				ContainSubstring(`"event":"REQUEST_CHANGES"`), ContainSubstring(`"commit_id":"sha"`),
			)))
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
			Expect(c.PostReview(ctx, "o/r", pr.Number, "sha", "body", workflow.Approve)).To(Succeed())

			got := p.Bodies("POST", reviewPath(pr))
			Expect(got).To(HaveLen(2))
			Expect(got[1]).To(And(ContainSubstring(`"event":"COMMENT"`), ContainSubstring("also the pull request's author")))
		})

		It("falls back to a comment review when GitHub refuses the request for changes", func() {
			n := 0

			p.Handle("POST /repos/o/r/pulls/"+itoa(pr.Number)+"/reviews", func(w http.ResponseWriter, r *http.Request) {
				n++
				if n == 1 {
					w.WriteHeader(http.StatusUnprocessableEntity)

					return
				}

				p.Forward(w, r)
			})

			Expect(c.PostReview(ctx, "o/r", pr.Number, "sha", "body", workflow.RequestChanges)).To(Succeed())
			Expect(p.Bodies("POST", reviewPath(pr))).To(ContainElement(ContainSubstring(`"event":"COMMENT"`)))
		})

		It("passes on any other error", func() {
			p.Status("POST /repos/o/r/pulls/"+itoa(pr.Number)+"/reviews", http.StatusInternalServerError)
			Expect(c.PostReview(ctx, "o/r", pr.Number, "sha", "b", workflow.Approve)).NotTo(Succeed())
		})
	})

	Describe("UpdatePR", func() {
		It("updates the pull request's title and body", func() {
			pr := pullRequest()

			Expect(c.UpdatePR(ctx, "o/r", pr.Number, "New", "New body")).To(Succeed())
			Expect(p.Bodies("PATCH", "/repos/o/r/pulls/"+itoa(pr.Number))).To(ConsistOf(And(
				ContainSubstring(`"title":"New"`), ContainSubstring(`"body":"New body"`),
			)))
		})
	})

	Describe("PostReplies", func() {
		It("comments the replies on the pull request, keyed by commit", func() {
			pr := pullRequest()

			Expect(c.PostReplies(ctx, "o/r", pr.Number, "sha", "replies")).To(Succeed())
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(pr.Number)+"/comments")).To(ConsistOf(
				ContainSubstring("replies")))
		})

		It("updates the keyed comment instead of adding a second", func() {
			pr := pullRequest()
			body := workflow.RepliesMarker("sha") + "\nold"
			Expect(emulator.Do(ctx, http.MethodPost, "/repos/o/r/issues/"+itoa(pr.Number)+"/comments",
				ghtest.AdminToken, map[string]string{"body": body}, nil)).To(Succeed())

			Expect(c.PostReplies(ctx, "o/r", pr.Number, "sha", "again")).To(Succeed())
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(pr.Number)+"/comments")).To(BeEmpty(), "posted a duplicate")
			Expect(p.Summary()).To(ContainElement(ContainSubstring("PATCH /repos/o/r/issues/comments/")))
		})
	})

	Describe("RequestReviewers", func() {
		It("asks the named people to review the pull request", func() {
			pr := pullRequest()

			Expect(c.RequestReviewers(ctx, "o/r", pr.Number, []string{"o"})).To(Succeed())
			Expect(p.Bodies("POST", "/repos/o/r/pulls/"+itoa(pr.Number)+"/requested_reviewers")).To(ConsistOf(
				ContainSubstring(`"reviewers":["o"]`)))
		})

		It("asks for nothing when there is no one to ask", func() {
			pr := pullRequest()

			Expect(c.RequestReviewers(ctx, "o/r", pr.Number, nil)).To(Succeed())
			Expect(p.Calls()).To(BeEmpty())
		})
	})
})

// reviewPath is a pull request's reviews endpoint.
func reviewPath(pr ghtest.PullRequest) string {
	return "/repos/o/r/pulls/" + itoa(pr.Number) + "/reviews"
}

// itoa formats a number for a URL.
func itoa(n int) string { return strconv.Itoa(n) }
