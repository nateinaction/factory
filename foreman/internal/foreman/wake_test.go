package foreman

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/ghtest"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("waking on a comment", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
	})

	// transition posts a handoff marker on issue 1 as the agent role's App.
	transition := func(t workflow.Transition) {
		GinkgoHelper()

		body, err := workflow.RenderTransition(t)
		Expect(err).NotTo(HaveOccurred())
		h.comment(1, appLogin(t.Role), body)
	}

	// forge posts a handoff marker on issue 1 as the repository's writer, who cannot hand off.
	forge := func(t workflow.Transition) {
		GinkgoHelper()

		body, err := workflow.RenderTransition(t)
		Expect(err).NotTo(HaveOccurred())
		Expect(emulator.CommentAs(context.Background(), "o/r", 1, ghtest.AdminToken, body)).To(Succeed())
	}

	It("does not wake on a marker only the writer forged", func() {
		h.queue(map[string][]map[string]any{workflow.LabelWaiting: {{"number": 1}}})
		forge(workflow.Transition{
			Role: workflow.Planner, RunID: "forged", From: workflow.Planning, To: workflow.Waiting,
		})
		forge(workflow.Transition{
			Role: workflow.Planner, RunID: "forged", From: workflow.Waiting, To: workflow.Plan,
		})
		h.comment(1, "o", "send it back to planning")

		h.installed = map[workflow.Role][]string{
			workflow.Planner: {"o/r"}, workflow.Implementer: {"o/r"}, workflow.Reviewer: {"o/r"},
		}

		h.out.Reset()
		code, pollErr := Poll(context.Background(), h.deps,
			pollConfig(workflow.Planner, workflow.Implementer, workflow.Reviewer))
		Expect(pollErr).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		Expect(h.labels(1)).To(ConsistOf(workflow.LabelWaiting))
		Expect(h.runner.find("enqueue")).To(BeEmpty())
	})

	It("does not block on a malformed marker the writer posted", func() {
		h.queue(map[string][]map[string]any{workflow.LabelImplement: {{"number": 1}}})
		h.comment(1, "o", "<!-- factory:handoff not a marker -->")

		h.installed = map[workflow.Role][]string{workflow.Implementer: {"o/r"}}

		h.out.Reset()
		code, pollErr := Poll(context.Background(), h.deps, pollConfig(workflow.Implementer))
		Expect(pollErr).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		Expect(h.labels(1)).To(ContainElement(workflow.LabelImplementing))
		Expect(h.runner.find("enqueue")).To(HaveLen(1))
	})

	It("sends a waiting issue back to the Planner on a writer's comment", func() {
		h.queue(map[string][]map[string]any{workflow.LabelWaiting: {{"number": 1}}})
		transition(workflow.Transition{
			Role: workflow.Planner, RunID: "old", From: workflow.Planning, To: workflow.Waiting,
		})
		h.comment(1, "o", "yes, that plan works")

		h.installed = map[workflow.Role][]string{
			workflow.Planner: {"o/r"}, workflow.Implementer: {"o/r"}, workflow.Reviewer: {"o/r"},
		}

		h.out.Reset()
		code, pollErr := Poll(context.Background(), h.deps,
			pollConfig(workflow.Planner, workflow.Implementer, workflow.Reviewer))
		Expect(pollErr).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		Expect(h.labels(1)).To(ContainElement(workflow.LabelPlanning))
		markers := h.markerComments(1)
		Expect(markers[0]).To(ContainSubstring(
			"role=planner run=run-1 from=factory:waiting to=factory:plan sha= -->"))
		Expect(h.runner.find("enqueue")).To(HaveLen(1))
		Expect(joined(h.runner.find("enqueue")[0])).To(ContainSubstring("factory-planner"))
	})

	It("sends a done issue back to the Implementer on a comment on its pull request", func() {
		h.queue(map[string][]map[string]any{workflow.LabelDone: {{"number": 1}}})
		transition(workflow.Transition{
			Role: workflow.Reviewer, RunID: "old", From: workflow.Reviewing, To: workflow.Done,
		})

		pr, err := emulator.CreatePR(context.Background(), "o/r", workflow.BranchName(1), "Add x", "Do x")
		Expect(err).NotTo(HaveOccurred())
		h.comment(pr.Number, "o", "please also handle the empty case")

		h.out.Reset()
		code, pollErr := Poll(context.Background(), h.deps,
			pollConfig(workflow.Implementer, workflow.Reviewer))
		Expect(pollErr).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		Expect(h.labels(1)).To(ContainElement(workflow.LabelImplementing))
		markers := h.markerComments(1)
		Expect(markers[0]).To(ContainSubstring(
			"role=implementer run=run-1 from=factory:done to=factory:implement sha="))

		// The Implementer's run reads the comment the writer left on the pull request.
		h.writeToken(workflow.Implementer, "repo-token\n")
		h.roleWrites(workflow.Implementer, 1, reworkOutput)
		Expect(Run(context.Background(), h.deps, workflow.Implementer, testRef(1), testAgent(), testRetro(), 3)).
			Error().NotTo(HaveOccurred())

		oc := h.runner.find("opencode run --agent implementer --format json Use the")
		Expect(oc).NotTo(BeEmpty())
		Expect(oc[0].Argv[len(oc[0].Argv)-1]).To(ContainSubstring("**o:** please also handle the empty case"))
	})

	It("does not wake on a comment posted before the latest marker", func() {
		h.queue(map[string][]map[string]any{workflow.LabelWaiting: {{"number": 1}}})
		h.comment(1, "o", "that was before the question")
		transition(workflow.Transition{
			Role: workflow.Planner, RunID: "old", From: workflow.Planning, To: workflow.Waiting,
		})

		h.installed = map[workflow.Role][]string{
			workflow.Planner: {"o/r"}, workflow.Implementer: {"o/r"}, workflow.Reviewer: {"o/r"},
		}

		h.out.Reset()
		code, pollErr := Poll(context.Background(), h.deps,
			pollConfig(workflow.Planner, workflow.Implementer, workflow.Reviewer))
		Expect(pollErr).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		Expect(h.labels(1)).To(ConsistOf(workflow.LabelWaiting))
		Expect(h.markerComments(1)).To(BeEmpty())
	})

	It("wakes nothing on a non-writer's comment", func() {
		h.queue(map[string][]map[string]any{workflow.LabelWaiting: {{"number": 1}}})
		transition(workflow.Transition{
			Role: workflow.Planner, RunID: "old", From: workflow.Planning, To: workflow.Waiting,
		})
		h.comment(1, "factory", "please work on this")

		h.installed = map[workflow.Role][]string{
			workflow.Planner: {"o/r"}, workflow.Implementer: {"o/r"}, workflow.Reviewer: {"o/r"},
		}

		h.out.Reset()
		code, pollErr := Poll(context.Background(), h.deps,
			pollConfig(workflow.Planner, workflow.Implementer, workflow.Reviewer))
		Expect(pollErr).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		Expect(h.labels(1)).To(ConsistOf(workflow.LabelWaiting))
		Expect(h.markerComments(1)).To(BeEmpty())
		Expect(h.runner.find("enqueue")).To(BeEmpty())
	})

	It("does not wake a done issue from a comment on the issue itself", func() {
		h.queue(map[string][]map[string]any{workflow.LabelDone: {{"number": 1}}})
		transition(workflow.Transition{
			Role: workflow.Reviewer, RunID: "old", From: workflow.Reviewing, To: workflow.Done,
		})
		h.comment(1, "o", "looks good")

		h.out.Reset()
		code, pollErr := Poll(context.Background(), h.deps,
			pollConfig(workflow.Implementer, workflow.Reviewer))
		Expect(pollErr).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		Expect(h.labels(1)).To(ConsistOf(workflow.LabelDone))
		Expect(h.markerComments(1)).To(BeEmpty())
	})

	It("requeues a failed issue to the agent role that failed and reads every comment since the marker", func() {
		h.queue(map[string][]map[string]any{workflow.LabelFailed: {{"number": 1}}})
		transition(workflow.Transition{
			Role: workflow.Implementer, RunID: "old", From: workflow.Implementing, To: workflow.Failed,
		})
		h.comment(1, "o", "first bit of guidance")
		h.comment(1, "o", "second bit of guidance")

		code, pollErr := Poll(context.Background(), h.deps,
			pollConfig(workflow.Implementer, workflow.Reviewer))
		Expect(pollErr).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		Expect(h.labels(1)).To(ContainElement(workflow.LabelImplementing))
		markers := h.markerComments(1)
		Expect(markers[0]).To(ContainSubstring(
			"role=implementer run=run-1 from=factory:failed to=factory:implement sha="))

		// The Implementer's run reads the guidance the writer left after the failure.
		h.writeToken(workflow.Implementer, "repo-token\n")
		h.roleWrites(workflow.Implementer, 1, firstRunOutput)
		Expect(Run(context.Background(), h.deps, workflow.Implementer, testRef(1), testAgent(), testRetro(), 3)).
			Error().NotTo(HaveOccurred())

		oc := h.runner.find("opencode run --agent implementer --format json Use the")
		Expect(oc).NotTo(BeEmpty())

		prompt := oc[0].Argv[len(oc[0].Argv)-1]
		Expect(prompt).To(ContainSubstring("**o:** first bit of guidance"))
		Expect(prompt).To(ContainSubstring("**o:** second bit of guidance"))
		Expect(strings.Index(prompt, "first bit of guidance")).
			To(BeNumerically("<", strings.Index(prompt, "second bit of guidance")))
	})
})

var _ = Describe("dead runs", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
	})

	// addLabel adds a label through the API, so the emulator records the timeline event.
	addLabel := func(number int) {
		GinkgoHelper()
		Expect(emulator.Do(context.Background(), http.MethodPost,
			"/repos/o/r/issues/"+strconv.Itoa(number)+"/labels", ghtest.AdminToken,
			map[string]any{"labels": []string{workflow.LabelImplementing}}, nil)).To(Succeed())
	}

	It("moves a running label older than the deadline plus margin to failed, once", func() {
		h.ensureIssue(1)
		addLabel(1)

		old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		h.gh.JSON("GET /repos/o/r/issues/1/timeline", []map[string]any{{
			"event": "labeled", "label": map[string]string{"name": workflow.LabelImplementing},
			"created_at": old,
		}})

		code, pollErr := Poll(context.Background(), h.deps,
			pollConfig(workflow.Implementer, workflow.Reviewer))
		Expect(pollErr).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		Expect(h.labels(1)).To(ContainElement(workflow.LabelFailed))
		Expect(h.labels(1)).NotTo(ContainElement(workflow.LabelImplementing))
		Expect(h.logs.String()).To(ContainSubstring("failing a dead run"))

		markers := h.markerComments(1)
		Expect(markers).To(HaveLen(1))
		Expect(markers[0]).To(ContainSubstring(
			"role=implementer run=run-1 from=factory:implementing to=factory:failed sha= -->"))
		Expect(h.runner.find("enqueue")).To(BeEmpty(), "a dead run must not be retried")

		// The next poll leaves the failed issue alone: it is not retried automatically.
		h.out.Reset()
		code, pollErr = Poll(context.Background(), h.deps,
			pollConfig(workflow.Implementer, workflow.Reviewer))
		Expect(pollErr).NotTo(HaveOccurred())
		Expect(code).To(BeZero())
		Expect(h.markerComments(1)).To(HaveLen(1))
	})

	It("leaves a run younger than the deadline plus margin claimed", func() {
		h.ensureIssue(1)
		addLabel(1)

		h.out.Reset()
		code, pollErr := Poll(context.Background(), h.deps,
			pollConfig(workflow.Implementer, workflow.Reviewer))
		Expect(pollErr).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		Expect(h.labels(1)).To(ConsistOf(workflow.LabelImplementing))
		Expect(h.markerComments(1)).To(BeEmpty())
		Expect(h.runner.find("enqueue")).To(BeEmpty())
	})

	It("leaves a running label alone when its agent role has no deadline", func() {
		h.ensureIssue(1)
		addLabel(1)

		cfg := pollConfig(workflow.Implementer, workflow.Reviewer)
		agent := cfg.Agents[string(workflow.Implementer)]
		agent.DeadlineMinutes = 0
		cfg.Agents[string(workflow.Implementer)] = agent

		code, pollErr := Poll(context.Background(), h.deps, cfg)
		Expect(pollErr).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		Expect(h.labels(1)).To(ConsistOf(workflow.LabelImplementing))
		Expect(h.markerComments(1)).To(BeEmpty())
		Expect(h.logs.String()).To(ContainSubstring("without its agent role's deadline"))
	})

	It("leaves a running label alone when the timeline cannot be read", func() {
		h.ensureIssue(1)
		addLabel(1)
		h.gh.Status("GET /repos/o/r/issues/1/timeline", http.StatusInternalServerError)

		code, pollErr := Poll(context.Background(), h.deps,
			pollConfig(workflow.Implementer, workflow.Reviewer))
		Expect(pollErr).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		Expect(h.labels(1)).To(ConsistOf(workflow.LabelImplementing))
		Expect(h.markerComments(1)).To(BeEmpty())
		Expect(h.logs.String()).To(ContainSubstring("skipping an issue the poll cannot read"))
	})

	It("leaves a running label alone when the timeline has no labeled event", func() {
		h.ensureIssue(1)
		addLabel(1)
		h.gh.JSON("GET /repos/o/r/issues/1/timeline", []map[string]any{{
			"event": "commented", "created_at": time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		}})

		code, pollErr := Poll(context.Background(), h.deps,
			pollConfig(workflow.Implementer, workflow.Reviewer))
		Expect(pollErr).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		Expect(h.labels(1)).To(ConsistOf(workflow.LabelImplementing))
		Expect(h.markerComments(1)).To(BeEmpty())
		Expect(h.logs.String()).To(ContainSubstring("no labeled event"))
	})

	It("leaves a dead run claimed when its token cannot be minted", func() {
		h.ensureIssue(1)
		addLabel(1)
		h.failMint(workflow.Implementer, true)

		old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		h.gh.JSON("GET /repos/o/r/issues/1/timeline", []map[string]any{{
			"event": "labeled", "label": map[string]string{"name": workflow.LabelImplementing},
			"created_at": old,
		}})

		code, pollErr := Poll(context.Background(), h.deps,
			pollConfig(workflow.Implementer, workflow.Reviewer))
		Expect(pollErr).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		Expect(h.labels(1)).To(ConsistOf(workflow.LabelImplementing))
		Expect(h.markerComments(1)).To(BeEmpty())
		Expect(h.logs.String()).To(ContainSubstring("cannot mint the dead run's own token"))
	})

	It("does not fail a run whose slow retro outlasts the deadline plus margin", func() {
		h.ensureIssue(5)
		addLabel(5)
		h.implementerGH(nil, nil)
		h.writeToken(workflow.Implementer, "repo-token\n")
		h.roleWrites(workflow.Implementer, 5, firstRunOutput)
		h.roleWritesRetro(workflow.Implementer, validRetro)

		// The run was claimed long before the retro began, so a poll that still saw the running
		// label would call it dead while the retro is going.
		old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		h.gh.JSON("GET /repos/o/r/issues/5/timeline", []map[string]any{{
			"event": "labeled", "label": map[string]string{"name": workflow.LabelImplementing},
			"created_at": old,
		}})

		code, runErr := Run(context.Background(), h.deps, workflow.Implementer, testRef(5), testAgent(), testRetro(), 3)
		Expect(runErr).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		h.out.Reset()
		pollCode, pollErr := Poll(context.Background(), h.deps, pollConfig(workflow.Implementer))
		Expect(pollErr).NotTo(HaveOccurred())
		Expect(pollCode).To(BeZero())

		Expect(h.labels(5)).To(ConsistOf(workflow.LabelReview))
		Expect(h.markerComments(5)).NotTo(ContainElement(ContainSubstring("to=factory:failed")))
		Expect(h.logs.String()).NotTo(ContainSubstring("failing a dead run"))
	})
})
