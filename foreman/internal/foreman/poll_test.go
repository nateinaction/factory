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
	"time"

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
		return Poll(context.Background(), h.deps, pollConfig(workflow.Implementer, workflow.Reviewer))
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

		It("reads each login's permission once for the whole poll", func() {
			number := h.createAs("factory", nil, "A promoted group")
			h.comment(number, "factory", "first")
			h.comment(number, "factory", "second")
			h.comment(number, "o", "please work on this")

			permissionReads := func(login string) int {
				reads := 0

				for _, call := range h.gh.Summary() {
					if call == "GET /repos/o/r/collaborators/"+login+"/permission" {
						reads++
					}
				}

				return reads
			}

			Expect(poll()).Error().NotTo(HaveOccurred())
			Expect(h.labels(number)).To(ContainElement(workflow.LabelPlan))

			Expect(permissionReads("factory")).To(Equal(1), "read a non-writer's permission more than once")
			Expect(permissionReads("o")).To(Equal(1), "read a writer's permission more than once")
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

					_, err := Poll(context.Background(), d, pollConfig(workflow.Implementer, workflow.Reviewer))
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

		It("claims a planning issue though the Planner's App cannot read pull requests", func() {
			h.installed = map[workflow.Role][]string{workflow.Planner: {"o/r"}}
			h.queue(map[string][]map[string]any{workflow.LabelPlan: {{"number": 1}}})
			// The Planner has no Pull requests permission: GitHub answers its pull request reads with 403.
			h.gh.Handle("GET /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer minted-token-"+string(workflow.Planner) {
					w.WriteHeader(http.StatusForbidden)

					return
				}

				h.gh.Forward(w, r)
			})

			code, err := Poll(context.Background(), h.deps, pollConfig(workflow.Planner))
			Expect(err).NotTo(HaveOccurred())
			Expect(code).To(BeZero())
			Expect(h.labels(1)).To(ContainElement(workflow.LabelPlanning))
			Expect(h.runner.find("enqueue")).To(HaveLen(1))
		})
	})

	Describe("recovery", func() {
		// strand seeds o/r#5 carrying label with a marker from the Implementer that names to.
		strand := func(label string, to workflow.State) {
			GinkgoHelper()
			h.queue(map[string][]map[string]any{label: {{"number": 5}}})

			marker, err := workflow.RenderTransition(workflow.Transition{
				Role: workflow.Implementer, RunID: "old-run", From: workflow.State(label), To: to,
			})
			Expect(err).NotTo(HaveOccurred())
			h.comment(5, appLogin(workflow.Implementer), marker)
		}

		// labelWrites counts the calls after the first n that write o/r#5's labels.
		labelWrites := func(n int) int {
			GinkgoHelper()

			writes := 0

			for _, call := range h.gh.Calls()[n:] {
				if call.Path == "/repos/o/r/issues/5/labels" || strings.HasPrefix(call.Path, "/repos/o/r/issues/5/labels/") {
					writes++
				}
			}

			return writes
		}

		It("re-applies the state the latest marker names when the issue does not carry it", func() {
			strand(workflow.LabelImplement, workflow.Implementing)

			Expect(poll()).Error().NotTo(HaveOccurred())

			Expect(h.labels(5)).To(ConsistOf(workflow.LabelImplementing), "left a state the marker does not name")
			Expect(h.runner.find("enqueue")).To(BeEmpty(), "ran an issue the marker says is claimed")
			Expect(h.logs.String()).To(ContainSubstring("repairing a stranded issue"))
		})

		It("leaves exactly the state label its latest marker names", func() {
			// A finish whose marker landed but whose running label stayed on leaves two states.
			strand(workflow.LabelImplementing, workflow.Done)

			Expect(poll()).Error().NotTo(HaveOccurred())

			Expect(h.factoryLabels(5)).To(ConsistOf(workflow.LabelDone))
		})

		It("leaves exactly the label a failed finish names", func() {
			strand(workflow.LabelReviewing, workflow.Failed)

			Expect(poll()).Error().NotTo(HaveOccurred())

			Expect(h.factoryLabels(5)).To(ConsistOf(workflow.LabelFailed))
		})

		It("keeps a paused overlay while it repairs", func() {
			// Paused is not a state: a person's overlay is never removed by recovery.
			strand(workflow.LabelImplementing, workflow.Done)
			Expect(emulator.Do(context.Background(), http.MethodPost, "/repos/o/r/issues/5/labels",
				ghtest.AdminToken, map[string]any{"labels": []string{workflow.LabelPaused}}, nil)).To(Succeed())

			expectNothingReady()

			Expect(h.factoryLabels(5)).To(ConsistOf(workflow.LabelPaused, workflow.LabelImplementing),
				"repaired a paused issue")
		})

		It("changes nothing when it recovers the same issue twice", func() {
			strand(workflow.LabelImplementing, workflow.Done)

			Expect(poll()).Error().NotTo(HaveOccurred())
			Expect(h.factoryLabels(5)).To(ConsistOf(workflow.LabelDone))

			before := len(h.gh.Calls())

			Expect(poll()).Error().NotTo(HaveOccurred())

			Expect(labelWrites(before)).To(BeZero(), "wrote a label on the second recovery")
			Expect(h.factoryLabels(5)).To(ConsistOf(workflow.LabelDone))
		})

		It("re-applies the state with the token of the agent role that made the transition", func() {
			h.queue(map[string][]map[string]any{workflow.LabelImplement: {{"number": 5}}})

			marker, err := workflow.RenderTransition(workflow.Transition{
				Role: workflow.Implementer, RunID: "old-run",
				From: workflow.Implement, To: workflow.Implementing,
			})
			Expect(err).NotTo(HaveOccurred())
			h.comment(5, appLogin(workflow.Implementer), marker)

			Expect(poll()).Error().NotTo(HaveOccurred())
			Expect(h.labels(5)).To(ContainElement(workflow.LabelImplementing))

			// The poll reads as the Reviewer, so only the repair itself names the Implementer.
			repaired := false

			for _, call := range h.gh.Calls() {
				if call.Method == http.MethodPost && call.Path == "/repos/o/r/issues/5/labels" {
					repaired = true

					Expect(call.Header.Get("Authorization")).To(Equal("Bearer minted-token-implementer"),
						"repaired with a token other than the owning agent role's")
				}
			}

			Expect(repaired).To(BeTrue(), "the stranded issue was not repaired")
		})

		It("leaves the issue stranded when the owning agent role's token cannot be minted", func() {
			h.queue(map[string][]map[string]any{workflow.LabelImplement: {{"number": 5}}})

			marker, err := workflow.RenderTransition(workflow.Transition{
				Role: workflow.Implementer, RunID: "old-run",
				From: workflow.Implement, To: workflow.Implementing,
			})
			Expect(err).NotTo(HaveOccurred())
			h.comment(5, appLogin(workflow.Implementer), marker)
			h.failMint(workflow.Implementer, true)

			Expect(poll()).Error().NotTo(HaveOccurred())
			Expect(h.labels(5)).NotTo(ContainElement(workflow.LabelImplementing),
				"repaired a transition it could not mint the owning role's token for")
			Expect(h.gh.Bodies("POST", "/repos/o/r/issues/5/labels")).To(BeEmpty())
			Expect(h.logs.String()).To(ContainSubstring("skipping an issue the poll cannot read"))
		})

		It("does not repair an issue that already carries only the state", func() {
			strand(workflow.LabelImplementing, workflow.Implementing)

			expectNothingReady()
			Expect(h.factoryLabels(5)).To(ConsistOf(workflow.LabelImplementing))
			Expect(labelWrites(0)).To(BeZero(), "wrote a label for an issue already in its state")
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

		It("does not claim an issue whose fallback blocker cannot be read", func() {
			h.queue(map[string][]map[string]any{
				workflow.LabelImplement: {{"number": 3, "body": "Blocked by: #2"}},
			})
			// A repository without native dependencies falls back to the issue body.
			h.gh.Status("GET /repos/o/r/issues/3/dependencies/blocked_by", http.StatusNotFound)
			h.gh.Status("GET /repos/o/r/issues/2", http.StatusInternalServerError)

			expectNothingReady()
			Expect(h.runner.find("enqueue")).To(BeEmpty())
			Expect(h.logs.String()).To(ContainSubstring("blockers cannot be read"))
		})
	})

	Describe("scheduling across hosts", func() {
		// busyDagu answers the poll's read-only dagu history with runs.
		busyDagu := func(runs string) {
			h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
				if len(c.Argv) > 1 && c.Argv[1] == "history" {
					return runs, nil
				}

				return base(c)
			})
		}

		// capped polls with the agent role's run capacity, which testConfig leaves at one.
		capped := func(role workflow.Role, capacity int) (int, error) {
			cfg := pollConfig(workflow.Implementer)
			cfg.RoleCapacity[role] = capacity

			return Poll(context.Background(), h.deps, cfg)
		}

		It("leaves an agent role queued while its DAG has a run waiting for a worker", func() {
			h.queue(map[string][]map[string]any{
				workflow.LabelImplement: {{"number": 5}},
			})
			busyDagu(`[{"name":"factory-implementer","dagRunId":"r1","status":"queued"}]`)

			code, err := capped(workflow.Implementer, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(code).To(BeZero())
			Expect(h.runner.find("enqueue")).To(BeEmpty(), "claimed while the role's DAG was busy")
			Expect(h.labels(5)).To(ContainElement(workflow.LabelImplement), "a waiting issue lost its queued label")
			Expect(h.markerComments(5)).To(BeEmpty())
			Expect(h.logs.String()).To(ContainSubstring("while its DAG is busy"))
		})

		It("claims another run while one is in progress and the run cap has room", func() {
			h.queue(map[string][]map[string]any{
				workflow.LabelImplement: {{"number": 5}, {"number": 6}},
			})
			busyDagu(`[{"name":"factory-implementer","dagRunId":"r1","status":"running"}]`)

			code, err := capped(workflow.Implementer, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(code).To(BeZero())

			enqueued := h.runner.find("enqueue")
			Expect(enqueued).To(HaveLen(1))
			Expect(joined(enqueued[0])).To(ContainSubstring("ISSUE=5"), "a free slot went to the newest issue")
			Expect(h.labels(6)).To(ContainElement(workflow.LabelImplement), "claimed past the run cap")
		})

		It("claims up to the run cap's free capacity in one poll", func() {
			h.queue(map[string][]map[string]any{
				workflow.LabelImplement: {{"number": 5}, {"number": 6}, {"number": 7}},
			})

			code, err := capped(workflow.Implementer, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(code).To(BeZero())

			enqueued := h.runner.find("enqueue")
			Expect(enqueued).To(HaveLen(2))
			Expect(joined(enqueued[0])).To(ContainSubstring("ISSUE=5"))
			Expect(joined(enqueued[1])).To(ContainSubstring("ISSUE=6"))
			Expect(h.labels(7)).To(ContainElement(workflow.LabelImplement), "claimed past the run cap")
		})

		It("claims nothing for an agent role whose runs fill its run cap", func() {
			h.queue(map[string][]map[string]any{
				workflow.LabelImplement: {{"number": 5}},
			})
			busyDagu(`[{"name":"factory-implementer","dagRunId":"r1","status":"running"}]`)

			expectNothingReady()
			Expect(h.runner.find("enqueue")).To(BeEmpty(), "claimed past the run cap")
			Expect(h.logs.String()).To(ContainSubstring("runs fill its capacity"))
		})

		It("still claims another agent role whose DAG is idle", func() {
			h.queue(map[string][]map[string]any{
				workflow.LabelReview:    {{"number": 12}},
				workflow.LabelImplement: {{"number": 5}},
			})
			busyDagu(`[{"name":"factory-implementer","dagRunId":"r1","status":"queued"}]`)

			Expect(poll()).Error().NotTo(HaveOccurred())
			Expect(h.runner.find("enqueue")).To(HaveLen(1))
			Expect(joined(h.runner.find("enqueue")[0])).To(ContainSubstring("factory-reviewer"))
			Expect(h.labels(5)).To(ContainElement(workflow.LabelImplement))
		})

		It("claims the oldest issue across repositories, not the first repository", func() {
			h.installed = map[workflow.Role][]string{workflow.Implementer: {"o/oldest-a", "o/oldest-b"}}
			h.queueRepo("o/oldest-b", map[string][]map[string]any{
				workflow.LabelImplement: {{"number": 5}},
			})
			h.queueRepo("o/oldest-a", map[string][]map[string]any{
				workflow.LabelImplement: {{"number": 1}},
			})

			// Pin each repository's creation time on the claim's list read. The emulator dates an
			// issue with the wall clock, which must never decide the order.
			created := map[string]string{
				"o/oldest-a": "2026-01-02T00:00:00Z",
				"o/oldest-b": "2026-01-01T00:00:00Z",
			}

			for repo, at := range created {
				h.gh.Handle("GET /repos/"+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("labels") == "" {
						h.gh.Forward(w, r)

						return
					}

					var issues []map[string]any
					Expect(emulator.Do(context.Background(), http.MethodGet, r.URL.RequestURI(),
						ghtest.AdminToken, nil, &issues)).To(Succeed())

					for _, issue := range issues {
						issue["created_at"] = at
					}

					ghtest.WriteJSON(w, issues)
				})
			}

			Expect(poll()).Error().NotTo(HaveOccurred())

			enqueued := h.runner.find("enqueue")
			Expect(enqueued).To(HaveLen(1))
			Expect(joined(enqueued[0])).To(ContainSubstring("REPO=o/oldest-b ISSUE=5"))

			older, err := emulator.Issue(context.Background(), "o/oldest-b", 5)
			Expect(err).NotTo(HaveOccurred())
			Expect(older.Labels).To(HaveLen(1))
			Expect(older.Labels[0].Name).To(Equal(workflow.LabelImplementing))

			newer, err := emulator.Issue(context.Background(), "o/oldest-a", 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(newer.Labels).To(HaveLen(1))
			Expect(newer.Labels[0].Name).To(Equal(workflow.LabelImplement))
		})

		It("warns once per poll for an agent role no worker host takes, and fails nothing", func() {
			cfg := pollConfig(workflow.Implementer, workflow.Reviewer)
			cfg.UnhostedRoles = []workflow.Role{workflow.Implementer}

			h.queue(map[string][]map[string]any{
				workflow.LabelImplement: {{"number": 5}},
			})

			code, err := Poll(context.Background(), h.deps, cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(code).To(BeZero(), "a role with no host must not fail the poll")
			Expect(h.runner.find("enqueue")).To(BeEmpty())
			Expect(h.labels(5)).To(ContainElement(workflow.LabelImplement), "a queued issue lost its label")
			Expect(strings.Count(h.logs.String(), "no worker host takes this agent role")).To(Equal(1))

			h.out.Reset()
			Expect(Poll(context.Background(), h.deps, cfg)).Error().NotTo(HaveOccurred())
			Expect(strings.Count(h.logs.String(), "no worker host takes this agent role")).To(Equal(2))
		})

		DescribeTable("orders issues by creation and then repository and number",
			func(a, b queuedIssue, want bool) {
				Expect(firstIssue(a, b)).To(Equal(want))
			},
			Entry("an older issue first",
				queuedIssue{repo: testRepo("o/a"), issue: workflow.Issue{Number: 1, CreatedAt: time.Unix(1, 0)}},
				queuedIssue{repo: testRepo("o/a"), issue: workflow.Issue{Number: 2, CreatedAt: time.Unix(2, 0)}},
				true),
			Entry("the repository name breaks a tie",
				queuedIssue{repo: testRepo("o/a"), issue: workflow.Issue{Number: 9}},
				queuedIssue{repo: testRepo("o/b"), issue: workflow.Issue{Number: 1}},
				true),
			Entry("the issue number breaks a tie in one repository",
				queuedIssue{repo: testRepo("o/a"), issue: workflow.Issue{Number: 1}},
				queuedIssue{repo: testRepo("o/a"), issue: workflow.Issue{Number: 2}},
				true),
			Entry("an undated issue goes last",
				queuedIssue{repo: testRepo("o/a"), issue: workflow.Issue{Number: 1, CreatedAt: time.Unix(2, 0)}},
				queuedIssue{repo: testRepo("o/a"), issue: workflow.Issue{Number: 2}},
				true),
		)
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

		It("fails loudly when the assigned repositories exceed the cap", func() {
			crowded := make([]string, 0, MaxAssignedRepos+1)
			for i := range MaxAssignedRepos + 1 {
				crowded = append(crowded, fmt.Sprintf("o/assigned-%d", i))
			}

			h.installed = map[workflow.Role][]string{workflow.Implementer: crowded}

			code, err := poll()
			Expect(err).To(MatchError(ContainSubstring("over the 200 a poll looks at")))
			Expect(code).NotTo(BeZero())
			Expect(h.gh.Calls()).To(BeEmpty(), "a poll over its cap must not act on a truncated view")
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
