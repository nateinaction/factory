// Package prompt builds the prompts foreman gives the model: the agent role's fixed instruction, the
// repository's steering docs, then the issue material, in that order.
package prompt

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// DocsLimitChars caps all repository docs put into one prompt, so the prompt stays under the
// operating system's limit on a single argument.
const DocsLimitChars = 40_000

// IssueMaterialHeading starts the variable part of a prompt. Everything before it is the prefix.
const IssueMaterialHeading = "\n# Issue material\n\n"

// Doc is one repository document put into a prompt.
type Doc struct {
	// Path is relative to the repository root.
	Path string
	// Text is the file's content, cut to fit DocsLimitChars.
	Text string
}

// repoDocPaths are the documents a repository steers runs with, in the order they are read.
var repoDocPaths = []string{"AGENTS.md", "GLOSSARY.md"}

// ReadRepoDocs reads the repository's steering documents in a fixed order: AGENTS.md, GLOSSARY.md,
// then docs/agents/*.md by name. A missing document is skipped; the total is capped.
func ReadRepoDocs(root string) ([]Doc, error) {
	paths := append([]string{}, repoDocPaths...)
	extra, err := filepath.Glob(filepath.Join(root, "docs", "agents", "*.md")) // Glob returns names sorted
	if err != nil {
		return nil, err
	}
	for _, p := range extra {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil, err
		}
		paths = append(paths, filepath.ToSlash(rel))
	}
	var docs []Doc
	budget := DocsLimitChars
	for _, rel := range paths {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if errors.Is(err, fs.ErrNotExist) {
			continue // a repository need not have every document; setup checks are a separate rule
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", rel, err)
		}
		text := workflow.Truncate(string(raw), budget)
		budget -= len(text)
		docs = append(docs, Doc{Path: rel, Text: text})
	}
	return docs, nil
}

// Build joins the three parts in their fixed order: the agent role's prompt, the repository
// docs, then the issue material. The first two are the prefix and carry nothing that varies per run.
func Build(rolePrompt string, docs []Doc, material string) string {
	var b strings.Builder
	b.WriteString(rolePrompt)
	b.WriteString("\n\n# Repository docs\n")
	for _, d := range docs {
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", d.Path, d.Text)
	}
	b.WriteString(IssueMaterialHeading)
	b.WriteString(material)
	return b.String()
}

// implementerRolePrompt is the Implementer's fixed instruction.
const implementerRolePrompt = "Use the `implement` skill to implement the GitHub issue given under " +
	"\"Issue material\". The repository is the current directory, already on its own branch. " +
	"Commit your changes to this branch; do not push."

// reviewerRolePrompt is the Reviewer's fixed instruction.
const reviewerRolePrompt = "Use the `code-review` skill on the current directory. Do not change the " +
	"repository, do not post anything, and do not ask questions. Classify every finding as high " +
	"(a bug, a spec miss or a security problem that must be fixed before merge), medium (a " +
	"documented-standard breach or a notable defect) or low (a judgement call or nit). Write the " +
	`result as JSON: {"summary": "<one paragraph>", "findings": [{"severity": ` +
	`"high|medium|low", "title": "...", "detail": "...", "location": "file:line or empty"}]}. ` +
	"An empty findings list means the change is clean."

// Implementer is the Implementer's prompt; feedback is the latest review, empty on a first run.
func Implementer(docs []Doc, repo string, issue workflow.Issue, feedback string) string {
	rework := ""
	if feedback != "" {
		rework = "\n\nThe open pull request for this branch was reviewed and the review found " +
			"problems. Fix every high and medium finding below on top of the existing " +
			"commits, then commit.\n\nReview:\n" + workflow.Truncate(feedback, workflow.BodyLimitChars)
	}
	return Build(implementerRolePrompt, docs, fmt.Sprintf("Repository: %s\nIssue: #%d\nTitle: %s\n\n%s%s",
		repo, issue.Number, issue.Title, workflow.Truncate(issue.Body, workflow.BodyLimitChars), rework))
}

// Reviewer is the Reviewer's prompt: base is the branch to compare with, specPath holds the
// issue, and the JSON verdict goes to outPath.
func Reviewer(docs []Doc, base, specPath, outPath string) string {
	return Build(reviewerRolePrompt, docs, fmt.Sprintf("The fixed point is origin/%s.\n"+
		"The spec is the issue saved at %s.\nWrite the JSON to %s.", base, specPath, outPath))
}

// Retro asks the finished session for its retro.
func Retro(repo string, number int, outPath string) string {
	return fmt.Sprintf("Use the `retro` skill on the session you just finished, which implemented issue "+
		"#%d of %s. Do not change the repository and do not ask questions. "+
		"Instead of presenting the candidates to a user, write them as Markdown, most "+
		"severe first, to %s; they will be posted as a comment on the retro ticket.", number, repo, outPath)
}
