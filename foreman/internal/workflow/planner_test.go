package workflow_test

import (
	"encoding/json"

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
})

var _ = Describe("the human gate", func() {
	DescribeTable("accepts a writer's approval",
		func(text string) { Expect(workflow.Approves(text)).To(BeTrue()) },
		Entry(nil, "yes"),
		Entry(nil, "Yes!"),
		Entry(nil, "  yes.  "),
		Entry(nil, "ready to build"),
		Entry(nil, "Looks good — ready to build."),
	)

	DescribeTable("does not accept anything else",
		func(text string) { Expect(workflow.Approves(text)).To(BeFalse()) },
		Entry(nil, "no"),
		Entry(nil, "not yet"),
		Entry(nil, "yes, but change the title"),
		Entry(nil, "yesterday"),
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
