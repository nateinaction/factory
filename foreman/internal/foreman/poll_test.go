package foreman

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/ghtest"
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

	// onlyGetter fails the spec when a GitHub call other than a GET reached the repository.
	expectNoComments := func(number int) {
		GinkgoHelper()
		Expect(h.gh.Bodies("POST", fmt.Sprintf("/repos/o/r/issues/%d/comments", number))).To(BeEmpty())
	}

	Describe("the factory labels", func() {
		It("creates every factory label on first sight, idempotently", func() {
			Expect(poll()).Error().NotTo(HaveOccurred())

			created := h.gh.Bodies("POST", "/repos/o/r/labels")
			Expect(created).To(HaveLen(len(workflow.FactoryLabels())))

			for _, body := range created {
				Expect(body).To(ContainSubstring(`"name":"factory:`))
			}

			labels, err := emulator.Labels(context.Background(), "o/r")
			Expect(err).NotTo(HaveOccurred())
			Expect(labels).To(ConsistOf(workflow.FactoryLabels()))

			before := len(h.gh.Bodies("POST", "/repos/o/r/labels"))

			Expect(poll()).Error().NotTo(HaveOccurred())
			Expect(h.gh.Bodies("POST", "/repos/o/r/labels")).To(HaveLen(before), "created a label a second time")
		})

		It("never writes a triage or wayfinder label", func() {
			Expect(poll()).Error().NotTo(HaveOccurred())

			for _, body := range h.gh.Bodies("POST", "/repos/o/r/labels") {
				Expect(body).NotTo(Or(
					ContainSubstring("needs-triage"), ContainSubstring("ready-for-"),
					ContainSubstring("wayfinder:"),
				))
			}
		})
	})

	Describe("routing a new issue", func() {
		DescribeTable("puts a ticket in its first queue",
			func(labels []string, want string) {
				number := h.createAs("o", labels, "A ticket")

				expectNothingReady()

				Expect(h.labels(number)).To(ContainElement(want))
			},
			Entry("a research ticket", []string{"wayfinder:research"}, workflow.LabelResearch),
			Entry("a grilling ticket", []string{"wayfinder:grilling"}, workflow.LabelPlan),
			Entry("a prototype ticket", []string{"prototype"}, workflow.LabelPlan),
			Entry("a task", []string{"task"}, workflow.LabelPlan),
			Entry("anything else", []string{"bug"}, workflow.LabelPlan),
		)

		DescribeTable("never routes",
			func(labels []string, title string) {
				number := h.createAs("o", labels, title)

				expectNothingReady()
				Expect(h.factoryLabels(number)).To(BeEmpty())
			},
			Entry("a wayfinder map", []string{"wayfinder:map"}, "A map"),
			Entry("the retro dashboard", nil, workflow.DashboardTitle),
		)

		It("routes an issue only after a writer's comment when a non-writer opened it", func() {
			number := h.createAs("factory", nil, "A promoted group")

			expectNothingReady()
			Expect(h.labels(number)).To(BeEmpty(), "routed a non-writer's issue")

			h.comment(number, "o", "please work on this")

			h.out.Reset()
			Expect(poll()).Error().NotTo(HaveOccurred())
			Expect(h.labels(number)).To(ContainElement(workflow.LabelPlan))
		})

		It("fails closed when it cannot read an author's permission", func() {
			number := h.createAs("o", nil, "A ticket")
			h.gh.Status("GET /repos/o/r/collaborators/o/permission", http.StatusInternalServerError)

			expectNothingReady()
			Expect(h.labels(number)).To(BeEmpty(), "routed an issue whose author it could not read")
			Expect(h.logs.String()).To(ContainSubstring("failing closed"))
		})
	})

	Describe("claiming", func() {
		It("posts the marker first, swaps the labels, reads the claim back and queues the run", func() {
			h.queue(map[string][]map[string]any{
				workflow.LabelImplement: {{"number": 5}},
			})

			code, err := poll()
			Expect(err).NotTo(HaveOccurred())
			Expect(code).To(BeZero())

			markers := h.markerComments(5)
			Expect(markers).To(HaveLen(1))
			Expect(markers[0]).To(Equal(
				"<!-- factory:handoff role=implementer run=run-1 " +
					"from=factory:implement to=factory:implementing sha= -->"))

			Expect(h.gh.Summary()).To(ContainElements(
				"DELETE /repos/o/r/issues/5/labels/factory:implement",
				"POST /repos/o/r/issues/5/labels",
				"GET /repos/o/r/issues/5",
			))
			Expect(h.labels(5)).To(ContainElement(workflow.LabelImplementing))

			enq := h.runner.find("enqueue")
			Expect(enq).To(HaveLen(1))
			Expect(enq[0].Argv).To(Equal([]string{
				"/usr/local/bin/dagu", "enqueue", "factory-implementer", "--", "REPO=o/r", "ISSUE=5",
			}))
		})

		It("claims reviews before work, with the owning agent role's token", func() {
			h.queue(map[string][]map[string]any{
				workflow.LabelReview:    {{"number": 12}},
				workflow.LabelImplement: {{"number": 3}},
			})

			Expect(poll()).Error().NotTo(HaveOccurred())
			Expect(h.runner.find("enqueue")).To(HaveLen(1))
			Expect(joined(h.runner.find("enqueue")[0])).To(ContainSubstring("factory-reviewer"))
			Expect(h.markerComments(12)).To(HaveLen(1))
			Expect(h.markerComments(3)).To(BeEmpty())
			h.expectAuthorizedAs(workflow.Reviewer)
		})

		It("records the reviewed commit in the claim marker", func() {
			h.queue(map[string][]map[string]any{workflow.LabelReview: {{"number": 5}}})
			h.implementerGH(map[string]any{}, []workflow.Item{})

			Expect(poll()).Error().NotTo(HaveOccurred())
			Expect(h.markerComments(5)[0]).To(MatchRegexp(`sha=[0-9a-f]{40} -->`))
		})

		It("skips a paused issue in any state, and resumes it where it was when the label goes", func() {
			h.queue(map[string][]map[string]any{
				workflow.LabelImplement: {{"number": 5}},
			})
			Expect(emulator.Do(context.Background(), http.MethodPost, "/repos/o/r/issues/5/labels",
				ghtest.AdminToken, map[string]any{"labels": []string{workflow.LabelPaused}}, nil)).To(Succeed())

			expectNothingReady()
			Expect(h.runner.find("enqueue")).To(BeEmpty())
			Expect(h.labels(5)).To(ContainElement(workflow.LabelImplement))

			Expect(emulator.RemoveLabel(context.Background(), "o/r", 5, workflow.LabelPaused)).To(Succeed())

			h.out.Reset()
			Expect(poll()).Error().NotTo(HaveOccurred())
			Expect(h.runner.find("enqueue")).To(HaveLen(1))
		})

		It("does not claim an issue that already carries a running label", func() {
			h.queue(map[string][]map[string]any{
				workflow.LabelImplement: {{"number": 5}, {"number": 6}},
			})
			Expect(emulator.Do(context.Background(), http.MethodPost, "/repos/o/r/issues/5/labels",
				ghtest.AdminToken, map[string]any{"labels": []string{workflow.LabelImplementing}}, nil)).To(Succeed())

			Expect(poll()).Error().NotTo(HaveOccurred())
			Expect(h.runner.find("enqueue")).To(HaveLen(1))
			Expect(joined(h.runner.find("enqueue")[0])).To(ContainSubstring("ISSUE=6"))
		})

		It("leaves exactly one run when two polls claim the same issue", func() {
			h.queue(map[string][]map[string]any{workflow.LabelImplement: {{"number": 5}}})

			deps := []Deps{h.deps, h.deps}
			deps[0].Out = &bytes.Buffer{}
			deps[1].Out = &bytes.Buffer{}
			deps[0].Log = slog.New(slog.DiscardHandler)
			deps[1].Log = slog.New(slog.DiscardHandler)

			var wg sync.WaitGroup
			wg.Add(2)

			for _, d := range deps {
				go func(d Deps) {
					defer GinkgoRecover()
					defer wg.Done()

					_, err := Poll(context.Background(), d, []workflow.Role{workflow.Implementer, workflow.Reviewer})
					Expect(err).NotTo(HaveOccurred())
				}(d)
			}

			wg.Wait()
			Expect(h.runner.find("enqueue")).To(HaveLen(1))
			Expect(h.labels(5)).To(ContainElement(workflow.LabelImplementing))
		})

		It("fails the issue when its run cannot be queued", func() {
			h.queue(map[string][]map[string]any{workflow.LabelImplement: {{"number": 5}}})
			h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
				if strings.Contains(joined(c), "enqueue") {
					return "", errors.New("dagu down")
				}

				return base(c)
			})

			Expect(poll()).To(Equal(1))
			Expect(h.logs.String()).To(ContainSubstring("dagu down"))
			Expect(h.labels(5)).To(ContainElement(workflow.LabelFailed))

			markers := h.markerComments(5)
			Expect(markers).To(HaveLen(2))
			Expect(markers[1]).To(ContainSubstring("from=factory:implementing to=factory:failed"))
			Expect(h.gh.Summary()).To(ContainElement("DELETE /repos/o/r/issues/5/labels/factory:implementing"))
		})

		It("claims with the owning agent role's repo-scoped token", func() {
			h.queue(map[string][]map[string]any{workflow.LabelReview: {{"number": 12}}})

			Expect(poll()).Error().NotTo(HaveOccurred())

			mint := h.runner.find("app-token", " token ")
			Expect(mint).NotTo(BeEmpty())
			h.expectAuthorizedAs(workflow.Reviewer)
		})
	})

	Describe("recovery", func() {
		It("re-applies the state the latest marker names when the issue does not carry it", func() {
			h.queue(map[string][]map[string]any{workflow.LabelImplement: {{"number": 5}}})

			marker, err := workflow.RenderTransition(workflow.Transition{
				Role: workflow.Implementer, RunID: "old-run",
				From: workflow.Implement, To: workflow.Implementing,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(emulator.CommentAs(context.Background(), "o/r", 5, "secret-token", marker)).To(Succeed())

			Expect(poll()).Error().NotTo(HaveOccurred())

			Expect(h.labels(5)).To(ContainElements(workflow.LabelImplement, workflow.LabelImplementing))
			Expect(h.runner.find("enqueue")).To(BeEmpty(), "ran an issue the marker says is claimed")
			Expect(h.logs.String()).To(ContainSubstring("repairing a stranded issue"))
		})

		It("does not repair an issue that already carries the state", func() {
			h.queue(map[string][]map[string]any{workflow.LabelImplement: {{"number": 5}}})

			marker, err := workflow.RenderTransition(workflow.Transition{
				Role: workflow.Implementer, RunID: "old-run",
				From: workflow.Implement, To: workflow.Implementing,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(emulator.CommentAs(context.Background(), "o/r", 5, "secret-token", marker)).To(Succeed())
			Expect(emulator.Do(context.Background(), http.MethodPost, "/repos/o/r/issues/5/labels",
				ghtest.AdminToken, map[string]any{"labels": []string{workflow.LabelImplementing}}, nil)).To(Succeed())

			expectNothingReady()
			Expect(h.labels(5)).To(ConsistOf(workflow.LabelImplement, workflow.LabelImplementing))
			Expect(h.runner.find("enqueue")).To(BeEmpty())
		})
	})

	Describe("blockers", func() {
		It("does not claim an issue with an open blocker, and claims it once the blocker closes", func() {
			h.queue(map[string][]map[string]any{
				workflow.LabelImplement: {{"number": 3, "body": "Blocked by: #2\n\nDo it"}},
			})

			expectNothingReady()
			Expect(h.runner.find("enqueue")).To(BeEmpty())
			Expect(h.labels(3)).To(ContainElement(workflow.LabelImplement))

			Expect(emulator.CloseIssue(context.Background(), "o/r", 2)).To(Succeed())

			h.out.Reset()
			Expect(poll()).Error().NotTo(HaveOccurred())
			Expect(h.runner.find("enqueue")).To(HaveLen(1))
		})

		It("reads native dependencies first", func() {
			h.queue(map[string][]map[string]any{
				workflow.LabelImplement: {{"number": 5, "body": "No fallback line"}},
			})
			h.gh.JSON("GET /repos/o/r/issues/5/dependencies/blocked_by",
				[]map[string]any{{"number": 2, "state": "open"}})

			expectNothingReady()
			Expect(h.runner.find("enqueue")).To(BeEmpty())
		})

		It("does not claim an issue whose blockers cannot be read", func() {
			h.queue(map[string][]map[string]any{
				workflow.LabelImplement: {{"number": 3, "body": "Blocked by: #2"}},
			})
			h.gh.Status("GET /repos/o/r/issues/3/dependencies/blocked_by", http.StatusInternalServerError)

			expectNothingReady()
			Expect(h.runner.find("enqueue")).To(BeEmpty())
			Expect(h.logs.String()).To(ContainSubstring("blockers cannot be read"))
		})
	})

	Describe("assignment and identity", func() {
		It("asks every configured App where it is installed", func() {
			Expect(poll()).Error().NotTo(HaveOccurred())

			lists := h.runner.find("app-token", " repositories ")

			apps := make([]string, 0, len(lists))
			for _, c := range lists {
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

			calls := h.gh.Calls()
			paths := make([]string, 0, len(calls))

			for _, call := range calls {
				paths = append(paths, call.Method+" "+call.Path)
			}

			Expect(paths).To(ContainElement("GET /repos/o/a/labels"))
			Expect(paths).To(ContainElement("GET /repos/o/b/labels"))
		})

		It("waits quietly when the owning App is missing: no comment and no label", func() {
			h.installed = map[workflow.Role][]string{workflow.Implementer: {"o/r"}}
			h.queue(map[string][]map[string]any{workflow.LabelReview: {{"number": 12}}})

			expectNothingReady()
			expectNoComments(12)
			Expect(h.runner.find("enqueue")).To(BeEmpty())
		})

		It("treats a repository whose token cannot be minted as unassigned for that agent role", func() {
			h.failMint(workflow.Reviewer, true)
			h.queue(map[string][]map[string]any{
				workflow.LabelReview:    {{"number": 12}},
				workflow.LabelImplement: {{"number": 3}},
			})

			Expect(poll()).Error().NotTo(HaveOccurred())
			Expect(h.runner.find("enqueue")).To(HaveLen(1))
			Expect(joined(h.runner.find("enqueue")[0])).To(ContainSubstring("factory-implementer"))
			Expect(h.logs.String()).To(And(
				ContainSubstring("WARN cannot mint token"),
				ContainSubstring("role=reviewer"),
				ContainSubstring("repo=o/r"),
			))
		})

		It("treats an App that cannot list its installations as installed nowhere", func() {
			h.installed = map[workflow.Role][]string{workflow.Reviewer: {"o/r"}}
			h.failMint(workflow.Reviewer, false)
			h.queue(map[string][]map[string]any{workflow.LabelReview: {{"number": 12}}})

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
			Expect(poll()).Error().NotTo(HaveOccurred())

			var labels []string

			for _, call := range h.gh.Calls() {
				v, err := url.ParseQuery(call.Query)
				Expect(err).NotTo(HaveOccurred())

				if label := v.Get("labels"); label != "" {
					labels = append(labels, label)
				}
			}

			Expect(labels).To(Equal([]string{workflow.LabelReview, workflow.LabelImplement}))
		})
	})
})
