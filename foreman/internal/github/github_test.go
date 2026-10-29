package github_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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
			Expect(c.DefaultBranch(ctx, testRepo())).To(Equal("main"))

			h := p.Calls()[0].Header
			Expect(h.Get("User-Agent")).To(Equal("foreman"))
			Expect(h.Get("Authorization")).To(Equal("Bearer secret-token"))
			Expect(h.Get("Accept")).To(Equal("application/vnd.github+json"))
			Expect(h.Get("X-GitHub-Api-Version")).To(Equal("2022-11-28"))
		})

		It("fails with an error that names the call but never the token", func() {
			p.Status("GET /repos/o/r", http.StatusForbidden)

			_, err := c.DefaultBranch(ctx, testRepo())
			Expect(err).To(MatchError(ContainSubstring("403")))
			Expect(err.Error()).NotTo(ContainSubstring("secret-token"))
		})
	})

	Describe("an answer it cannot trust", func() {
		It("refuses a response over the size cap", func() {
			// 16 MiB is the cap; one byte more must fail rather than be decoded or truncated.
			const overCap = 16<<20 + 1

			p.Handle("GET /repos/o/r", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, err := w.Write(bytes.Repeat([]byte(" "), overCap))
				Expect(err).NotTo(HaveOccurred())
			})

			_, err := c.DefaultBranch(ctx, testRepo())
			Expect(err).To(MatchError(ContainSubstring("response over")))
		})

		It("fails on a malformed response rather than reading it as empty", func() {
			p.Handle("GET /repos/o/r", func(w http.ResponseWriter, _ *http.Request) {
				_, err := w.Write([]byte(`{"default_branch": `))
				Expect(err).NotTo(HaveOccurred())
			})

			branch, err := c.DefaultBranch(ctx, testRepo())
			Expect(err).To(MatchError(ContainSubstring("decode")))
			Expect(branch).To(BeEmpty())
		})
	})

	Describe("API paths", func() {
		It("escapes each part so that none can leave its own segment", func() {
			var asked []string

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked = append(asked, r.Method+" "+r.RequestURI)

				ghtest.WriteJSON(w, map[string]string{"permission": "none"})
			}))
			DeferCleanup(srv.Close)

			escaping := github.NewClient(srv.URL, "secret-token", srv.Client())

			_, err := escaping.CollaboratorPermission(ctx, testRepo(), "o/../factory")
			Expect(err).NotTo(HaveOccurred())
			Expect(asked).To(Equal([]string{"GET /repos/o/r/collaborators/o%2F..%2Ffactory/permission"}))
		})
	})

	Describe("Paged", func() {
		It("fails when the last allowed page is full", func() {
			p.JSON("GET /full", make([]workflow.Item, github.PageSize))

			items, err := github.Paged[workflow.Item](ctx, c, "/full")
			Expect(items).To(BeNil())
			Expect(err).To(MatchError(ContainSubstring("/full")))
			Expect(err).To(MatchError(ContainSubstring("over 5 pages")))
			Expect(p.Calls()).To(HaveLen(github.MaxPages))
		})

		It("takes a listing whose last allowed page is short as complete", func() {
			p.Handle("GET /nearly-full", func(w http.ResponseWriter, r *http.Request) {
				page, err := strconv.Atoi(r.URL.Query().Get("page"))
				Expect(err).NotTo(HaveOccurred())

				size := github.PageSize
				if page == github.MaxPages {
					size = 1
				}

				ghtest.WriteJSON(w, make([]workflow.Item, size))
			})

			items, err := github.Paged[workflow.Item](ctx, c, "/nearly-full")
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen((github.MaxPages-1)*github.PageSize + 1))
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

			issues, err := c.ListLabeled(ctx, testRepo(), workflow.LabelImplement)
			Expect(err).NotTo(HaveOccurred())
			Expect(issues).To(HaveLen(1))
			Expect(issues[0].Number).To(Equal(first))
			Expect(p.Calls()[0].Query).To(Equal(
				"direction=asc&labels=factory%3Aimplement&per_page=20&sort=created&state=open"))
		})
	})

	Describe("ListIssues", func() {
		It("lists every open issue with its author and labels", func() {
			number := issue(workflow.LabelTask)

			issues, err := c.ListIssues(ctx, testRepo(), github.StateOpen, github.Oldest)
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
			Expect(c.EnsureLabels(ctx, testRepo(), workflow.FactoryLabels())).To(Succeed())
			Expect(p.Bodies("POST", "/repos/o/r/labels")).To(HaveLen(len(workflow.FactoryLabels())))
			Expect(p.Bodies("POST", "/repos/o/r/labels")[0]).To(ContainSubstring(`{"name":"factory:`))

			p2 := ghtest.NewProxy(emulator.URL())
			DeferCleanup(p2.Close)

			again := github.NewClient(p2.URL(), "secret-token", p2.Client())
			Expect(again.EnsureLabels(ctx, testRepo(), workflow.FactoryLabels())).To(Succeed())
			Expect(p2.Bodies("POST", "/repos/o/r/labels")).To(BeEmpty())
		})
	})

	Describe("ListComments", func() {
		It("lists a comment's author", func() {
			number := issue()
			Expect(emulator.Do(ctx, http.MethodPost, "/repos/o/r/issues/"+itoa(number)+"/comments",
				ghtest.AdminToken, map[string]string{"body": "hello"}, nil)).To(Succeed())

			comments, err := c.ListComments(ctx, ref(number))
			Expect(err).NotTo(HaveOccurred())
			Expect(comments).To(HaveLen(1))
			Expect(comments[0].Body).To(Equal("hello"))
			Expect(comments[0].Author).To(Equal("o"))
		})
	})

	Describe("CollaboratorPermission", func() {
		It("reads the owner's permission", func() {
			permission, err := c.CollaboratorPermission(ctx, testRepo(), "o")
			Expect(err).NotTo(HaveOccurred())
			Expect(permission).To(Equal("admin"))
		})

		It("gives a non-collaborator no permission", func() {
			permission, err := c.CollaboratorPermission(ctx, testRepo(), "factory")
			Expect(err).NotTo(HaveOccurred())
			Expect(permission).To(Equal("none"))
		})

		It("passes on an unreadable permission", func() {
			p.Status("GET /repos/o/r/collaborators/o/permission", http.StatusInternalServerError)

			_, err := c.CollaboratorPermission(ctx, testRepo(), "o")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("OpenBlockers", func() {
		It("reports that dependencies are unavailable when the endpoint is missing", func() {
			number := issue()

			blockers, supported, err := c.OpenBlockers(ctx, ref(number))
			Expect(err).NotTo(HaveOccurred())
			Expect(supported).To(BeFalse())
			Expect(blockers).To(BeEmpty())
		})
	})

	Describe("Timeline", func() {
		It("lists the labeled events that date a running label", func() {
			number := issue()

			Expect(emulator.Do(ctx, http.MethodPost, "/repos/o/r/issues/"+itoa(number)+"/labels",
				ghtest.AdminToken, map[string]any{"labels": []string{workflow.LabelImplementing}}, nil)).To(Succeed())

			events, err := c.Timeline(ctx, ref(number))
			Expect(err).NotTo(HaveOccurred())

			at, found := workflow.LabeledAt(events, workflow.LabelImplementing)
			Expect(found).To(BeTrue())
			Expect(at).NotTo(BeZero())
		})
	})

	Describe("Claim", func() {
		transition := workflow.Transition{
			Role: workflow.Implementer, RunID: "run-1",
			From: workflow.Implement, To: workflow.Implementing, SHA: "abc",
		}

		It("posts the marker, swaps the labels and reads the claim back", func() {
			number := issue(workflow.LabelImplement)

			Expect(c.Claim(ctx, ref(number), transition)).To(BeTrue())
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

			Expect(c.Claim(ctx, ref(number), transition)).To(BeFalse())
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/labels")).To(BeEmpty(),
				"added a label after losing the claim")
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/comments")).To(HaveLen(1),
				"want the marker the claim posts first")
		})

		It("passes on any other error", func() {
			number := issue(workflow.LabelImplement)
			p.Status("DELETE /repos/o/r/issues/"+itoa(number)+"/labels/factory:implement",
				http.StatusInternalServerError)

			won, err := c.Claim(ctx, ref(number), transition)
			Expect(err).To(HaveOccurred())
			Expect(won).To(BeFalse())
		})

		It("fails closed when the claim did not stick", func() {
			number := issue(workflow.LabelImplement)

			// The label write succeeds, but the read-back shows the issue never carried it.
			p.JSON("GET /repos/o/r/issues/"+itoa(number), map[string]any{
				"number": number, "labels": []string{},
			})

			won, err := c.Claim(ctx, ref(number), transition)
			Expect(err).To(MatchError(ContainSubstring("did not stick")))
			Expect(won).To(BeFalse())
		})
	})

	Describe("Finish", func() {
		transition := workflow.Transition{
			Role: workflow.Reviewer, RunID: "run-2",
			From: workflow.Reviewing, To: workflow.Done, SHA: "abc",
		}

		// The marker is posted by the Reviewer's App, so a repeat is one the factory authored.
		BeforeEach(func() {
			c = github.NewClient(p.URL(), "minted-token-reviewer", p.Client())
		})

		markerFor := func(t workflow.Transition) string {
			GinkgoHelper()

			marker, err := workflow.RenderTransition(t)
			Expect(err).NotTo(HaveOccurred())

			return marker
		}

		It("posts the marker when an outsider already wrote one that carries it", func() {
			number := issue(workflow.LabelReviewing)

			Expect(emulator.CommentAs(ctx, "o/r", number, "admin-token",
				markerFor(transition)+"\n\nnot foreman's record")).To(Succeed())

			_, err := c.Finish(ctx, ref(number), transition, "done")
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/comments")).To(HaveLen(1),
				"an outsider's marker suppressed the factory's transition")
		})

		It("does not post the marker a factory App already wrote", func() {
			number := issue(workflow.LabelReviewing)

			Expect(emulator.CommentAs(ctx, "o/r", number, "minted-token-reviewer",
				markerFor(transition)+"\n\nforeman's record")).To(Succeed())

			_, err := c.Finish(ctx, ref(number), transition, "done")
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/comments")).To(BeEmpty(),
				"a factory marker was recorded twice")
		})

		It("posts the marker with its outcome, then swaps the labels", func() {
			number := issue(workflow.LabelReviewing)

			_, err := c.Finish(ctx, ref(number), transition, "done")
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Summary()).To(Equal([]string{
				"GET /repos/o/r/issues/" + itoa(number) + "/comments",
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

		It("records the same transition once, however many times it is applied", func() {
			number := issue(workflow.LabelReviewing)

			first, err := c.Finish(ctx, ref(number), transition, "done")
			Expect(err).NotTo(HaveOccurred())

			second, err := c.Finish(ctx, ref(number), transition, "done")
			Expect(err).NotTo(HaveOccurred())

			Expect(second).To(Equal(first), "a repeat returned another comment")
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/comments")).To(HaveLen(1),
				"posted the same transition twice")
		})

		It("restores the destination label when the marker is already recorded", func() {
			number := issue(workflow.LabelReviewing)

			_, err := c.Finish(ctx, ref(number), transition, "done")
			Expect(err).NotTo(HaveOccurred())

			Expect(emulator.RemoveLabel(ctx, "o/r", number, workflow.LabelDone)).To(Succeed())

			_, err = c.Finish(ctx, ref(number), transition, "done")
			Expect(err).NotTo(HaveOccurred())

			got, err := emulator.Issue(ctx, "o/r", number)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Labels).To(ContainElement(HaveField("Name", workflow.LabelDone)))
		})

		It("tolerates a label already removed", func() {
			number := issue()

			_, err := c.Finish(ctx, ref(number), transition, "done")
			Expect(err).NotTo(HaveOccurred())
		})

		It("passes on any other delete error", func() {
			number := issue(workflow.LabelReviewing)
			p.Status("DELETE /repos/o/r/issues/"+itoa(number)+"/labels/factory:reviewing",
				http.StatusBadGateway)

			_, err := c.Finish(ctx, ref(number), transition, "x")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("RemoveLabel", func() {
		It("takes one label off and is safe to repeat", func() {
			number := issue(workflow.LabelReviewing, "bug")

			Expect(c.RemoveLabel(ctx, ref(number), workflow.LabelReviewing)).To(Succeed())
			Expect(c.RemoveLabel(ctx, ref(number), workflow.LabelReviewing)).To(Succeed())

			got, err := emulator.Issue(ctx, "o/r", number)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Labels).To(ConsistOf(HaveField("Name", "bug")))
		})

		It("passes on any other delete error", func() {
			number := issue(workflow.LabelReviewing)
			p.Status("DELETE /repos/o/r/issues/"+itoa(number)+"/labels/factory:reviewing",
				http.StatusBadGateway)

			Expect(c.RemoveLabel(ctx, ref(number), workflow.LabelReviewing)).To(HaveOccurred())
		})
	})

	Describe("ExistingPR", func() {
		It("finds the open pull request for the owner's branch", func() {
			pr := pullRequest()

			found, err := c.ExistingPR(ctx, testRepo(), "agent/issue-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(found.Number).To(Equal(pr.Number))
			Expect(found.HTMLURL).To(Equal(pr.HTMLURL))
			Expect(found.Head.SHA).NotTo(BeEmpty())
			Expect(p.Calls()[0].Query).To(Equal("head=o%3Aagent%2Fissue-1&state=open"))
		})

		It("is nil without an error when there is none", func() {
			Expect(c.ExistingPR(ctx, testRepo(), "b")).To(BeNil())
		})
	})

	Describe("LatestReviewBody", func() {
		// review seeds a review as the Reviewer's App: the author a verdict foreman reads must have.
		review := func(pr ghtest.PullRequest, body string) {
			GinkgoHelper()
			Expect(emulator.CreateReviewAs(ctx, "o/r", pr.Number, "minted-token-reviewer", body)).To(Succeed())
		}

		It("is the last review foreman posted", func() {
			pr := pullRequest()

			review(pr, workflow.ReviewMarker("a")+" first")
			Expect(emulator.CreateReview(ctx, "o/r", pr.Number, "human")).To(Succeed())
			review(pr, workflow.ReviewMarker("b")+" second")
			Expect(emulator.CreateReview(ctx, "o/r", pr.Number, "human 2")).To(Succeed())

			Expect(c.LatestReviewBody(ctx, ref(pr.Number))).To(Equal(workflow.ReviewMarker("b") + " second"))
		})

		It("is empty when foreman has posted none", func() {
			pr := pullRequest()
			Expect(emulator.CreateReview(ctx, "o/r", pr.Number, "human")).To(Succeed())

			Expect(c.LatestReviewBody(ctx, ref(pr.Number))).To(BeEmpty())
		})

		It("ignores a review carrying the Reviewer's marker from another author", func() {
			pr := pullRequest()

			review(pr, workflow.ReviewMarker("a")+" real")
			Expect(emulator.CreateReview(ctx, "o/r", pr.Number, workflow.ReviewMarker("a")+" forged")).To(Succeed())

			Expect(c.LatestReviewBody(ctx, ref(pr.Number))).To(Equal(workflow.ReviewMarker("a") + " real"))
		})
	})

	Describe("UpsertComment", func() {
		// The Implementer's App writes the marked records foreman keeps, so its token owns them.
		BeforeEach(func() {
			c = github.NewClient(p.URL(), "minted-token-implementer", p.Client())
		})

		It("leaves one comment when the same marked comment is posted twice", func() {
			number := issue()
			marker := "<!-- factory:test key=1 -->"

			Expect(c.UpsertComment(ctx, ref(number), marker, marker+"\n\nfirst")).To(Succeed())
			Expect(c.UpsertComment(ctx, ref(number), marker, marker+"\n\nsecond")).To(Succeed())

			comments, err := emulator.Comments(ctx, "o/r", number)
			Expect(err).NotTo(HaveOccurred())
			Expect(comments).To(HaveLen(1))
			Expect(comments[0].Body).To(ContainSubstring("second"))
		})

		It("keeps comments with different markers apart", func() {
			number := issue()

			Expect(c.UpsertComment(ctx, ref(number), "<!-- a -->", "<!-- a -->\none")).To(Succeed())
			Expect(c.UpsertComment(ctx, ref(number), "<!-- b -->", "<!-- b -->\ntwo")).To(Succeed())

			comments, err := emulator.Comments(ctx, "o/r", number)
			Expect(err).NotTo(HaveOccurred())
			Expect(comments).To(HaveLen(2))
		})

		It("creates its own comment rather than editing one another author marked", func() {
			number := issue()
			marker := "<!-- factory:test key=1 -->"

			Expect(emulator.CommentAs(ctx, "o/r", number, ghtest.AdminToken, marker+"\ntheirs")).To(Succeed())

			Expect(c.UpsertComment(ctx, ref(number), marker, marker+"\nours")).To(Succeed())

			Expect(p.Summary()).NotTo(ContainElement(ContainSubstring("PATCH /repos/o/r/issues/comments/")),
				"edited a comment foreman did not write")
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/comments")).To(ConsistOf(
				ContainSubstring("ours")))

			comments, err := emulator.Comments(ctx, "o/r", number)
			Expect(err).NotTo(HaveOccurred())
			Expect(comments).To(HaveLen(2))
			Expect(comments[0].Body).To(Equal(marker + "\ntheirs"))
		})
	})

	Describe("UpsertComment with a retro marker", func() {
		// The Reviewer's App writes its own retro, so its token owns the keyed comment.
		BeforeEach(func() {
			c = github.NewClient(p.URL(), "minted-token-reviewer", p.Client())
		})

		marker := func(number int) string { return workflow.RetroMarker(number, workflow.Reviewer, "abc") }
		body := func(number int, claim string) string {
			text, err := workflow.RenderRetro(marker(number),
				[]workflow.RetroFinding{{
					Target: workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"},
					Group:  "guide-misses-tests", Severity: workflow.SeverityLow,
					Claim: claim, Evidence: "e",
				}})
			Expect(err).NotTo(HaveOccurred())

			return text
		}

		It("creates one comment on the issue it worked, keyed by issue, agent role and run key", func() {
			number := issue()

			Expect(c.UpsertComment(ctx, ref(number), marker(number), body(number, "first"))).To(Succeed())
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/comments")).To(ConsistOf(
				ContainSubstring("<!-- factory:retro issue=" + itoa(number) + " role=reviewer key=abc -->")))
		})

		It("updates the keyed comment on a repeat, posting no second one", func() {
			number := issue()

			Expect(c.UpsertComment(ctx, ref(number), marker(number), body(number, "first"))).To(Succeed())
			Expect(c.UpsertComment(ctx, ref(number), marker(number), body(number, "again"))).To(Succeed())

			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/comments")).To(
				HaveLen(1), "posted a duplicate")

			comments, err := emulator.Comments(ctx, "o/r", number)
			Expect(err).NotTo(HaveOccurred())
			Expect(comments).To(HaveLen(1))
			Expect(p.Bodies("PATCH", "/repos/o/r/issues/comments/"+strconv.FormatInt(comments[0].ID, 10))).To(
				ConsistOf(ContainSubstring("again")))
		})
	})

	Describe("PostReview", func() {
		var pr ghtest.PullRequest

		BeforeEach(func() {
			pr = pullRequest()
		})

		It("posts an approval as a native review", func() {
			Expect(c.PostReview(ctx, ref(pr.Number), "sha", "body", workflow.Approve)).To(Succeed())
			Expect(p.Bodies("POST", reviewPath(pr))).To(ConsistOf(And(
				ContainSubstring(`"event":"APPROVE"`), ContainSubstring(`"commit_id":"sha"`),
			)))
		})

		It("posts a request for changes as a native review", func() {
			Expect(c.PostReview(ctx, ref(pr.Number), "sha", "body", workflow.RequestChanges)).To(Succeed())
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
			Expect(c.PostReview(ctx, ref(pr.Number), "sha", "body", workflow.Approve)).To(Succeed())

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

			Expect(c.PostReview(ctx, ref(pr.Number), "sha", "body", workflow.RequestChanges)).To(Succeed())
			Expect(p.Bodies("POST", reviewPath(pr))).To(ContainElement(ContainSubstring(`"event":"COMMENT"`)))
		})

		It("passes on any other error", func() {
			p.Status("POST /repos/o/r/pulls/"+itoa(pr.Number)+"/reviews", http.StatusInternalServerError)
			Expect(c.PostReview(ctx, ref(pr.Number), "sha", "b", workflow.Approve)).NotTo(Succeed())
		})
	})

	Describe("UpdatePR", func() {
		It("updates the pull request's title and body", func() {
			pr := pullRequest()

			Expect(c.UpdatePR(ctx, ref(pr.Number), "New", "New body")).To(Succeed())
			Expect(p.Bodies("PATCH", "/repos/o/r/pulls/"+itoa(pr.Number))).To(ConsistOf(And(
				ContainSubstring(`"title":"New"`), ContainSubstring(`"body":"New body"`),
			)))
		})
	})

	Describe("PostReplies", func() {
		It("comments the replies on the pull request, keyed by commit", func() {
			pr := pullRequest()

			Expect(c.UpsertComment(ctx, ref(pr.Number), workflow.RepliesMarker("sha"), "replies")).To(Succeed())
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(pr.Number)+"/comments")).To(ConsistOf(
				ContainSubstring("replies")))
		})

		It("updates the keyed comment instead of adding a second", func() {
			pr := pullRequest()
			body := workflow.RepliesMarker("sha") + "\nold"
			// The Implementer's App wrote the earlier replies: the record foreman may update.
			Expect(emulator.CommentAs(ctx, "o/r", pr.Number, "minted-token-implementer", body)).To(Succeed())

			Expect(c.UpsertComment(ctx, ref(pr.Number), workflow.RepliesMarker("sha"), "again")).To(Succeed())
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(pr.Number)+"/comments")).To(BeEmpty(), "posted a duplicate")
			Expect(p.Summary()).To(ContainElement(ContainSubstring("PATCH /repos/o/r/issues/comments/")))
		})
	})

	Describe("planning links", func() {
		It("reads a parent's sub-issues and links one by its database id", func() {
			parent := issue()
			child := issue()

			p.JSON("GET /repos/o/r/issues/"+itoa(parent)+"/sub_issues",
				[]map[string]any{{"number": child}})

			numbers, supported, err := c.SubIssueNumbers(ctx, ref(parent))
			Expect(err).NotTo(HaveOccurred())
			Expect(supported).To(BeTrue())
			Expect(numbers).To(Equal([]int{child}))

			id, err := c.IssueID(ctx, ref(child))
			Expect(err).NotTo(HaveOccurred())

			p.Status("POST /repos/o/r/issues/"+itoa(parent)+"/sub_issues", http.StatusCreated)
			Expect(c.AddSubIssue(ctx, ref(parent), id)).To(Succeed())
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(parent)+"/sub_issues")).To(ConsistOf(
				ContainSubstring(`"sub_issue_id":`)))
		})

		It("reads blockers and adds an edge by the blocker's database id", func() {
			child := issue()
			blocker := issue()

			p.JSON("GET /repos/o/r/issues/"+itoa(child)+"/dependencies/blocked_by",
				[]map[string]any{{"number": blocker}})

			numbers, supported, err := c.BlockedByNumbers(ctx, ref(child))
			Expect(err).NotTo(HaveOccurred())
			Expect(supported).To(BeTrue())
			Expect(numbers).To(Equal([]int{blocker}))

			id, err := c.IssueID(ctx, ref(blocker))
			Expect(err).NotTo(HaveOccurred())

			p.Status("POST /repos/o/r/issues/"+itoa(child)+"/dependencies/blocked_by", http.StatusCreated)
			Expect(c.AddBlockedBy(ctx, ref(child), id)).To(Succeed())

			raw := p.Bodies("POST", "/repos/o/r/issues/"+itoa(child)+"/dependencies/blocked_by")
			Expect(raw).To(ConsistOf(ContainSubstring(strconv.FormatInt(id, 10))))
		})

		It("treats an existing sub-issue link as already applied", func() {
			parent := issue()
			id, err := c.IssueID(ctx, ref(issue()))
			Expect(err).NotTo(HaveOccurred())

			p.Status("POST /repos/o/r/issues/"+itoa(parent)+"/sub_issues",
				http.StatusUnprocessableEntity)

			Expect(c.AddSubIssue(ctx, ref(parent), id)).To(Succeed())
		})

		It("treats an existing blocked-by edge as already applied", func() {
			child := issue()
			id, err := c.IssueID(ctx, ref(issue()))
			Expect(err).NotTo(HaveOccurred())

			p.Status("POST /repos/o/r/issues/"+itoa(child)+"/dependencies/blocked_by",
				http.StatusUnprocessableEntity)

			Expect(c.AddBlockedBy(ctx, ref(child), id)).To(Succeed())
		})
	})

	Describe("RequestReviewers", func() {
		It("asks the named people to review the pull request", func() {
			pr := pullRequest()

			Expect(c.RequestReviewers(ctx, ref(pr.Number), []string{"o"})).To(Succeed())
			Expect(p.Bodies("POST", "/repos/o/r/pulls/"+itoa(pr.Number)+"/requested_reviewers")).To(ConsistOf(
				ContainSubstring(`"reviewers":["o"]`)))
		})

		It("asks for nothing when there is no one to ask", func() {
			pr := pullRequest()

			Expect(c.RequestReviewers(ctx, ref(pr.Number), nil)).To(Succeed())
			Expect(p.Calls()).To(BeEmpty())
		})
	})

	Describe("Parent", func() {
		It("reads the ticket's parent issue", func() {
			number := issue()

			p.JSON("GET /repos/o/r/issues/"+itoa(number)+"/parent",
				map[string]any{"number": 9, "labels": []map[string]string{{"name": workflow.LabelMap}}})

			parent, supported, err := c.Parent(ctx, ref(number))
			Expect(err).NotTo(HaveOccurred())
			Expect(supported).To(BeTrue())
			Expect(parent.Number).To(Equal(9))
			Expect(parent.Labels).To(Equal([]string{workflow.LabelMap}))
		})

		It("reports that sub-issues are unavailable when the endpoint is missing", func() {
			number := issue()

			_, supported, err := c.Parent(ctx, ref(number))
			Expect(err).NotTo(HaveOccurred())
			Expect(supported).To(BeFalse())
		})

		It("passes on any other error", func() {
			number := issue()
			p.Status("GET /repos/o/r/issues/"+itoa(number)+"/parent", http.StatusInternalServerError)

			_, _, err := c.Parent(ctx, ref(number))
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("UpdateIssueBody", func() {
		It("replaces the issue's body", func() {
			number := issue()

			Expect(c.UpdateIssueBody(ctx, ref(number), "## Decisions so far\n\n- new\n")).To(Succeed())
			Expect(p.Bodies("PATCH", "/repos/o/r/issues/"+itoa(number))).To(ConsistOf(
				ContainSubstring(`"body":"## Decisions so far\n\n- new\n"`)))

			got, err := emulator.Issue(ctx, "o/r", number)
			Expect(err).NotTo(HaveOccurred())

			var body struct {
				Body string `json:"body"`
			}
			Expect(emulator.Do(ctx, http.MethodGet, "/repos/o/r/issues/"+itoa(got.Number),
				ghtest.AdminToken, nil, &body)).To(Succeed())
			Expect(body.Body).To(Equal("## Decisions so far\n\n- new\n"))
		})
	})

	Describe("PostResolution", func() {
		// The Researcher's App writes the resolution, so its token owns the marked comment.
		BeforeEach(func() {
			c = github.NewClient(p.URL(), "minted-token-researcher", p.Client())
		})

		It("posts the resolution once and updates it on a repeat", func() {
			number := issue()

			repo, err := workflow.ParseRepo("o/r")
			Expect(err).NotTo(HaveOccurred())

			body := workflow.RenderResolution(repo, "research/emulate", "It works.")
			Expect(c.UpsertComment(ctx, ref(number), workflow.ResearchMarker("research/emulate"), body)).To(Succeed())
			Expect(c.UpsertComment(ctx, ref(number), workflow.ResearchMarker("research/emulate"), body+"\n\nmore")).To(Succeed())

			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/comments")).To(ConsistOf(
				ContainSubstring(workflow.ResearchMarker("research/emulate"))))
			Expect(p.Summary()).To(ContainElement(ContainSubstring("PATCH /repos/o/r/issues/comments/")))
		})
	})

	Describe("CloseResolved", func() {
		transition := workflow.Transition{
			Role: workflow.Researcher, RunID: "run-3",
			From: workflow.Researching, To: workflow.Closed,
		}

		// The marker is posted by the Researcher's App, so a repeat is one the factory authored.
		BeforeEach(func() {
			c = github.NewClient(p.URL(), "minted-token-researcher", p.Client())
		})

		It("posts the marker, removes the claim and closes the ticket", func() {
			number := issue(workflow.LabelResearching)

			Expect(c.CloseResolved(ctx, ref(number), transition)).To(Succeed())
			Expect(p.Summary()).To(Equal([]string{
				"GET /repos/o/r/issues/" + itoa(number) + "/comments",
				"POST /repos/o/r/issues/" + itoa(number) + "/comments",
				"DELETE /repos/o/r/issues/" + itoa(number) + "/labels/factory:researching",
				"PATCH /repos/o/r/issues/" + itoa(number),
			}))
			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/comments")[0]).To(
				ContainSubstring("<!-- factory:handoff role=researcher run=run-3 " +
					"from=factory:researching to=closed sha= -->"))

			got, err := emulator.Issue(ctx, "o/r", number)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.State).To(Equal("closed"))
		})

		It("records the same close once, however many times it is applied", func() {
			number := issue(workflow.LabelResearching)

			Expect(c.CloseResolved(ctx, ref(number), transition)).To(Succeed())
			Expect(c.CloseResolved(ctx, ref(number), transition)).To(Succeed())

			Expect(p.Bodies("POST", "/repos/o/r/issues/"+itoa(number)+"/comments")).To(HaveLen(1),
				"posted the same transition twice")
		})

		It("tolerates a label already removed", func() {
			number := issue()

			Expect(c.CloseResolved(ctx, ref(number), transition)).To(Succeed())
		})

		It("passes on any other delete error", func() {
			number := issue(workflow.LabelResearching)
			p.Status("DELETE /repos/o/r/issues/"+itoa(number)+"/labels/factory:researching",
				http.StatusBadGateway)

			Expect(c.CloseResolved(ctx, ref(number), transition)).NotTo(Succeed())
		})
	})
})

// reviewPath is a pull request's reviews endpoint.
func reviewPath(pr ghtest.PullRequest) string {
	return "/repos/o/r/pulls/" + itoa(pr.Number) + "/reviews"
}

// itoa formats a number for a URL.
func itoa(n int) string { return strconv.Itoa(n) }
