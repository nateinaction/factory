package prompt

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("Implementer", func() {
	issue := workflow.Issue{Number: 4, Title: "T", Body: "B"}

	It("carries the issue after the role prompt and the docs", func() {
		got := Implementer([]Doc{{Path: "AGENTS.md", Text: "RULES"}}, "o/r", issue, "")
		Expect(got).To(Equal(implementerRolePrompt + "\n\n# Repository docs\n\n## AGENTS.md\n\nRULES\n" +
			IssueMaterialHeading + "Repository: o/r\nIssue: #4\nTitle: T\n\nB"))
	})

	It("ends a rework with the review to fix", func() {
		Expect(Implementer(nil, "o/r", issue, "FEEDBACK")).To(HaveSuffix(
			"Fix every high and medium finding below on top of the existing commits, then commit.\n\nReview:\nFEEDBACK"))
	})

	It("cuts the body and the feedback to the body limit, by character", func() {
		long := strings.Repeat("é", workflow.BodyLimitChars+50)
		got := Implementer(nil, "o/r", workflow.Issue{Number: 1, Title: "T", Body: long}, long)
		Expect(strings.Count(got, "é")).To(Equal(2 * workflow.BodyLimitChars))
	})
})

var _ = Describe("Retro and Reviewer", func() {
	It("names the issue and where the retro goes", func() {
		r := Retro("o/r", 9, "/w/out.md")
		Expect(r).To(And(ContainSubstring("#9 of o/r"), ContainSubstring("to /w/out.md;")))
	})

	It("names the fixed point, the spec and the output", func() {
		p := Reviewer(nil, "main", "/w/spec.md", "/w/out.json")
		Expect(p).To(And(
			ContainSubstring("origin/main"), ContainSubstring("saved at /w/spec.md"), ContainSubstring("JSON to /w/out.json"),
		))
	})
})

var _ = Describe("ReadRepoDocs", func() {
	It("reads in a fixed order, skips missing docs, and stays under the cap", func() {
		dir := GinkgoT().TempDir()
		write := func(name, text string) {
			path := filepath.Join(dir, name)
			Expect(os.MkdirAll(filepath.Dir(path), 0o700)).To(Succeed())
			Expect(os.WriteFile(path, []byte(text), 0o600)).To(Succeed())
		}
		write("docs/agents/b.md", "B")
		write("docs/agents/a.md", "A")
		write("GLOSSARY.md", strings.Repeat("g", DocsLimitChars+10))
		write("AGENTS.md", "root")

		docs, err := ReadRepoDocs(dir)
		Expect(err).NotTo(HaveOccurred())
		names := make([]string, 0, len(docs))
		total := 0
		for _, d := range docs {
			names = append(names, d.Path)
			total += len(d.Text)
		}
		Expect(names).To(Equal([]string{"AGENTS.md", "GLOSSARY.md", "docs/agents/a.md", "docs/agents/b.md"}))
		Expect(total).To(BeNumerically("<=", DocsLimitChars))
	})
})
