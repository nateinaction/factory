package foreman

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/ghtest"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// plannerQuestions is a valid round of questions.
const plannerQuestions = `{"version":1,"kind":"questions","comment":` +
	`"**Q1** - Should exports include archived accounts? Recommended answer: no.\n` +
	`**Q2** - Is one command enough? Recommended answer: yes."}`

// plannerSpec is the spec a final round carries.
const plannerSpec = "## Spec\n\nExport transactions as CSV.\n\n## Domain changes\n\nNone."

// plannerSingle is a valid single-issue final round.
const plannerSingle = `{"version":1,"kind":"final_round","comment":` +
	`"Small enough for one pull request. **Is this ready to build?**","proposal":` +
	`{"shape":"single","spec":` + `"## Spec\n\nExport transactions as CSV.\n\n## Domain changes\n\nNone."` + `}}`

var _ = Describe("the Planner", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
		h.installed = map[workflow.Role][]string{workflow.Planner: {"o/r"}}
		h.ensureIssue(5)
	})

	// plannerWrites makes the Planner's model write text as its output file.
	plannerWrites := func(text string) {
		out := h.deps.Settings.WorkRoot + "/planner-5/" + workflow.PlannerOutputFile
		h.opencodeWrites("--agent planner ", out, text)
	}

	// runPlanner runs the Planner on issue 5.
	runPlanner := func() (int, error) {
		h.writeToken(workflow.Planner, "repo-token\n")

		return Run(context.Background(), h.deps, workflow.Planner, "o/r", testAgent(), testRetro(), 5)
	}

	It("posts a round of questions, moves the issue to waiting and runs no retro", func() {
		plannerWrites(plannerQuestions)

		Expect(runPlanner()).To(BeZero())

		comments := h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")
		Expect(comments).To(HaveLen(1), "one round comment")
		Expect(comments[0]).To(And(
			ContainSubstring("role=planner"),
			ContainSubstring("from=factory:planning to=factory:waiting"),
			ContainSubstring("Should exports include archived accounts?"),
			Not(ContainSubstring("factory:proposal")),
		))
		Expect(h.labels(5)).To(ContainElement(workflow.LabelWaiting))
		Expect(h.labels(5)).NotTo(ContainElement(workflow.LabelPlanning))

		Expect(h.opencodeCalls()).To(HaveLen(1), "the questions round has no retro")
		Expect(h.runner.find("--session")).To(BeEmpty())
		Expect(h.runner.find("git clone")).To(BeEmpty(), "the Planner never clones or commits")
		Expect(h.runner.find("commit -m")).To(BeEmpty())
	})

	It("gives the model the issue thread, the output path and a read-only token", func() {
		h.comment(5, "o", "Earlier context for the plan")
		plannerWrites(plannerQuestions)

		Expect(runPlanner()).To(BeZero())

		oc := h.runner.find("opencode run --agent planner")
		Expect(oc).ToNot(BeEmpty())
		Expect(oc[0].Env).To(HaveKeyWithValue("GH_TOKEN", "read-only-token-planner"))
		Expect(oc[0].Argv[len(oc[0].Argv)-1]).To(And(
			ContainSubstring("Earlier context for the plan"),
			ContainSubstring("Write the JSON to "+h.deps.Settings.WorkRoot+"/planner-5/"+workflow.PlannerOutputFile),
			ContainSubstring("Domain changes"),
		))
		Expect(joined(oc[0])).NotTo(ContainSubstring("or-key"))
		h.expectLeakFree("repo-token", "or-key")
	})

	It("stores a final round's proposal hidden in the comment and keys its retro by the comment id", func() {
		plannerWrites(plannerSingle)
		h.opencodeWrites("--session", h.retroOut(workflow.Planner), validRetro)

		Expect(runPlanner()).To(BeZero())

		comments := h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")
		Expect(comments).To(HaveLen(2), "the round and its retro")
		Expect(comments[0]).To(And(
			ContainSubstring("role=planner"),
			ContainSubstring("Is this ready to build?"),
			ContainSubstring("<!-- factory:proposal"),
			ContainSubstring("single"),
		))

		posted, err := emulator.Comments(context.Background(), "o/r", 5)
		Expect(err).NotTo(HaveOccurred())

		var id int64

		for _, c := range posted {
			if strings.Contains(c.Body, "factory:proposal") {
				id = c.ID
			}
		}

		Expect(id).NotTo(BeZero())

		retros := h.retroComments()
		Expect(retros).To(HaveLen(1), "the final-round run has its retro")
		Expect(retros[0]).To(HavePrefix(fmt.Sprintf(
			"<!-- factory:retro issue=5 role=planner key=%d -->", id)))
	})

	It("rejects an invalid output, applying nothing", func() {
		plannerWrites(`{"version":1,"kind":"questions","comment":"","labels":["factory:plan"]}`)

		code, err := runPlanner()
		Expect(code).To(Equal(workflow.ExitReported))
		Expect(err).To(HaveOccurred())

		comments := h.gh.Bodies("POST", "/repos/o/r/issues/5/comments")
		Expect(comments).To(HaveLen(1))
		Expect(comments[0]).To(ContainSubstring("output file was rejected"))
		Expect(h.labels(5)).NotTo(ContainElement(workflow.LabelWaiting))
	})
})

var _ = Describe("applying an approved proposal", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
		h.installed = map[workflow.Role][]string{workflow.Planner: {"o/r"}}
	})

	// plannerRound posts a Planner round on issue number as the Planner App.
	plannerRound := func(output workflow.PlannerOutput) {
		marker, err := workflow.RenderTransition(workflow.Transition{
			Role: workflow.Planner, RunID: "old", From: workflow.Planning, To: workflow.Waiting,
		})
		Expect(err).NotTo(HaveOccurred())

		body := marker + "\n\n" + output.Comment

		if output.Proposal != nil {
			block, err := workflow.ProposalBlock(*output.Proposal)
			Expect(err).NotTo(HaveOccurred())

			body += "\n\n" + block
		}

		Expect(emulator.CommentAs(context.Background(), "o/r", 1,
			"minted-token-planner", body)).To(Succeed())
	}

	// waitingIssue creates issue 1 in factory:waiting.
	waitingIssue := func() {
		h.queue(map[string][]map[string]any{workflow.LabelWaiting: {{"number": 1}}})
	}

	// poll runs a poll of the Planner's queue.
	poll := func() {
		h.out.Reset()
		Expect(Poll(context.Background(), h.deps, pollConfig(workflow.Planner))).To(BeZero())
	}

	// applyPlanner twice calls the Planner's apply directly, as a crashed poll would be retried.
	applyPlanner := func(p workflow.PlannerProposal) {
		c := h.deps.client("minted-token-planner")
		Expect(applyProposal(context.Background(), h.deps, c, "o/r", 1, 99, p)).To(Succeed())
	}

	single := workflow.PlannerProposal{Shape: workflow.ShapeSingle, Spec: plannerSpec}

	It("applies a single proposal on a writer's yes, with no model run", func() {
		waitingIssue()
		plannerRound(workflow.PlannerOutput{
			Version: 1, Kind: workflow.PlannerFinalRound,
			Comment: "One PR. **Is this ready to build?**", Proposal: &single,
		})
		h.comment(1, "o", "yes")

		poll()

		Expect(h.labels(1)).To(ContainElement(workflow.LabelImplement))
		Expect(h.labels(1)).NotTo(ContainElement(workflow.LabelWaiting))
		Expect(h.markerComments(1)).To(ContainElement(ContainSubstring(
			"role=planner run=run-1 from=factory:waiting to=factory:implement")))
		Expect(h.runner.find("opencode")).To(BeEmpty(), "approval must not run the model")
		Expect(h.runner.find("enqueue")).To(BeEmpty())
	})

	It("applies on a writer's ready-to-build", func() {
		waitingIssue()
		plannerRound(workflow.PlannerOutput{
			Version: 1, Kind: workflow.PlannerFinalRound,
			Comment: "One PR. **Is this ready to build?**", Proposal: &single,
		})
		h.comment(1, "o", "looks good, ready to build")

		poll()

		Expect(h.labels(1)).To(ContainElement(workflow.LabelImplement))
	})

	It("does not apply on a non-writer's yes", func() {
		waitingIssue()
		plannerRound(workflow.PlannerOutput{
			Version: 1, Kind: workflow.PlannerFinalRound,
			Comment: "One PR. **Is this ready to build?**", Proposal: &single,
		})
		h.comment(1, "factory", "yes")

		poll()

		Expect(h.labels(1)).To(ConsistOf(workflow.LabelWaiting))
		Expect(h.runner.find("enqueue")).To(BeEmpty())
	})

	It("wakes the Planner on a reply other than yes", func() {
		waitingIssue()
		plannerRound(workflow.PlannerOutput{
			Version: 1, Kind: workflow.PlannerFinalRound,
			Comment: "One PR. **Is this ready to build?**", Proposal: &single,
		})
		h.comment(1, "o", "not yet, split the storage change out")

		poll()

		Expect(h.labels(1)).To(ContainElement(workflow.LabelPlanning))
		Expect(h.runner.find("enqueue")).To(HaveLen(1))
		Expect(joined(h.runner.find("enqueue")[0])).To(ContainSubstring("factory-planner"))
	})

	It("creates the slices, links the spec, closes the original and creates nothing twice", func() {
		waitingIssue()

		slices := workflow.PlannerProposal{
			Shape: workflow.ShapeSlices, Spec: plannerSpec,
			Issues: []workflow.PlannedIssue{
				{Key: "csv-writer", Title: "Write transactions as CSV", Body: "## What to build\n\nWrite them."},
				{
					Key: "export-cli", Title: "Add an export command", Body: "## What to build\n\nAdd it.",
					BlockedBy: []string{"csv-writer"},
				},
			},
		}
		plannerRound(workflow.PlannerOutput{
			Version: 1, Kind: workflow.PlannerFinalRound,
			Comment: "Two slices. **Is this ready to build?**", Proposal: &slices,
		})
		h.comment(1, "o", "yes")

		poll()

		all, err := emulator.Issues(context.Background(), "o/r")
		Expect(err).NotTo(HaveOccurred())

		var issues []ghtest.Issue

		for _, issue := range all {
			if issue.Title != workflow.DashboardTitle {
				issues = append(issues, issue)
			}
		}

		Expect(issues).To(HaveLen(2), "two slices; the original is closed")

		slicesByKey := map[string]int{}

		for _, issue := range issues {
			from, key, found := workflow.PlannedFrom(issue.Body)
			Expect(found).To(BeTrue())
			Expect(from).To(Equal(1))

			slicesByKey[key] = issue.Number
			Expect(h.labels(issue.Number)).To(ContainElement(workflow.LabelImplement))
			Expect(issue.Body).To(ContainSubstring("Part of #1"), "the sub-issue fallback")
			Expect(issue.Body).To(ContainSubstring("/issues/1#issuecomment-"), "links the spec comment")
		}

		Expect(slicesByKey).To(HaveKey("csv-writer"))
		Expect(slicesByKey).To(HaveKey("export-cli"))

		cli, err := emulator.Issue(context.Background(), "o/r", slicesByKey["export-cli"])
		Expect(err).NotTo(HaveOccurred())
		Expect(cli.Body).To(ContainSubstring(fmt.Sprintf("Blocked by: #%d", slicesByKey["csv-writer"])))

		original, err := emulator.Issue(context.Background(), "o/r", 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(original.State).To(Equal("closed"))

		comments, err := emulator.Comments(context.Background(), "o/r", 1)
		Expect(err).NotTo(HaveOccurred())

		var list string

		for _, c := range comments {
			if strings.Contains(c.Body, workflow.SlicesMarker(1)) {
				list = c.Body
			}
		}

		Expect(list).To(ContainSubstring(fmt.Sprintf("#%d Write transactions as CSV", slicesByKey["csv-writer"])))
		Expect(list).To(ContainSubstring(fmt.Sprintf("#%d Add an export command", slicesByKey["export-cli"])))

		// Applying the same proposal again creates nothing new.
		applyPlanner(slices)
		applyPlanner(slices)

		again, err := emulator.Issues(context.Background(), "o/r")
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(HaveLen(len(all)), "nothing new: no second slice and no second dashboard")
	})
})
