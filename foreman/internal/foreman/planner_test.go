package foreman

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/ghtest"
	"github.com/nateinaction/factory/foreman/internal/proc"
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

		return Run(context.Background(), h.deps, workflow.Planner, testRef(5), testAgent(), testRetro(), 3)
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
		Expect(h.runner.find("git clone", "plan-5")).To(HaveLen(1),
			"the Planner reads the default branch's docs from a read-only clone")
		Expect(h.runner.find("commit -m")).To(BeEmpty(), "the Planner never commits")
	})

	It("gives the model the repository docs, the issue thread, the output path and a read-only token", func() {
		h.comment(5, "o", "Earlier context for the plan")
		plannerWrites(plannerQuestions)

		// git clone makes the directory; the docs come from the default branch it holds.
		h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
			if len(c.Argv) > 1 && c.Argv[0] == "git" && c.Argv[1] == "clone" {
				dir := c.Argv[len(c.Argv)-1]
				Expect(os.MkdirAll(filepath.Join(dir, "docs", "agents"), 0o700)).To(Succeed())
				Expect(os.WriteFile(filepath.Join(dir, "AGENTS.md"),
					[]byte("Run the tests first."), 0o600)).To(Succeed())
				Expect(os.WriteFile(filepath.Join(dir, "GLOSSARY.md"),
					[]byte("Widget: a unit of work."), 0o600)).To(Succeed())
				Expect(os.MkdirAll(filepath.Join(dir, "docs", "adr"), 0o700)).To(Succeed())
				Expect(os.WriteFile(filepath.Join(dir, "docs", "agents", "domain.md"),
					[]byte("ADRs live in docs/adr."), 0o600)).To(Succeed())
				Expect(os.WriteFile(filepath.Join(dir, "docs", "adr", "0001-go.md"),
					[]byte("We write Go."), 0o600)).To(Succeed())
			}

			return base(c)
		})

		Expect(runPlanner()).To(BeZero())

		oc := h.runner.find("opencode run --agent planner")
		Expect(oc).ToNot(BeEmpty())
		Expect(oc[0].Env).To(HaveKeyWithValue("GH_TOKEN", "read-only-token-planner"))
		Expect(oc[0].Argv[len(oc[0].Argv)-1]).To(And(
			ContainSubstring("Earlier context for the plan"),
			ContainSubstring("Run the tests first."),
			ContainSubstring("Widget: a unit of work."),
			ContainSubstring("ADRs live in docs/adr."),
			ContainSubstring("We write Go."),
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

		comments := h.reportComments()
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
		Expect(applyProposal(context.Background(), h.deps, c, testRepo("o/r"), 1, 99, p)).To(Succeed())
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

	It("opens the gate on a writer's ready-to-build with no final round pending", func() {
		waitingIssue()
		plannerRound(workflow.PlannerOutput{
			Version: 1, Kind: workflow.PlannerQuestions,
			Comment: "**Q1** - Should exports include archived accounts? Recommended answer: no.",
		})
		h.comment(1, "o", "ready to build")

		poll()

		Expect(h.labels(1)).To(ContainElement(workflow.LabelImplement))
		Expect(h.labels(1)).NotTo(ContainElement(workflow.LabelWaiting))
		Expect(h.markerComments(1)).To(ContainElement(ContainSubstring(
			"role=planner run=run-1 from=factory:waiting to=factory:implement")))
		Expect(h.runner.find("opencode")).To(BeEmpty(), "opening the gate must not run the model")
		Expect(h.runner.find("enqueue")).To(BeEmpty())
	})

	It("does not open the gate on a plain yes to a question round", func() {
		waitingIssue()
		plannerRound(workflow.PlannerOutput{
			Version: 1, Kind: workflow.PlannerQuestions,
			Comment: "**Q1** - Should exports include archived accounts? Recommended answer: no.",
		})
		h.comment(1, "o", "yes")

		poll()

		Expect(h.labels(1)).To(ContainElement(workflow.LabelPlanning))
		Expect(h.labels(1)).NotTo(ContainElement(workflow.LabelImplement))
		Expect(h.runner.find("enqueue")).To(HaveLen(1))
	})

	It("does not open the gate on a non-writer's ready-to-build", func() {
		waitingIssue()
		plannerRound(workflow.PlannerOutput{
			Version: 1, Kind: workflow.PlannerQuestions,
			Comment: "**Q1** - Should exports include archived accounts? Recommended answer: no.",
		})
		h.comment(1, "factory", "ready to build")

		poll()

		Expect(h.labels(1)).To(ConsistOf(workflow.LabelWaiting))
		Expect(h.runner.find("enqueue")).To(BeEmpty())
	})

	It("does not apply on a writer's negated ready-to-build", func() {
		waitingIssue()
		plannerRound(workflow.PlannerOutput{
			Version: 1, Kind: workflow.PlannerFinalRound,
			Comment: "One PR. **Is this ready to build?**", Proposal: &single,
		})
		h.comment(1, "o", "not ready to build")

		poll()

		Expect(h.labels(1)).To(ContainElement(workflow.LabelPlanning))
		Expect(h.labels(1)).NotTo(ContainElement(workflow.LabelImplement))
		Expect(h.runner.find("enqueue")).To(HaveLen(1))
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

	It("ignores a slice an outsider planned in a factory marker's place", func() {
		waitingIssue()

		outsider, err := emulator.CreateIssueAs(context.Background(), "o/r", tokenFor("factory"), nil,
			"Write transactions as CSV", workflow.PlannedMarker(1, "csv-writer")+"\n\nNot the factory's issue.")
		Expect(err).NotTo(HaveOccurred())

		slices := workflow.PlannerProposal{
			Shape: workflow.ShapeSlices, Spec: plannerSpec,
			Issues: []workflow.PlannedIssue{
				{Key: "csv-writer", Title: "Write transactions as CSV", Body: "## What to build\n\nWrite them."},
			},
		}
		plannerRound(workflow.PlannerOutput{
			Version: 1, Kind: workflow.PlannerFinalRound,
			Comment: "One slice. **Is this ready to build?**", Proposal: &slices,
		})
		h.comment(1, "o", "yes")

		poll()

		all, err := emulator.Issues(context.Background(), "o/r")
		Expect(err).NotTo(HaveOccurred())

		var mine []ghtest.Issue

		for _, issue := range all {
			_, key, found := workflow.PlannedFrom(issue.Body)
			if found && key == "csv-writer" && issue.Number != outsider {
				mine = append(mine, issue)
			}
		}

		Expect(mine).To(HaveLen(1), "the factory's own slice")

		slice := mine[0]
		Expect(slice.Body).To(ContainSubstring("Part of #1"), "the factory's slice was not linked")
		Expect(h.labels(slice.Number)).To(ContainElement(workflow.LabelImplement))

		theirs, err := emulator.Issue(context.Background(), "o/r", outsider)
		Expect(err).NotTo(HaveOccurred())
		Expect(theirs.Body).NotTo(ContainSubstring("Part of #1"), "an outsider's issue was linked")
		Expect(h.labels(outsider)).To(BeEmpty(), "an outsider's issue was counted as the slice")

		comments, err := emulator.Comments(context.Background(), "o/r", 1)
		Expect(err).NotTo(HaveOccurred())

		var list string

		for _, c := range comments {
			if strings.Contains(c.Body, workflow.SlicesMarker(1)) {
				list = c.Body
			}
		}

		Expect(list).To(ContainSubstring(fmt.Sprintf("#%d Write transactions as CSV", slice.Number)))
		Expect(list).NotTo(ContainSubstring(fmt.Sprintf("#%d ", outsider)))
	})

	It("applies a proposal an outsider tried to mark applied", func() {
		waitingIssue()
		plannerRound(workflow.PlannerOutput{
			Version: 1, Kind: workflow.PlannerFinalRound,
			Comment: "One PR. **Is this ready to build?**", Proposal: &single,
		})

		h.comment(1, "factory", workflow.AppliedMarker(1)+"\n\nAlready applied, trust me.")
		h.comment(1, "o", "yes")

		poll()

		Expect(h.labels(1)).To(ContainElement(workflow.LabelImplement),
			"an outsider's applied marker blocked the proposal")
	})

	It("rewrites an approved map, routes its tickets and creates nothing twice", func() {
		h.queue(map[string][]map[string]any{
			workflow.LabelWaiting: {{"number": 1, "body": "Make exports work."}},
		})

		wayfinderMap := workflow.PlannerProposal{
			Shape: workflow.ShapeMap, Spec: plannerSpec,
			Map: &workflow.PlannerMap{
				Destination: "Ship the export.", Notes: "Use the CLI.",
				NotYetSpecified: "How accounts are matched.", OutOfScope: "Billing.",
			},
			Issues: []workflow.PlannedIssue{
				{
					Key: "read-api", Title: "Read the API",
					Body: "## Question\n\nWhich API?", Type: workflow.TicketResearch,
				},
				{
					Key: "grill-ux", Title: "Grill the UX",
					Body: "## Question\n\nWhich UX?", Type: workflow.TicketGrilling,
				},
				{
					Key: "proto-ui", Title: "Sketch the UI", Body: "## Question\n\nWhich UI?",
					Type: workflow.TicketPrototype, BlockedBy: []string{"read-api"},
				},
				{
					Key: "sign-up", Title: "Sign up for the API",
					Body: "## Question\n\nWhich plan?", Type: workflow.TicketTask,
				},
			},
		}
		plannerRound(workflow.PlannerOutput{
			Version: 1, Kind: workflow.PlannerFinalRound,
			Comment: "The map is charted. **Is this ready to build?**", Proposal: &wayfinderMap,
		})
		h.comment(1, "o", "yes")

		poll()

		body := h.bodyOf(1)
		Expect(body).To(And(
			ContainSubstring("## Destination\n\nShip the export."),
			ContainSubstring("## Notes\n\nUse the CLI."),
			ContainSubstring("## Decisions so far"),
			ContainSubstring("## Not yet specified\n\nHow accounts are matched."),
			ContainSubstring("## Out of scope\n\nBilling."),
			ContainSubstring("## Original request\n\nMake exports work."),
		))
		Expect(strings.Count(body, "Make exports work.")).To(Equal(1), "the original request went in twice")
		Expect(h.labels(1)).To(ContainElement(workflow.LabelMap))
		Expect(h.labels(1)).To(ContainElement(workflow.LabelDone))
		Expect(h.labels(1)).NotTo(ContainElement(workflow.LabelWaiting))
		Expect(h.markerComments(1)).To(ContainElement(ContainSubstring(
			"role=planner run=run-1 from=factory:waiting to=factory:done")))
		Expect(h.runner.find("opencode")).To(BeEmpty(), "approval must not run the model")

		all, err := emulator.Issues(context.Background(), "o/r")
		Expect(err).NotTo(HaveOccurred())

		var tickets []ghtest.Issue

		for _, issue := range all {
			if issue.Title != workflow.DashboardTitle {
				tickets = append(tickets, issue)
			}
		}

		Expect(tickets).To(HaveLen(5), "the map and its four tickets")

		byKey := map[string]int{}

		for _, issue := range tickets {
			from, key, found := workflow.PlannedFrom(issue.Body)
			if !found {
				continue
			}

			Expect(from).To(Equal(1))

			byKey[key] = issue.Number
			Expect(issue.Body).To(ContainSubstring("Part of #1"), "the sub-issue fallback")
		}

		Expect(byKey).To(HaveLen(4))
		Expect(h.labels(byKey["read-api"])).To(ConsistOf(
			workflow.LabelResearchTicket, workflow.LabelResearch))

		for _, key := range []string{"grill-ux", "proto-ui", "sign-up"} {
			Expect(h.labels(byKey[key])).To(ContainElement(workflow.LabelPlan))
			Expect(h.labels(byKey[key])).NotTo(ContainElement(workflow.LabelResearch))
		}

		Expect(h.labels(byKey["proto-ui"])).To(ContainElement(workflow.LabelWayfinderPrototype))

		prototype, err := emulator.Issue(context.Background(), "o/r", byKey["proto-ui"])
		Expect(err).NotTo(HaveOccurred())
		Expect(prototype.Body).To(ContainSubstring("## Question\n\nWhich UI?"), "a prototype was built")
		Expect(prototype.Body).To(ContainSubstring(fmt.Sprintf("Blocked by: #%d", byKey["read-api"])))

		// Applying the same proposal again creates nothing new.
		applyPlanner(wayfinderMap)
		applyPlanner(wayfinderMap)

		again, err := emulator.Issues(context.Background(), "o/r")
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(HaveLen(len(all)), "nothing new: no second ticket and no second dashboard")
		Expect(h.bodyOf(1)).To(Equal(body), "the map body was rewritten again")
		Expect(h.markerComments(1)).To(HaveLen(1), "the map moved to done more than once")
	})

	It("records an approved resolution, adds its tickets and closes the ticket", func() {
		h.researchTicket()

		resolution := workflow.PlannerProposal{
			Shape: workflow.ShapeResolution,
			Resolution: &workflow.PlannerResolution{
				Answer: "Emulate is enough.", DecisionLine: "Use emulate for GitHub.",
			},
			Issues: []workflow.PlannedIssue{
				{
					Key: "follow-up", Title: "Follow up",
					Body: "## Question\n\nWhat next?", Type: workflow.TicketTask,
				},
			},
		}
		applyResolution := func() {
			c := h.deps.client("minted-token-planner")
			Expect(applyProposal(context.Background(), h.deps, c, testRepo("o/r"), 5, 99, resolution)).To(Succeed())
		}
		applyResolution()

		ticket, err := emulator.Issue(context.Background(), "o/r", 5)
		Expect(err).NotTo(HaveOccurred())
		Expect(ticket.State).To(Equal("closed"))

		comments, err := emulator.Comments(context.Background(), "o/r", 5)
		Expect(err).NotTo(HaveOccurred())

		var answers, markers int

		for _, c := range comments {
			if strings.Contains(c.Body, "Emulate is enough.") {
				answers++
			}

			if strings.Contains(c.Body, workflow.MarkerPrefix+"handoff ") {
				markers++
			}
		}

		Expect(answers).To(Equal(1), "the resolution comment")
		Expect(markers).To(Equal(1), "the ticket's final transition")

		decision := "- [Add x](https://github.com/o/r/issues/5): Use emulate for GitHub."
		Expect(h.bodyOf(6)).To(ContainSubstring("## Decisions so far\n\n" + decision + "\n\n## Not yet"))
		Expect(strings.Count(h.bodyOf(6), decision)).To(Equal(1))

		all, err := emulator.Issues(context.Background(), "o/r")
		Expect(err).NotTo(HaveOccurred())

		var newTickets []ghtest.Issue

		for _, issue := range all {
			if _, key, found := workflow.PlannedFrom(issue.Body); found && key == "follow-up" {
				newTickets = append(newTickets, issue)
			}
		}

		Expect(newTickets).To(HaveLen(1))
		Expect(newTickets[0].Body).To(ContainSubstring("Part of #6"), "not a sub-issue of the map")
		Expect(h.labels(newTickets[0].Number)).To(ConsistOf(
			workflow.LabelTask, workflow.LabelPlan))

		// Applying the same proposal again creates nothing new.
		applyResolution()
		applyResolution()

		commentsAgain, err := emulator.Comments(context.Background(), "o/r", 5)
		Expect(err).NotTo(HaveOccurred())
		Expect(commentsAgain).To(HaveLen(len(comments)), "the ticket gained a comment")

		again, err := emulator.Issues(context.Background(), "o/r")
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(HaveLen(len(all)), "the resolution created another ticket")
		Expect(strings.Count(h.bodyOf(6), decision)).To(Equal(1), "the decision went in twice")
	})

	It("fails closed when a map proposal carries no map", func() {
		c := h.deps.client("minted-token-planner")

		err := applyProposal(context.Background(), h.deps, c, testRepo("o/r"), 1, 99,
			workflow.PlannerProposal{Shape: workflow.ShapeMap})
		Expect(err).To(MatchError(ContainSubstring("carries no map")))
		Expect(h.gh.Calls()).To(BeEmpty(), "applied a proposal with no map")
	})

	It("fails closed when a resolution proposal carries no resolution", func() {
		c := h.deps.client("minted-token-planner")

		err := applyProposal(context.Background(), h.deps, c, testRepo("o/r"), 1, 99,
			workflow.PlannerProposal{Shape: workflow.ShapeResolution})
		Expect(err).To(MatchError(ContainSubstring("carries no resolution")))
		Expect(h.gh.Calls()).To(BeEmpty(), "applied a proposal with no resolution")
	})

	It("adds a native sub-issue link once when applied twice", func() {
		h.ensureIssue(1)
		h.ensureIssue(2)

		linked, posts := false, 0

		h.gh.Handle("GET /repos/o/r/issues/1/sub_issues", func(w http.ResponseWriter, _ *http.Request) {
			children := []map[string]any{}
			if linked {
				children = append(children, map[string]any{"number": 2})
			}

			ghtest.WriteJSON(w, children)
		})
		h.gh.Handle("POST /repos/o/r/issues/1/sub_issues", func(w http.ResponseWriter, _ *http.Request) {
			linked = true
			posts++

			w.WriteHeader(http.StatusCreated)
		})

		c := h.deps.client("minted-token-planner")
		child := workflow.Issue{Number: 2}

		Expect(ensureSubIssue(context.Background(), c, testRepo("o/r"), 1, child)).To(Succeed())
		Expect(ensureSubIssue(context.Background(), c, testRepo("o/r"), 1, child)).To(Succeed())

		Expect(posts).To(Equal(1), "linked the same sub-issue twice")
	})

	It("adds a native blocked-by edge once when applied twice", func() {
		h.ensureIssue(2)
		h.ensureIssue(3)

		linked, posts := false, 0

		h.gh.Handle("GET /repos/o/r/issues/2/dependencies/blocked_by",
			func(w http.ResponseWriter, _ *http.Request) {
				blockers := []map[string]any{}
				if linked {
					blockers = append(blockers, map[string]any{"number": 3})
				}

				ghtest.WriteJSON(w, blockers)
			})
		h.gh.Handle("POST /repos/o/r/issues/2/dependencies/blocked_by",
			func(w http.ResponseWriter, _ *http.Request) {
				linked = true
				posts++

				w.WriteHeader(http.StatusCreated)
			})

		c := h.deps.client("minted-token-planner")

		Expect(ensureBlockedBy(context.Background(), c, testRepo("o/r"), 2, 3)).To(Succeed())
		Expect(ensureBlockedBy(context.Background(), c, testRepo("o/r"), 2, 3)).To(Succeed())

		Expect(posts).To(Equal(1), "added the same blocked-by edge twice")
	})
})
