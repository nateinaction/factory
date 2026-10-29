package workflow_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// researchRun is a valid researcher-output.json.
const researchRun = `{"version":1,"branch":"emulate","resolution":"Findings are in the file.",` +
	`"decision_line":"Use emulate."}`

var _ = Describe("ParseResearcherOutput", func() {
	It("reads the branch, the resolution and the decision line", func() {
		out, err := workflow.ParseResearcherOutput(researchRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(workflow.ResearcherOutput{
			Version: 1, Branch: "emulate", Resolution: "Findings are in the file.",
			DecisionLine: "Use emulate.",
		}))
	})

	DescribeTable("rejects the whole file",
		func(text, want string) {
			_, err := workflow.ParseResearcherOutput(text)
			Expect(err).To(HaveOccurred())
			Expect(problems(err)).To(ContainElement(ContainSubstring(want)))
		},
		Entry("empty", ``, "empty"),
		Entry("not JSON", `nope`, "not valid JSON"),
		Entry("missing version", `{"branch":"b","resolution":"r","decision_line":"d"}`, "version"),
		Entry("unknown field", `{"version":1,"branch":"b","resolution":"r","decision_line":"d","labels":["x"]}`,
			`unknown field "labels"`),
		Entry("repeated key", `{"version":1,"branch":"a","branch":"b","resolution":"r","decision_line":"d"}`,
			"repeats"),
		Entry("factory marker in the resolution",
			`{"version":1,"branch":"b","resolution":"<!-- factory:handoff -->","decision_line":"d"}`, "marker"),
		Entry("no branch", `{"version":1,"resolution":"r","decision_line":"d"}`, "branch"),
		Entry("a branch with a slash", `{"version":1,"branch":"a/b","resolution":"r","decision_line":"d"}`, "branch"),
		Entry("a branch that escapes", `{"version":1,"branch":"..","resolution":"r","decision_line":"d"}`, "branch"),
		Entry("no resolution", `{"version":1,"branch":"b","decision_line":"d"}`, "resolution"),
		Entry("no decision line", `{"version":1,"branch":"b","resolution":"r"}`, "decision_line"),
		Entry("a decision line over several lines",
			`{"version":1,"branch":"b","resolution":"r","decision_line":"one\ntwo"}`, "one line"),
	)
})

var _ = Describe("research branches", func() {
	It("names the findings branch after the output's name", func() {
		branch, err := workflow.ResearchBranch("emulate")
		Expect(err).NotTo(HaveOccurred())
		Expect(branch).To(Equal("research/emulate"))
	})

	DescribeTable("refuses a name that is not one safe branch segment",
		func(name string) {
			_, err := workflow.ResearchBranch(name)
			Expect(err).To(HaveOccurred())
		},
		Entry(nil, ""),
		Entry(nil, "a/b"),
		Entry(nil, ".."),
		Entry(nil, "a..b"),
		Entry(nil, ".hidden"),
		Entry(nil, "-leading"),
		Entry(nil, "has space"),
		Entry(nil, "trailing."),
		Entry(nil, strings.Repeat("a", 65)),
	)
})

var _ = Describe("the map's decisions", func() {
	It("reads the parent from the Part of convention at the top of a ticket", func() {
		number, ok := workflow.PartOf("Part of #12\n\n## Question\n\nWhich?")
		Expect(ok).To(BeTrue())
		Expect(number).To(Equal(12))
	})

	DescribeTable("finds no parent",
		func(body string) {
			_, ok := workflow.PartOf(body)
			Expect(ok).To(BeFalse())
		},
		Entry(nil, ""),
		Entry(nil, "## Question\n\nPart of #12"),
		Entry(nil, "Part of the problem"),
		Entry(nil, "Part of #nope"),
	)

	It("renders a one-line index entry from the ticket's name and gist", func() {
		Expect(workflow.DecisionEntry("Use [emulate]", "https://github.com/o/r/issues/7", "It\nworks")).
			To(Equal(`- [Use \[emulate\]](https://github.com/o/r/issues/7): It works`))
	})

	// mapBody is a map with one decision already recorded.
	const mapBody = "## Destination\n\nA spec.\n\n## Decisions so far\n\n" +
		"- [Old](https://github.com/o/r/issues/1): it was so\n\n## Not yet specified\n\nFog.\n"

	It("appends the line to the Decisions so far section", func() {
		body, err := workflow.AppendDecision(mapBody, "- [New](https://github.com/o/r/issues/2): it is")
		Expect(err).NotTo(HaveOccurred())
		Expect(body).To(Equal("## Destination\n\nA spec.\n\n## Decisions so far\n\n" +
			"- [Old](https://github.com/o/r/issues/1): it was so\n" +
			"- [New](https://github.com/o/r/issues/2): it is\n\n## Not yet specified\n\nFog.\n"))
	})

	It("changes nothing when the line is already there", func() {
		line := "- [Old](https://github.com/o/r/issues/1): it was so"

		body, err := workflow.AppendDecision(mapBody, line)
		Expect(err).NotTo(HaveOccurred())
		Expect(body).To(Equal(mapBody))
	})

	It("refuses a map with no Decisions so far section", func() {
		_, err := workflow.AppendDecision("## Destination\n\nx\n", "- entry")
		Expect(err).To(MatchError(ContainSubstring("Decisions so far")))
	})

	It("keeps the blank line under an empty section", func() {
		body, err := workflow.AppendDecision("## Decisions so far\n\n## Not yet specified\n\nFog.\n", "- first")
		Expect(err).NotTo(HaveOccurred())
		Expect(body).To(Equal("## Decisions so far\n\n- first\n\n## Not yet specified\n\nFog.\n"))
	})

	It("builds an issue's web URL", func() {
		ref := workflow.IssueRef{Repo: mustRepo("o/r"), Number: 7}
		Expect(workflow.IssueURL(ref)).To(Equal("https://github.com/o/r/issues/7"))
	})
})

var _ = Describe("a research resolution", func() {
	It("keys the comment to its findings branch", func() {
		Expect(workflow.ResearchMarker("research/emulate")).To(
			Equal("<!-- factory:research branch=research/emulate -->"))
	})

	It("carries the answer and a link to the branch", func() {
		body := workflow.RenderResolution(mustRepo("o/r"), "research/emulate", "It works.")

		Expect(body).To(And(
			HavePrefix(workflow.ResearchMarker("research/emulate")),
			ContainSubstring("It works."),
			ContainSubstring("[research/emulate](https://github.com/o/r/tree/research/emulate)"),
		))
	})

	It("truncates a resolution over the comment limit, keeping the link to the branch", func() {
		body := workflow.RenderResolution(mustRepo("o/r"), "research/emulate",
			strings.Repeat("é", workflow.CommentLimitChars+50))
		Expect(len([]rune(body))).To(BeNumerically("<=", workflow.CommentLimitChars))
		Expect(body).To(HaveSuffix("[research/emulate](https://github.com/o/r/tree/research/emulate)"))
	})
})
