package workflow_test

import (
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("ParseRetroTarget", func() {
	DescribeTable("accepts the three kinds",
		func(text string, kind workflow.RetroTargetKind, name string) {
			got, err := workflow.ParseRetroTarget(text)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Kind).To(Equal(kind))
			Expect(got.Name).To(Equal(name))
			Expect(got.String()).To(Equal(text))
		},
		Entry(nil, "skill:implement", workflow.TargetSkill, "implement"),
		Entry(nil, "skill:code-review", workflow.TargetSkill, "code-review"),
		Entry(nil, "docs:docs/agents/issue-tracker.md", workflow.TargetDocs, "docs/agents/issue-tracker.md"),
		Entry(nil, "docs:AGENTS.md", workflow.TargetDocs, "AGENTS.md"),
		Entry(nil, "factory:retro-dashboard", workflow.TargetFactory, "retro-dashboard"),
	)

	DescribeTable("fails closed on anything else",
		func(text string) {
			_, err := workflow.ParseRetroTarget(text)
			Expect(err).To(HaveOccurred())
		},
		Entry("empty", ""),
		Entry("no kind", "implement"),
		Entry("unknown kind", "role:implement"),
		Entry("empty name", "skill:"),
		Entry("uppercase", "skill:Implement"),
		Entry("skill path", "skill:skills/implement"),
		Entry("docs outside agents", "docs:README.md"),
		Entry("docs escaping", "docs:docs/agents/../secret.md"),
		Entry("docs absolute", "docs:/etc/passwd"),
		Entry("docs nested", "docs:docs/agents/deep/file.md"),
		Entry("factory path", "factory:foreman/run.go"),
		Entry("marker", "skill:implement<!-- factory:"),
	)
})

var _ = Describe("ParseRetro", func() {
	valid := `{"version":1,"findings":[{"target":"skill:implement","group":"guide-misses-tests",` +
		`"severity":"medium","claim":"the guide never mentions tests","evidence":"I searched and found none"},` +
		`{"target":"docs:docs/agents/issue-tracker.md","group":"no-issue-commands",` +
		`"severity":"low","claim":"tracker doc omits gh commands","evidence":"read it"}]}`

	It("reads every finding, with its target, group, severity, claim and evidence", func() {
		findings, err := workflow.ParseRetro(valid)
		Expect(err).NotTo(HaveOccurred())
		Expect(findings).To(HaveLen(2))
		Expect(findings[0]).To(Equal(workflow.RetroFinding{
			Target:   workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"},
			Group:    "guide-misses-tests",
			Severity: workflow.SeverityMedium,
			Claim:    "the guide never mentions tests",
			Evidence: "I searched and found none",
		}))
	})

	It("accepts a retro with no findings, and one whose evidence spans lines", func() {
		findings, err := workflow.ParseRetro(`{"version":1,"findings":[]}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(findings).To(BeEmpty())

		findings, err = workflow.ParseRetro(`{"version":1,"findings":[{"target":"factory:retro",` +
			`"group":"slow","severity":"high","claim":"slow","evidence":"line one\nline two"}]}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(findings[0].Evidence).To(Equal("line one\nline two"))
	})

	DescribeTable("rejects a whole invalid retro",
		func(text string) {
			_, err := workflow.ParseRetro(text)
			Expect(err).To(HaveOccurred())
		},
		Entry("not JSON", `nope`),
		Entry("no version", `{"findings":[]}`),
		Entry("wrong version", `{"version":2,"findings":[]}`),
		Entry("no findings key", `{"version":1}`),
		Entry("null findings", `{"version":1,"findings":null}`),
		Entry("unknown field", `{"version":1,"findings":[],"extra":true}`),
		Entry("repeated key", `{"version":1,"findings":[],"findings":[]}`),
		Entry("repeated key in a finding", `{"version":1,"findings":[{"target":"skill:implement",`+
			`"group":"g","severity":"low","claim":"c","claim":"d","evidence":"e"}]}`),
		Entry("trailing data", `{"version":1,"findings":[]} {"version":1,"findings":[]}`),
		Entry("unknown finding field", `{"version":1,"findings":[{"target":"skill:implement",`+
			`"group":"g","severity":"low","claim":"c","evidence":"e","extra":1}]}`),
		Entry("bad target", `{"version":1,"findings":[{"target":"other:x",`+
			`"group":"g","severity":"low","claim":"c","evidence":"e"}]}`),
		Entry("bad group", `{"version":1,"findings":[{"target":"skill:implement",`+
			`"group":"Not A Key","severity":"low","claim":"c","evidence":"e"}]}`),
		Entry("bad severity", `{"version":1,"findings":[{"target":"skill:implement",`+
			`"group":"g","severity":"urgent","claim":"c","evidence":"e"}]}`),
		Entry("two-line claim", `{"version":1,"findings":[{"target":"skill:implement",`+
			`"group":"g","severity":"low","claim":"c\nmore","evidence":"e"}]}`),
		Entry("empty claim", `{"version":1,"findings":[{"target":"skill:implement",`+
			`"group":"g","severity":"low","claim":" ","evidence":"e"}]}`),
		Entry("empty evidence", `{"version":1,"findings":[{"target":"skill:implement",`+
			`"group":"g","severity":"low","claim":"c","evidence":""}]}`),
		Entry("marker in text", `{"version":1,"findings":[{"target":"skill:implement",`+
			`"group":"g","severity":"low","claim":"c","evidence":"<!-- factory:handoff -->"}]}`),
	)
})

var _ = Describe("retro comments", func() {
	marker := "<!-- factory:retro issue=5 role=reviewer key=sha-head -->"

	// comment is a retro comment as foreman posts it.
	comment := func(markerLine string, findings ...workflow.RetroFinding) workflow.Item {
		body, err := workflow.RenderRetro(markerLine, findings)
		Expect(err).NotTo(HaveOccurred())

		return workflow.Item{ID: 7, Body: body}
	}

	It("keys the marker by issue, agent role and run key", func() {
		Expect(workflow.RetroMarker(5, workflow.Reviewer, "sha-head")).To(Equal(marker))
	})

	It("round-trips the findings it renders", func() {
		findings := []workflow.RetroFinding{{
			Target:   workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"},
			Group:    "guide-misses-tests",
			Severity: workflow.SeverityHigh,
			Claim:    "the guide never mentions tests",
			Evidence: "I searched and found none",
		}}

		got, ok, err := workflow.ParseRetroComment(comment(marker, findings...).Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(got.Issue).To(Equal(5))
		Expect(got.Role).To(Equal(workflow.Reviewer))
		Expect(got.Key).To(Equal("sha-head"))
		Expect(got.Findings).To(Equal(findings))
	})

	It("ignores a comment that is not a retro", func() {
		_, ok, err := workflow.ParseRetroComment("<!-- agent-retro:5:abc -->\nold")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("refuses a marker or a comment it could not read back whole", func() {
		_, err := workflow.RenderRetro("<!-- factory:retro issue=5 role=reviewer key=a b -->", nil)
		Expect(err).To(HaveOccurred())

		huge := []workflow.RetroFinding{{
			Target: workflow.RetroTarget{Kind: workflow.TargetFactory, Name: "retro"},
			Group:  "huge", Severity: workflow.SeverityLow,
			Claim: "huge", Evidence: strings.Repeat("é", workflow.CommentLimitChars),
		}}
		_, err = workflow.RenderRetro(marker, huge)
		Expect(err).To(MatchError(ContainSubstring("over the")))
	})

	It("refuses more findings than one retro may report", func() {
		findings := make([]string, 0, workflow.MaxRetroFindings+1)
		for range workflow.MaxRetroFindings + 1 {
			findings = append(findings, `{"target":"factory:retro","group":"g","severity":"low",`+
				`"claim":"c","evidence":"e"}`)
		}

		_, err := workflow.ParseRetro(`{"version":1,"findings":[` + strings.Join(findings, ",") + `]}`)
		Expect(err).To(MatchError(ContainSubstring("at most")))
	})

	It("reports a malformed factory retro as an error", func() {
		_, ok, err := workflow.ParseRetroComment("<!-- factory:retro issue=x role=reviewer key=a -->\n")
		Expect(ok).To(BeTrue())
		Expect(err).To(HaveOccurred())
	})

	Describe("existing groups", func() {
		It("keeps one key and claim per target from the same agent role, skipping the current run", func() {
			first := comment("<!-- factory:retro issue=5 role=reviewer key=earlier -->", workflow.RetroFinding{
				Target: workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"},
				Group:  "guide-misses-tests", Severity: workflow.SeverityLow,
				Claim: "the guide never mentions tests", Evidence: "SECRET EVIDENCE",
			})
			repeat := comment("<!-- factory:retro issue=5 role=reviewer key=later -->", workflow.RetroFinding{
				Target: workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"},
				Group:  "guide-misses-tests", Severity: workflow.SeverityLow,
				Claim: "same group, different wording", Evidence: "MORE SECRET EVIDENCE",
			})
			current := comment(marker, workflow.RetroFinding{
				Target: workflow.RetroTarget{Kind: workflow.TargetFactory, Name: "retro"},
				Group:  "current-run", Severity: workflow.SeverityLow,
				Claim: "this run", Evidence: "e",
			})

			groups, err := workflow.RetroGroups([]workflow.Item{first, repeat, current}, workflow.Reviewer, "sha-head")
			Expect(err).NotTo(HaveOccurred())
			Expect(groups).To(Equal([]workflow.RetroGroup{{
				Target: workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"},
				Key:    "guide-misses-tests",
				Claim:  "the guide never mentions tests",
			}}))
		})

		It("leaves another agent role's groups out", func() {
			impl := comment("<!-- factory:retro issue=5 role=implementer key=abc -->", workflow.RetroFinding{
				Target: workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"},
				Group:  "impl-only", Severity: workflow.SeverityLow, Claim: "c", Evidence: "IMPL EVIDENCE",
			})

			groups, err := workflow.RetroGroups([]workflow.Item{impl}, workflow.Reviewer, "sha-head")
			Expect(err).NotTo(HaveOccurred())
			Expect(groups).To(BeEmpty())
		})

		It("sorts and bounds the groups it shows", func() {
			items := make([]workflow.Item, 0, workflow.MaxRetroGroups+5)
			for i := range workflow.MaxRetroGroups + 5 {
				key := fmt.Sprintf("group-%03d", i)
				items = append(items, comment("<!-- factory:retro issue=5 role=reviewer key=k"+key+" -->",
					workflow.RetroFinding{
						Target: workflow.RetroTarget{Kind: workflow.TargetFactory, Name: "retro"},
						Group:  key, Severity: workflow.SeverityLow, Claim: key, Evidence: "e",
					}))
			}

			groups, err := workflow.RetroGroups(items, workflow.Reviewer, "sha-head")
			Expect(err).NotTo(HaveOccurred())
			Expect(groups).To(HaveLen(workflow.MaxRetroGroups))
			Expect(groups[0].Key).To(Equal("group-000"))
		})
	})
})
