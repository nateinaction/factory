package workflow_test

import (
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// plannerFinal builds a final-round output with the given proposal.
func plannerFinal(proposal workflow.PlannerProposal, comment string) workflow.PlannerOutput {
	return workflow.PlannerOutput{
		Version: workflow.OutputFileVersion, Kind: workflow.PlannerFinalRound,
		Comment: comment, Proposal: &proposal,
	}
}

// readyToBuild is a final round's comment that ends with the gate question.
const readyToBuild = "The spec and slices are below. **Is this ready to build?**"

var _ = Describe("planner-output.json", func() {
	validSpec := "## Spec\n\nDo the thing.\n\n## Domain changes\n\nNone."

	It("accepts a round of numbered questions", func() {
		out, err := workflow.ParsePlannerOutput(`{"version":1,"kind":"questions",` +
			`"comment":"**Q1** - Should exports include archived accounts? Recommended: no."}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Kind).To(Equal(workflow.PlannerQuestions))
		Expect(out.Proposal).To(BeNil())
	})

	It("accepts a single-issue final round", func() {
		out, err := workflow.ParsePlannerOutput(mustJSON(
			plannerFinal(workflow.PlannerProposal{Shape: workflow.ShapeSingle, Spec: validSpec}, readyToBuild)))
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Proposal.Shape).To(Equal(workflow.ShapeSingle))
		Expect(out.Proposal.Spec).To(ContainSubstring("Domain changes"))
	})

	It("accepts a slices final round and keeps the slugs", func() {
		out, err := workflow.ParsePlannerOutput(mustJSON(plannerFinal(workflow.PlannerProposal{
			Shape: workflow.ShapeSlices, Spec: validSpec,
			Issues: []workflow.PlannedIssue{
				{Key: "first", Title: "First", Body: "## What to build\n\nA"},
				{Key: "second", Title: "Second", Body: "## What to build\n\nB", BlockedBy: []string{"first", "#7"}},
			},
		}, readyToBuild)))
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Proposal.Issues).To(HaveLen(2))
		Expect(out.Proposal.Issues[1].BlockedBy).To(Equal([]string{"first", "#7"}))
	})

	It("accepts a map final round and keeps its tickets' types", func() {
		out, err := workflow.ParsePlannerOutput(mustJSON(plannerFinal(workflow.PlannerProposal{
			Shape: workflow.ShapeMap, Spec: validSpec,
			Map: &workflow.PlannerMap{Destination: "Ship it.", Notes: "None."},
			Issues: []workflow.PlannedIssue{
				{Key: "read", Title: "Read", Body: "## Question\n\nWhich?", Type: workflow.TicketResearch},
				{
					Key: "ask", Title: "Ask", Body: "## Question\n\nWhich?",
					Type: workflow.TicketGrilling, BlockedBy: []string{"read"},
				},
			},
		}, readyToBuild)))
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Proposal.Issues[1].Type).To(Equal(workflow.TicketGrilling))
	})

	It("accepts a resolution only with the resolved gate", func() {
		resolution := workflow.PlannerProposal{
			Shape: workflow.ShapeResolution,
			Resolution: &workflow.PlannerResolution{
				Answer: "Use emulate.", DecisionLine: "Use emulate for GitHub.",
			},
			Issues: []workflow.PlannedIssue{
				{Key: "follow-up", Title: "Follow up", Body: "## Question\n\nWhat?", Type: workflow.TicketTask},
			},
		}

		out, err := workflow.ParsePlannerOutput(mustJSON(
			plannerFinal(resolution, "We will use emulate. **Is this resolved?**")))
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Proposal.Shape).To(Equal(workflow.ShapeResolution))

		_, err = workflow.ParsePlannerOutput(mustJSON(plannerFinal(resolution, readyToBuild)))
		Expect(err).To(MatchError(ContainSubstring("Is this resolved?")))
	})

	DescribeTable("rejects an output that is not a valid round",
		func(mutate func(*workflow.PlannerOutput)) {
			out := plannerFinal(workflow.PlannerProposal{Shape: workflow.ShapeSingle, Spec: validSpec}, readyToBuild)
			mutate(&out)

			_, err := workflow.ParsePlannerOutput(mustJSON(out))
			Expect(err).To(HaveOccurred())
		},
		Entry("an unknown kind", func(o *workflow.PlannerOutput) { o.Kind = "chat" }),
		Entry("an empty comment", func(o *workflow.PlannerOutput) { o.Comment = " " }),
		Entry("a questions round with a proposal", func(o *workflow.PlannerOutput) {
			o.Kind = workflow.PlannerQuestions
		}),
		Entry("a final round without a proposal", func(o *workflow.PlannerOutput) { o.Proposal = nil }),
		Entry("a final round that does not end with the gate", func(o *workflow.PlannerOutput) {
			o.Comment = "Is this ready?"
		}),
		Entry("a spec without Domain changes", func(o *workflow.PlannerOutput) {
			o.Proposal.Spec = "## Spec\n\nDo it."
		}),
		Entry("a single proposal with slices", func(o *workflow.PlannerOutput) {
			o.Proposal.Issues = []workflow.PlannedIssue{{Key: "a", Title: "A", Body: "x"}}
		}),
		Entry("a slices proposal with no slices", func(o *workflow.PlannerOutput) {
			o.Proposal.Shape = workflow.ShapeSlices
		}),
		Entry("a repeated key", func(o *workflow.PlannerOutput) {
			o.Proposal.Issues = []workflow.PlannedIssue{
				{Key: "a", Title: "A", Body: "x"}, {Key: "a", Title: "B", Body: "y"},
			}
		}),
		Entry("a malformed key", func(o *workflow.PlannerOutput) {
			o.Proposal.Issues = []workflow.PlannedIssue{{Key: "Not A Slug", Title: "A", Body: "x"}}
		}),
		Entry("a dangling blocked_by", func(o *workflow.PlannerOutput) {
			o.Proposal.Issues = []workflow.PlannedIssue{{
				Key: "a", Title: "A", Body: "x", BlockedBy: []string{"missing"},
			}}
		}),
	)

	It("rejects an unknown field, including a label the model picked", func() {
		raw := `{"version":1,"kind":"final_round","comment":` + quote(readyToBuild) + `,"proposal":` +
			`{"shape":"slices","spec":` + quote(validSpec) + `,"issues":[` +
			`{"key":"a","title":"A","body":"x","labels":["factory:done"]}]}}`

		_, err := workflow.ParsePlannerOutput(raw)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("labels"))
	})

	It("rejects a file that repeats a key or carries trailing data", func() {
		out := mustJSON(plannerFinal(workflow.PlannerProposal{Shape: workflow.ShapeSingle, Spec: validSpec},
			readyToBuild))

		_, err := workflow.ParsePlannerOutput(strings.Replace(out, `"kind"`, `"kind":"questions","kind"`, 1))
		Expect(err).To(MatchError(ContainSubstring("repeats the key")))

		_, err = workflow.ParsePlannerOutput(out + ` {}`)
		Expect(err).To(MatchError(ContainSubstring("is not valid JSON")))
	})

	It("rejects a factory marker in the model's text", func() {
		out := plannerFinal(workflow.PlannerProposal{Shape: workflow.ShapeSingle, Spec: validSpec},
			readyToBuild+" <!-- factory:planned from=#1 key=a -->")

		_, err := workflow.ParsePlannerOutput(mustJSON(out))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("factory marker"))
	})
})

var _ = Describe("storing a final round's proposal", func() {
	proposal := workflow.PlannerProposal{
		Shape: workflow.ShapeSlices, Spec: "## Spec\n\nx\n\n## Domain changes\n\nNone.",
		Issues: []workflow.PlannedIssue{{Key: "first", Title: "First", Body: "A"}},
	}

	It("writes a hidden block that reads back whole", func() {
		block, err := workflow.ProposalBlock(proposal)
		Expect(err).NotTo(HaveOccurred())
		Expect(block).To(HavePrefix(workflow.MarkerPrefix + "proposal\n"))
		Expect(block).To(HaveSuffix("\n-->"))

		got, found, err := workflow.StoredProposal("The round.\n\n" + block)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(got).To(Equal(proposal))
	})

	It("finds no proposal in a question round", func() {
		_, found, err := workflow.StoredProposal("<!-- factory:handoff role=planner run=r " +
			"from=factory:planning to=factory:waiting sha= -->\n\n**Q1** - Yes?")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
	})

	It("fails closed on a malformed stored proposal", func() {
		_, found, err := workflow.StoredProposal(workflow.MarkerPrefix + "proposal\n{\"shape\":\"nope\"}\n-->")
		Expect(found).To(BeTrue())
		Expect(err).To(HaveOccurred())
	})

	DescribeTable("fails closed on a stored proposal it cannot read whole",
		func(text string) {
			_, found, err := workflow.StoredProposal(workflow.MarkerPrefix + "proposal\n" + text + "\n-->")
			Expect(found).To(BeTrue())
			Expect(err).To(HaveOccurred())
		},
		Entry("a repeated key", `{"shape":"single","spec":"## Spec\n\nx\n\n## Domain changes\n\nNone.",`+
			`"spec":"## Spec\n\nx\n\n## Domain changes\n\nNone."}`),
		Entry("an unknown field", `{"shape":"slices","labels":["x"]}`),
		Entry("trailing data", `{"shape":"slices"} {}`),
	)
})

var _ = Describe("the spec a final round stores", func() {
	// stored renders a factory comment carrying p's hidden proposal block.
	stored := func(p workflow.PlannerProposal) workflow.Item {
		GinkgoHelper()

		block, err := workflow.ProposalBlock(p)
		Expect(err).NotTo(HaveOccurred())

		return workflow.Item{ID: 1, Author: "o-factory-planner[bot]", Body: "The round.\n\n" + block}
	}

	single := workflow.PlannerProposal{
		Shape: workflow.ShapeSingle,
		Spec:  "## The spec\n\nx\n\n## Domain changes\n\nNone.",
	}

	It("returns a single proposal's spec", func() {
		spec, found, err := workflow.StoredSpec([]workflow.Item{stored(single)}, "o")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(spec).To(Equal(single.Spec))
	})

	It("keeps another shape's spec out of the final-round comment", func() {
		slices := workflow.PlannerProposal{
			Shape: workflow.ShapeSlices, Spec: single.Spec,
			Issues: []workflow.PlannedIssue{{Key: "first", Title: "First", Body: "A"}},
		}

		_, found, err := workflow.StoredSpec([]workflow.Item{stored(slices)}, "o")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
	})

	It("reads the latest stored proposal, not an earlier one", func() {
		const newerSpec = "## The newer spec\n\ny\n\n## Domain changes\n\nNone."

		older := stored(single)
		newer := stored(workflow.PlannerProposal{Shape: workflow.ShapeSingle, Spec: newerSpec})
		newer.ID = 2

		spec, found, err := workflow.StoredSpec([]workflow.Item{older, newer}, "o")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(spec).To(Equal(newerSpec))
	})

	It("ignores a proposal another author forged", func() {
		forged := stored(single)
		forged.Author = "o"

		_, found, err := workflow.StoredSpec([]workflow.Item{forged}, "o")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
	})
})

var _ = Describe("the human gate", func() {
	DescribeTable("accepts a writer's explicit yes to the gate",
		func(text string) { Expect(workflow.OpensGate(text)).To(BeTrue()) },
		Entry(nil, "yes"),
		Entry(nil, "Yes!"),
		Entry(nil, "  yes.  "),
		Entry(nil, "Yes, please."),
		Entry(nil, "lgtm"),
		Entry(nil, "ready to build"),
		Entry(nil, "Looks good — ready to build."),
		Entry(nil, "I'm ready to build"),
		Entry(nil, "ready to build now"),
		Entry(nil, "build it"),
		Entry(nil, "go ahead and build it"),
		Entry(nil, "let's build it"),
		Entry(nil, "LGTM, ready to build"),
		Entry(nil, "lgtm - ready to build!"),
		Entry(nil, "looks good, ready to build"),
		Entry(nil, "ship it, ready to build"),
		Entry(nil, "approved, ready to build"),
		Entry(nil, "go, ready to build"),
		Entry(nil, "yes, ready to build"),
		Entry(nil, "ok, ready to build"),
	)

	DescribeTable("does not accept anything else",
		func(text string) { Expect(workflow.OpensGate(text)).To(BeFalse()) },
		Entry(nil, "no"),
		Entry(nil, "not yet"),
		Entry(nil, "yes, but change the title"),
		Entry(nil, "yesterday"),
		Entry(nil, ""),
		Entry(nil, "not ready to build"),
		Entry(nil, "Not ready to build."),
		Entry(nil, "not yet ready to build"),
		Entry(nil, "I'm not ready to build"),
		Entry(nil, "don't build it"),
		Entry(nil, "do not build"),
		Entry(nil, "no changes, ready to build"),
		Entry(nil, "if you change the title, ready to build"),
		Entry(nil, "ship it after you fix the bug, ready to build"),
		Entry(nil, "ready to build, but split the storage change"),
		Entry(nil, "yes, no changes needed"),
		Entry(nil, "ready to build?"),
		Entry(nil, "Is this ready to build?"),
		Entry(nil, "should we build it?"),
		Entry(nil, "maybe ready to build"),
		Entry(nil, "I think it is ready to build"),
	)

	DescribeTable("accepts each decided word as a plain yes",
		func(text string) { Expect(workflow.OpensGate(text)).To(BeTrue()) },
		Entry(nil, "yes"),
		Entry(nil, "yep"),
		Entry(nil, "yeah"),
		Entry(nil, "ok"),
		Entry(nil, "okay"),
		Entry(nil, "lgtm"),
		Entry(nil, "LGTM!"),
	)

	DescribeTable("refuses a word that is not decided as a plain yes",
		func(text string) {
			Expect(workflow.OpensGate(text)).To(BeFalse())
			Expect(workflow.RequestsBuild(text)).To(BeFalse())
		},
		Entry(nil, "sure"),
		Entry(nil, "yup"),
		Entry(nil, "y"),
		Entry(nil, "k"),
		Entry(nil, "fine"),
		Entry(nil, "approved"),
		Entry(nil, "ship it"),
		Entry(nil, "go"),
		Entry(nil, "looks good"),
		Entry(nil, "sounds good"),
		Entry(nil, "+1"),
		Entry(nil, "true"),
	)

	DescribeTable("refuses a decided word that carries a clause",
		func(text string) { Expect(workflow.OpensGate(text)).To(BeFalse()) },
		Entry(nil, "yep, but split the storage change"),
		Entry(nil, "ok if the tests pass"),
		Entry(nil, "lgtm after you rename it"),
		Entry(nil, "okay?"),
	)

	DescribeTable("accepts a writer's explicit request to build",
		func(text string) { Expect(workflow.RequestsBuild(text)).To(BeTrue()) },
		Entry(nil, "ready to build"),
		Entry(nil, "Ready to build!"),
		Entry(nil, "Looks good — ready to build."),
		Entry(nil, "build it"),
		Entry(nil, "go ahead and build it"),
		Entry(nil, "let's build it"),
		Entry(nil, "please build it"),
		Entry(nil, "LGTM, ready to build"),
		Entry(nil, "approved, ready to build"),
	)

	DescribeTable("does not take a plain answer as a request to build",
		func(text string) { Expect(workflow.RequestsBuild(text)).To(BeFalse()) },
		Entry(nil, "yes"),
		Entry(nil, "ok"),
		Entry(nil, "lgtm"),
		Entry(nil, "looks good"),
		Entry(nil, "not ready to build"),
		Entry(nil, "do not build"),
		Entry(nil, "ready to build?"),
		Entry(nil, ""),
	)
})

var _ = Describe("planning markers", func() {
	It("round-trips an issue planned from another", func() {
		body := workflow.PlannedMarker(12, "first-slice") + "\n\nA body."

		from, key, found := workflow.PlannedFrom(body)
		Expect(found).To(BeTrue())
		Expect(from).To(Equal(12))
		Expect(key).To(Equal("first-slice"))
	})

	It("does not match a body planned from another issue or with no marker", func() {
		_, _, found := workflow.PlannedFrom("A body with no marker")
		Expect(found).To(BeFalse())
	})

	It("renders the closed issue's slice list under its marker", func() {
		body := workflow.RenderSlices(12, []workflow.SliceRef{
			{Key: "first", Title: "First", Number: 20},
			{Key: "second", Title: "Second", Number: 21},
		})

		Expect(body).To(HavePrefix(workflow.SlicesMarker(12)))
		Expect(body).To(ContainSubstring("- #20 First"))
		Expect(body).To(ContainSubstring("- #21 Second"))
	})
})

var _ = Describe("a wayfinder map's body", func() {
	mapProposal := workflow.PlannerMap{
		Destination: "Ship the export.", Notes: "Use the CLI.",
		NotYetSpecified: "How accounts are matched.", OutOfScope: "Billing.",
	}

	It("lays out the map and keeps the writer's text under Original request", func() {
		body := workflow.RenderMap("Make exports work.", mapProposal)

		Expect(body).To(Equal("## Destination\n\nShip the export.\n\n## Notes\n\nUse the CLI.\n\n" +
			"## Decisions so far\n\n## Not yet specified\n\nHow accounts are matched.\n\n" +
			"## Out of scope\n\nBilling.\n\n## Original request\n\nMake exports work."))
	})

	It("leaves an empty section ready for its first decision", func() {
		body := workflow.RenderMap("Make exports work.", mapProposal)

		decision := "- [Add x](https://github.com/o/r/issues/2): Use it"

		once, err := workflow.AppendDecision(body, decision)
		Expect(err).NotTo(HaveOccurred())
		Expect(once).To(ContainSubstring("## Decisions so far\n\n" + decision + "\n\n## Not yet specified"))

		twice, err := workflow.AppendDecision(once, decision)
		Expect(err).NotTo(HaveOccurred())
		Expect(twice).To(Equal(once), "the second apply added the decision again")
	})
})

var _ = Describe("deriving a ticket's labels", func() {
	It("routes research to the Researcher and the rest to the Planner", func() {
		research, err := workflow.TicketLabels(workflow.TicketResearch)
		Expect(err).NotTo(HaveOccurred())
		Expect(research).To(ConsistOf(workflow.LabelResearchTicket, workflow.LabelResearch))

		for _, typ := range []workflow.TicketType{workflow.TicketGrilling, workflow.TicketPrototype, workflow.TicketTask} {
			labels, err := workflow.TicketLabels(typ)
			Expect(err).NotTo(HaveOccurred())
			Expect(labels).To(ContainElement(workflow.LabelPlan))
			Expect(labels).NotTo(ContainElement(workflow.LabelResearch))
		}

		prototype, err := workflow.TicketLabels(workflow.TicketPrototype)
		Expect(err).NotTo(HaveOccurred())
		Expect(prototype).To(ContainElement(workflow.LabelWayfinderPrototype))
	})

	It("refuses a type it does not know", func() {
		_, err := workflow.TicketLabels("epic")
		Expect(err).To(HaveOccurred())
	})
})

// quote renders text as a JSON string.
func quote(text string) string {
	raw, err := json.Marshal(text)
	Expect(err).NotTo(HaveOccurred())

	return string(raw)
}

// mustJSON makes an output's wire form.
func mustJSON(out workflow.PlannerOutput) string {
	raw, err := json.Marshal(out)
	Expect(err).NotTo(HaveOccurred())

	return string(raw)
}
