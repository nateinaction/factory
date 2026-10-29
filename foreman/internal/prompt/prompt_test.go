package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("Implementer", func() {
	issue := workflow.Issue{Number: 4, Title: "T", Body: "B"}

	It("carries the issue after the role prompt and the docs", func() {
		got := Implementer([]Doc{{Path: "AGENTS.md", Text: "RULES"}}, "o/r", issue, "", nil, "/w/out.json")
		Expect(got).To(Equal(implementerRolePrompt + "\n\n# Repository docs\n\n## AGENTS.md\n\nRULES\n" +
			IssueMaterialHeading + "Repository: o/r\nIssue: #4\nTitle: T\n\nB\n\nWrite the JSON to /w/out.json."))
	})

	It("ends a rework with the review to fix", func() {
		Expect(Implementer(nil, "o/r", issue, "FEEDBACK", nil, "/w/out.json")).To(HaveSuffix(
			"Fix every high and medium finding below on top of the existing commits and answer each one " +
				"in \"replies\".\n\nReview:\nFEEDBACK\n\nWrite the JSON to /w/out.json."))
	})

	It("carries every comment since the marker, oldest first", func() {
		got := Implementer(nil, "o/r", issue, "", []workflow.Item{
			{ID: 3, Body: "first", Author: "o"},
			{ID: 4, Body: "second", Author: "o"},
		}, "/w/out.json")
		Expect(got).To(ContainSubstring(ThreadHeading + "\n\n**o:** first\n\n**o:** second"))
		Expect(strings.Index(got, "first")).To(BeNumerically("<", strings.Index(got, "second")))
	})

	It("cuts the body and the feedback to the body limit, by character", func() {
		long := strings.Repeat("é", workflow.BodyLimitChars+50)
		got := Implementer(nil, "o/r", workflow.Issue{Number: 1, Title: "T", Body: long}, long, nil, "/w/out.json")
		Expect(strings.Count(got, "é")).To(Equal(2 * workflow.BodyLimitChars))
	})

	It("keeps the thread within its own limit", func() {
		long := strings.Repeat("é", workflow.BodyLimitChars+50)
		got := Implementer(nil, "o/r", issue, "", []workflow.Item{{ID: 1, Body: long, Author: "o"}}, "/w/out.json")
		Expect(strings.Count(got, "é")).To(Equal(workflow.BodyLimitChars))
	})
})

var _ = Describe("Retro and Reviewer", func() {
	It("names the issue, the output file and the JSON shape", func() {
		r := Retro(RetroRequest{Role: workflow.Implementer, Repo: "o/r", Issue: 9, OutPath: "/w/retro.json"})
		Expect(r).To(And(
			ContainSubstring("#9 of o/r"), ContainSubstring("write retro.json to /w/retro.json"),
			ContainSubstring(`"target"`), ContainSubstring(`"group"`), ContainSubstring(`"evidence"`),
		))
	})

	It("shows the existing group keys and one-line claims for each target", func() {
		r := Retro(RetroRequest{
			Role: workflow.Reviewer, Repo: "o/r", Issue: 9, OutPath: "/w/retro.json",
			Groups: []workflow.RetroGroup{
				{
					Target: workflow.RetroTarget{Kind: workflow.TargetSkill, Name: "implement"},
					Key:    "guide-misses-tests", Claim: "the guide never mentions tests",
				},
				{
					Target: workflow.RetroTarget{Kind: workflow.TargetFactory, Name: "retro"},
					Key:    "slow-retro", Claim: "the retro takes too long",
				},
			},
		})
		Expect(r).To(ContainSubstring("## factory:retro\n\n- `slow-retro`: the retro takes too long"))
		Expect(r).To(ContainSubstring("## skill:implement\n\n- `guide-misses-tests`: the guide never mentions tests"))
		Expect(r).To(ContainSubstring("findings may target the Implementer's skills"))
	})

	It("says so when there are no existing groups", func() {
		Expect(Retro(RetroRequest{Role: workflow.Implementer, Repo: "o/r", Issue: 1, OutPath: "/w/x"})).To(
			ContainSubstring("No existing groups."))
	})

	It("names the fixed point, the spec and the output", func() {
		p := Reviewer(nil, "main", "/w/spec.md", "/w/out.json")
		Expect(p).To(And(
			ContainSubstring("origin/main"), ContainSubstring("saved at /w/spec.md"), ContainSubstring("JSON to /w/out.json"),
		))
	})
})

var _ = Describe("ReadRepoDocs", func() {
	var dir string

	// write puts a document in the repository at dir.
	write := func(name, text string) {
		path := filepath.Join(dir, name)
		Expect(os.MkdirAll(filepath.Dir(path), 0o700)).To(Succeed())
		Expect(os.WriteFile(path, []byte(text), 0o600)).To(Succeed())
	}

	// chars counts the characters across docs.
	chars := func(docs []Doc) int {
		total := 0
		for _, d := range docs {
			total += utf8.RuneCountInString(d.Text)
		}

		return total
	}

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
	})

	It("reads in a fixed order, skips missing docs, and stays under the cap", func() {
		write("docs/agents/b.md", "B")
		write("docs/agents/a.md", "A")
		write("GLOSSARY.md", strings.Repeat("g", DocsLimitChars+10))
		write("AGENTS.md", "root")

		docs, err := ReadRepoDocs(dir)
		Expect(err).NotTo(HaveOccurred())

		names := make([]string, 0, len(docs))
		for _, d := range docs {
			names = append(names, d.Path)
		}

		Expect(names).To(Equal([]string{"AGENTS.md", "GLOSSARY.md", "docs/agents/a.md", "docs/agents/b.md"}))
		Expect(chars(docs)).To(BeNumerically("<=", DocsLimitChars))
	})

	It("counts the cap in characters, so multi-byte docs neither overrun it nor break later docs", func() {
		write("AGENTS.md", strings.Repeat("é", DocsLimitChars))
		write("GLOSSARY.md", "g")
		write("docs/agents/a.md", "A")

		docs, err := ReadRepoDocs(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(chars(docs)).To(Equal(DocsLimitChars))
		Expect(docs[1:]).To(HaveEach(HaveField("Text", BeEmpty())))
	})

	It("refuses a document that is a symlink out of the repository", func() {
		secret := filepath.Join(GinkgoT().TempDir(), "openrouter-key")
		Expect(os.WriteFile(secret, []byte("or-key"), 0o600)).To(Succeed())
		Expect(os.Symlink(secret, filepath.Join(dir, "AGENTS.md"))).To(Succeed())

		docs, err := ReadRepoDocs(dir)
		Expect(err).To(MatchError(ContainSubstring("AGENTS.md")))
		Expect(docs).To(BeNil())
	})
})
