package foreman

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("Poll", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
	})

	// poll polls for the Implementer's and the Reviewer's queues.
	poll := func() (int, error) {
		return Poll(context.Background(), h.deps, []workflow.Role{workflow.Implementer, workflow.Reviewer})
	}

	// expectNothingReady fails the spec unless the poll found nothing and said so.
	expectNothingReady := func() {
		GinkgoHelper()
		code, err := poll()
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(BeZero())
		Expect(h.out.String()).To(Equal("nothing ready\n"))
	}

	It("claims reviews before work", func() {
		h.queue(map[string][]map[string]any{
			workflow.LabelReviewReady: {{"number": 12}},
			workflow.LabelReady:       {{"number": 3}},
		})
		h.acceptWrites("12")
		Expect(poll()).To(BeZero())
		Expect(h.gh.Summary()).To(Equal([]string{
			"GET /repos/o/r/issues",
			"DELETE /repos/o/r/issues/12/labels/ready-for-agent-review",
			"POST /repos/o/r/issues/12/labels",
			"POST /repos/o/r/issues/12/comments",
		}))
		Expect(h.gh.Bodies("POST", "/repos/o/r/issues/12/labels")[0]).To(ContainSubstring(workflow.LabelReviewing))
		Expect(h.gh.Bodies("POST", "/repos/o/r/issues/12/comments")[0]).To(
			ContainSubstring("Picked up for review by the factory agent."))
		enq := h.runner.find("dagu", "enqueue")
		Expect(enq).To(HaveLen(1))
		Expect(enq[0].Argv).To(Equal([]string{
			"/usr/local/bin/dagu", "enqueue", "factory-reviewer", "--", "REPO=o/r", "ISSUE=12",
		}))
	})

	It("claims work when no review waits, skipping pull requests", func() {
		h.queue(map[string][]map[string]any{
			workflow.LabelReady: {{"number": 4, "pull_request": map[string]any{}}, {"number": 5}},
		})
		h.acceptWrites("5")
		Expect(poll()).To(BeZero())
		Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/labels")[0]).To(ContainSubstring(workflow.LabelWorking))
		Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")[0]).To(ContainSubstring("Picked up by the factory agent."))
		enq := h.runner.find("enqueue")
		Expect(enq).To(HaveLen(1))
		Expect(enq[0].Argv).To(ContainElements("factory-implementer", "ISSUE=5"))
	})

	It("writes nothing when nothing is ready", func() {
		h.queue(nil)
		expectNothingReady()
		h.expectOnlyReads()
		Expect(h.runner.find("enqueue")).To(BeEmpty())
	})

	It("stops quietly after losing the claim", func() {
		h.queue(map[string][]map[string]any{workflow.LabelReady: {{"number": 5}}})
		h.gh.Status("DELETE /repos/o/r/issues/5/labels/{label}", http.StatusNotFound)
		Expect(poll()).To(BeZero())
		Expect(h.out.String()).To(Equal("issue 5 was claimed by someone else\n"))
		Expect(h.runner.find("enqueue")).To(BeEmpty())
		Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")).To(BeEmpty())
	})

	It("fails the issue when its run cannot be queued", func() {
		h.queue(map[string][]map[string]any{workflow.LabelReady: {{"number": 5}}})
		h.acceptWrites("5")
		h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
			if strings.Contains(joined(c), "enqueue") {
				return "", errors.New("dagu down")
			}
			return base(c)
		})
		Expect(poll()).To(Equal(1))
		Expect(h.errb.String()).To(ContainSubstring("dagu down"))
		labels := h.gh.Bodies("POST", "/repos/o/r/issues/5/labels")
		Expect(labels).To(HaveLen(2))
		Expect(labels[1]).To(ContainSubstring(workflow.LabelFailed))
		Expect(h.gh.Summary()).To(ContainElement("DELETE /repos/o/r/issues/5/labels/agent-working"), "did not release the claim")
		Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")[1]).To(ContainSubstring("Could not queue the agent run"))
	})

	It("claims with the owning agent role's repo-scoped token", func() {
		h.queue(map[string][]map[string]any{workflow.LabelReviewReady: {{"number": 12}}})
		h.acceptWrites("12")
		Expect(poll()).Error().NotTo(HaveOccurred())
		mint := h.runner.find("app-token", " token ")
		Expect(mint).To(HaveLen(1))
		Expect(mint[0].Argv).To(Equal([]string{"/usr/local/bin/github-app-token", "token", "--app", "reviewer", "--repo", "o/r"}))
		Expect(mint[0].Capture).To(BeTrue())
		h.expectAuthorizedAs(workflow.Reviewer)
	})

	It("asks every configured App where it is installed", func() {
		h.queue(nil)
		Expect(poll()).Error().NotTo(HaveOccurred())
		var apps []string
		for _, c := range h.runner.find("app-token", " repositories ") {
			apps = append(apps, appOf(c))
		}
		Expect(apps).To(Equal([]string{"implementer", "reviewer"}))
	})

	It("ignores a repository no factory App is installed on", func() {
		h.installed = map[workflow.Role][]string{}
		expectNothingReady()
		Expect(h.gh.Calls()).To(BeEmpty())
		Expect(h.runner.find(" token ")).To(BeEmpty())
	})

	It("polls each repository for the agent roles installed on it", func() {
		h.installed = map[workflow.Role][]string{workflow.Implementer: {"o/a"}, workflow.Reviewer: {"o/b", "o/a"}}
		h.queueRepo("o/a", map[string][]map[string]any{})
		h.queueRepo("o/b", map[string][]map[string]any{})
		Expect(poll()).Error().NotTo(HaveOccurred())
		Expect(h.gh.Summary()).To(Equal([]string{"GET /repos/o/a/issues", "GET /repos/o/b/issues", "GET /repos/o/a/issues"}))
	})

	It("works only the agent roles installed on a repository; others' issues wait", func() {
		h.installed = map[workflow.Role][]string{workflow.Implementer: {"o/r"}}
		h.queue(map[string][]map[string]any{
			workflow.LabelReviewReady: {{"number": 12}},
			workflow.LabelReady:       {{"number": 3}},
		})
		h.acceptWrites("3")
		Expect(poll()).To(BeZero())
		Expect(h.gh.Summary()).To(Equal([]string{
			"GET /repos/o/r/issues",
			"DELETE /repos/o/r/issues/3/labels/ready-for-agent",
			"POST /repos/o/r/issues/3/labels",
			"POST /repos/o/r/issues/3/comments",
		}))
	})

	It("waits quietly when the owning App is missing: no comment and no label", func() {
		h.installed = map[workflow.Role][]string{workflow.Implementer: {"o/r"}}
		h.queue(map[string][]map[string]any{workflow.LabelReviewReady: {{"number": 12}}})
		expectNothingReady()
		h.expectOnlyReads()
	})

	It("treats a repository whose token cannot be minted as unassigned for that agent role", func() {
		h.failMint(workflow.Reviewer, true)
		h.queue(map[string][]map[string]any{
			workflow.LabelReviewReady: {{"number": 12}},
			workflow.LabelReady:       {{"number": 3}},
		})
		h.acceptWrites("3")
		Expect(poll()).To(BeZero())
		Expect(h.gh.Summary()).To(And(HaveLen(4), ContainElement("DELETE /repos/o/r/issues/3/labels/ready-for-agent")))
		Expect(h.errb.String()).To(ContainSubstring("reviewer"))
	})

	It("treats an App that cannot list its installations as installed nowhere", func() {
		h.installed = map[workflow.Role][]string{workflow.Reviewer: {"o/r"}}
		h.failMint(workflow.Reviewer, false)
		h.queue(map[string][]map[string]any{workflow.LabelReviewReady: {{"number": 12}}})
		expectNothingReady()
		Expect(h.gh.Calls()).To(BeEmpty())
		Expect(h.runner.find("--app reviewer", " token ")).To(BeEmpty())
	})

	It("counts only owner/name lines as installations", func() {
		h.installed = map[workflow.Role][]string{workflow.Implementer: {"../etc", "not a repo", ""}}
		Expect(poll()).To(BeZero())
		Expect(h.gh.Calls()).To(BeEmpty())
	})

	It("lists the queues by label, reviews first", func() {
		h.queue(nil)
		Expect(poll()).Error().NotTo(HaveOccurred())
		var labels []string
		for _, r := range h.gh.Calls() {
			v, err := url.ParseQuery(r.Query)
			Expect(err).NotTo(HaveOccurred())
			labels = append(labels, v.Get("labels"))
		}
		Expect(labels).To(Equal([]string{workflow.LabelReviewReady, workflow.LabelReady}))
	})
})
