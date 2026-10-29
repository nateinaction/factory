package workflow_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// factoryApp is the bot login of the factory's App for an agent role, as GitHub reports it.
func factoryApp(role workflow.Role) string {
	return "o-factory-" + string(role) + "[bot]"
}

var _ = Describe("ParseRole", func() {
	DescribeTable("accepts the known set",
		func(name string) {
			Expect(workflow.ParseRole(name)).To(BeEquivalentTo(name))
		},
		Entry(nil, "planner"), Entry(nil, "researcher"), Entry(nil, "implementer"),
		Entry(nil, "reviewer"), Entry(nil, "curator"),
	)

	DescribeTable("fails closed on anything else",
		func(name string) {
			_, err := workflow.ParseRole(name)
			Expect(err).To(MatchError(ContainSubstring("unknown agent role")))
		},
		Entry("empty", ""), Entry("capitalised", "Implementer"), Entry("unknown", "worker"),
		Entry("trailing space", "implementer "), Entry("path", "../reviewer"),
	)
})

var _ = Describe("ParseIssueNumber", func() {
	It("accepts up to nine digits", func() {
		Expect(workflow.ParseIssueNumber("123456789")).To(Equal(123456789))
	})

	DescribeTable("rejects anything but digits",
		func(text string) {
			_, err := workflow.ParseIssueNumber(text)
			Expect(err).To(HaveOccurred())
		},
		Entry(nil, ""), Entry(nil, "12a"), Entry(nil, "-1"), Entry(nil, "1234567890"),
		Entry(nil, " 1"), Entry(nil, "1\n"), Entry(nil, "../1"),
	)
})

var _ = Describe("names and markers", func() {
	It("puts an issue's work on agent/issue-<n>", func() {
		Expect(workflow.BranchName(7)).To(Equal("agent/issue-7"))
	})

	It("keys reviews and retros by commit", func() {
		Expect(workflow.ReviewMarker("abc")).To(Equal("<!-- agent-review:abc -->"))
		Expect(workflow.RetroMarker(5, workflow.Implementer, "abc")).To(
			Equal("<!-- factory:retro issue=5 role=implementer key=abc -->"))
	})

	It("makes a pull request body close the issue, without doubling a link the model wrote", func() {
		Expect(workflow.PRBody(5, "Fixes the bug")).To(Equal("Fixes the bug\n\nCloses #5"))
		Expect(workflow.PRBody(5, "Closes #5")).To(Equal("Closes #5"))
		Expect(workflow.PRBody(5, "Part of #5")).To(Equal("Part of #5"))
	})
})

var _ = Describe("Blocking", func() {
	It("keeps high and medium findings", func() {
		got := workflow.Blocking([]workflow.ReviewFinding{
			{Severity: workflow.SeverityHigh}, {Severity: workflow.SeverityMedium}, {Severity: workflow.SeverityLow},
		})
		Expect(got).To(Equal([]workflow.ReviewFinding{
			{Severity: workflow.SeverityHigh}, {Severity: workflow.SeverityMedium},
		}))
	})
})

var _ = Describe("RenderReview", func() {
	It("groups findings by severity, most severe first, against the reviewed commit", func() {
		got := workflow.RenderReview("sha1", workflow.ReviewerOutput{
			Summary: "all fine",
			Findings: []workflow.ReviewFinding{
				{Severity: workflow.SeverityLow, Path: "a.go", Claim: "nit"},
				{Severity: workflow.SeverityHigh, Path: "a.go", Line: 3, Claim: "bad"},
			},
		})
		Expect(got).To(Equal("<!-- agent-review:sha1 -->\n## Agent review\n\nall fine\n\n" +
			"### High (1)\n- **`a.go:3`**: bad\n\n### Low (1)\n- **`a.go`**: nit"))
	})

	It("says so when there are no findings", func() {
		Expect(workflow.RenderReview("s", workflow.ReviewerOutput{Summary: "ok"})).To(HaveSuffix("\n\nNo findings."))
	})

	It("is cut to the comment limit", func() {
		got := workflow.RenderReview("s", workflow.ReviewerOutput{
			Summary: strings.Repeat("x", workflow.CommentLimitChars+10),
		})
		Expect([]rune(got)).To(HaveLen(workflow.CommentLimitChars))
	})
})

var _ = Describe("ReviewRounds", func() {
	// The Reviewer's App posted every marked review, so each one is the factory's own record.
	reviews := []workflow.Item{
		{Body: workflow.ReviewMarker("a") + "\nx", Author: factoryApp(workflow.Reviewer)},
		{Body: workflow.ReviewMarker("a") + "\ny", Author: factoryApp(workflow.Reviewer)},
		{Body: workflow.ReviewMarker("b"), Author: factoryApp(workflow.Reviewer)},
		{Body: "human review"},
	}

	It("counts distinct reviewed commits, a repeat of one counting once", func() {
		Expect(workflow.ReviewRounds(reviews, nil, "a", "o")).To(Equal(2))
	})

	It("counts the current commit before its review is posted", func() {
		Expect(workflow.ReviewRounds(reviews, nil, "c", "o")).To(Equal(3))
	})
})
