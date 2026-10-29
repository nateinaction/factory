// Package prompt builds the prompts foreman gives the model: the agent role's fixed instruction, the
// repository's steering docs, then the issue material, in that order.
package prompt

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// DocsLimitChars caps all repository docs put into one prompt, so the prompt stays under the
// operating system's limit on a single argument.
const DocsLimitChars = 40_000

// MaxDocFiles bounds how many files one prompt reads from the repository's docs directories.
const MaxDocFiles = 100

// docDirs are the directories whose Markdown files join the repository docs, in read order.
var docDirs = []string{"docs/agents", "docs/adr"} //nolint:gochecknoglobals // a read-only order

// IssueMaterialHeading starts the variable part of a prompt. Everything before it is the prefix.
const IssueMaterialHeading = "\n# Issue material\n\n"

// ThreadHeading starts the comments a woken run reads.
const ThreadHeading = "\n\n# Comments since the marker"

// CIFailureHeading starts the CI failure report an Implementer run was sent back for.
const CIFailureHeading = "\n\n# CI failure report"

// Doc is one repository document put into a prompt.
type Doc struct {
	// Path is relative to the repository root.
	Path string
	// Text is the file's content, cut to fit DocsLimitChars.
	Text string
}

// ReadRepoDocs reads the repository's steering documents in a fixed order: AGENTS.md, GLOSSARY.md,
// then every Markdown file under docs/agents and docs/adr by path. A missing document is skipped; the
// total is capped at DocsLimitChars characters and MaxDocFiles files. The clone is untrusted, so
// every read goes through an os.Root: a document that is a symlink out of the repository fails the
// read instead of putting a host file into the prompt.
func ReadRepoDocs(dir string) ([]Doc, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open repository: %w", err)
	}
	defer func() {
		// Read-only handle: a failed close loses nothing.
		_ = root.Close()
	}()

	extra, err := docFiles(root)
	if err != nil {
		return nil, err
	}

	paths := append([]string{"AGENTS.md", "GLOSSARY.md"}, extra...)
	docs := make([]Doc, 0, len(paths))
	budget := DocsLimitChars

	for _, rel := range paths {
		raw, err := root.ReadFile(rel)
		if errors.Is(err, fs.ErrNotExist) {
			continue // a repository need not have every document; setup checks are a separate rule
		}

		if err != nil {
			return nil, fmt.Errorf("read %s: %w", rel, err)
		}

		text := workflow.Truncate(string(raw), budget)
		budget -= utf8.RuneCountInString(text)
		docs = append(docs, Doc{Path: rel, Text: text})
	}

	return docs, nil
}

// docFiles lists the Markdown files under the repository's doc directories, sorted, at most
// MaxDocFiles. A missing directory holds no documents.
func docFiles(root *os.Root) ([]string, error) {
	var out []string

	for _, dir := range docDirs {
		err := fs.WalkDir(root.FS(), dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}

				return err
			}

			if entry.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}

			if len(out) >= MaxDocFiles {
				return fs.SkipAll
			}

			out = append(out, path)

			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", dir, err)
		}
	}

	return out, nil
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
const implementerRolePrompt = "Use the `implement` skill on the issue given under \"Issue material\". " +
	"The repository is the current directory, already on its own branch. Leave your changes " +
	"uncommitted: do not commit, push, comment or ask questions, because foreman makes every write " +
	"from your output file. Write the result as JSON with \"version\": 1, \"commit_message\", and, on " +
	"a first run, \"pr_title\" and \"pr_body\". On a review round add \"replies\": one entry per " +
	"finding, {\"finding\": the finding's location or its claim, \"status\": \"fixed\" or " +
	"\"contested\", \"reason\": \"...\"}."

// plannerRolePrompt is the Planner's fixed instruction. The model never chooses a label, never
// commits, and writes only its output file; foreman makes every tracker write.
const plannerRolePrompt = "Use the `grilling`, `wayfinder`, `to-spec` and `to-tickets` skills on the " +
	"issue given under \"Issue material\". The repository is a read-only clone of the default branch: " +
	"do not change it, do not commit, do not comment " +
	"and do not ask questions in the session: foreman posts the round and moves the issue. Write one " +
	"JSON object to the output file with \"version\": 1, \"kind\" and \"comment\". " +
	"A \"kind\": \"questions\" round is numbered questions, each with a recommended answer worded so " +
	"that \"yes\" accepts it; it carries no proposal. A \"kind\": \"final_round\" carries a " +
	"\"proposal\" and its comment ends with exactly \"Is this ready to build?\" or, for a decision " +
	"ticket's resolution, \"Is this resolved?\". " +
	"A proposal is {\"shape\": \"single\"} or {\"shape\": \"slices\"}, with \"spec\" and, for slices, " +
	"\"issues\": [{\"key\": \"slug\", \"title\": \"...\", \"body\": \"...\", \"blocked_by\": [\"slug\" or \"#N\"]}]. " +
	"A {\"shape\": \"map\"} charts a wayfinder map: its \"map\" carries \"destination\", \"notes\", " +
	"\"not_yet_specified\" and \"out_of_scope\", and its \"issues\" are the tickets, each with a " +
	"\"type\": \"research\", \"grilling\", \"prototype\" or \"task\". " +
	"A {\"shape\": \"resolution\"} resolves a decision ticket: its \"resolution\" carries \"answer\" " +
	"and \"decision_line\", and its \"issues\" are any new tickets under the same map. " +
	"A prototype ticket is left for a human: propose one, but never build a prototype in this run. " +
	"The spec must carry a \"Domain changes\" section listing proposed glossary terms and ADRs. " +
	"Do not set labels or write factory markers: foreman derives every label."

// Planner is the Planner's prompt: the issue, its whole thread oldest first, and where the JSON
// goes. The Planner reads the thread rather than a clone.
func Planner(docs []Doc, repo string, issue workflow.Issue, thread []workflow.Item, outPath string) string {
	return Build(plannerRolePrompt, docs, fmt.Sprintf("Repository: %s\nIssue: #%d\nTitle: %s\n\n%s%s"+
		"\n\nWrite the JSON to %s.", repo, issue.Number, issue.Title,
		workflow.Truncate(issue.Body, workflow.BodyLimitChars), PlannerThread(thread), outPath))
}

// PlannerThread renders the issue's whole thread, oldest first, under its own heading.
func PlannerThread(comments []workflow.Item) string {
	return thread("\n\n# Issue thread", comments)
}

// reviewerRolePrompt is the Reviewer's fixed instruction.
const reviewerRolePrompt = "Use the `code-review` skill on the current directory. Do not change the " +
	"repository, do not post anything, and do not ask questions. Classify every finding as high " +
	"(a bug, a spec miss or a security problem that must be fixed before merge), medium (a " +
	"documented-standard breach or a notable defect) or low (a judgement call or nit). Write the " +
	`result as JSON with "version": 1, "verdict": "APPROVE" or "REQUEST_CHANGES", "summary", and ` +
	`"findings": [{"severity": "high|medium|low", "path": "file", "line": 3, "claim": "..."}]. ` +
	"A high or medium finding requires REQUEST_CHANGES, and REQUEST_CHANGES needs at least one finding."

// curatorRolePrompt is the Curator's fixed instruction.
const curatorRolePrompt = "Use the `curate` skill on the retro dashboard given under \"Issue material\". " +
	"The repository is a read-only clone of the default branch: do not change it, do not commit, push " +
	"or comment, and do not ask questions, because foreman makes every write from your output file. " +
	"Write the result as JSON with \"version\": 1, \"merges\", \"links\" and \"issues\". " +
	"A merge is {\"id\": \"<stable key>\", \"groups\": [\"<target>/<key>\", \"<target>/<key>\"], " +
	"\"reason\": \"<one line>\"}; its two groups share one target and the first is the one kept. " +
	"A link has the same shape and must cross targets. " +
	"An issue is one draft per ready group: {\"group\": \"<target>/<key>\", \"title\": \"...\", " +
	"\"body\": \"...\"}, or, in a private repository, {\"group\": ..., \"title\": ..., " +
	"\"lesson\": \"...\", \"private_evidence\": \"...\"}. " +
	"Keep a private draft's lesson general and its evidence apart from it. " +
	"Never split, rename or reopen a group, never touch a group a writer merged, and never propose a " +
	"pair the dashboard state lists as rejected. Give every proposal a stable key so applying the same " +
	"file twice changes nothing."

// researcherRolePrompt is the Researcher's fixed instruction.
const researcherRolePrompt = "Use the `research` skill on the ticket given under \"Issue material\". " +
	"Write the findings as Markdown files under `docs/research/` in the current directory; that " +
	"directory is the only part of the clone you may change. Do not commit, push, comment, close or " +
	"ask questions, because foreman makes every write from your output file. Write the result as JSON " +
	"with \"version\": 1, \"branch\": the <name> in research/<name> (one branch-safe segment), " +
	"\"resolution\": the comment to post (a link to the findings and a short answer), and " +
	"\"decision_line\": the map's one-line gist of the decision."

// Implementer is the Implementer's prompt; feedback is the latest review, empty on a first run; thread
// is every comment since the marker when a writer's comment woke the issue; ci is the CI failure
// report the run was sent back for, empty when CI did not fail; and outPath is where the model writes
// implementer-output.json.
func Implementer(
	docs []Doc, repo string, issue workflow.Issue, feedback string, thread []workflow.Item, ci, outPath string,
) string {
	rework := ""
	if feedback != "" {
		rework = "\n\nThe open pull request for this branch was reviewed and the review found " +
			"problems. Fix every high and medium finding below on top of the existing " +
			"commits and answer each one in \"replies\".\n\nReview:\n" +
			workflow.Truncate(feedback, workflow.BodyLimitChars)
	}

	if ci != "" {
		rework += CIFailureHeading + "\n\nThe pull request's CI failed. Fix every failed check " +
			"below on top of the existing commits.\n\n" + workflow.Truncate(ci, workflow.BodyLimitChars)
	}

	return Build(implementerRolePrompt, docs, fmt.Sprintf("Repository: %s\nIssue: #%d\nTitle: %s\n\n%s%s%s"+
		"\n\nWrite the JSON to %s.", repo, issue.Number, issue.Title,
		workflow.Truncate(issue.Body, workflow.BodyLimitChars), Thread(thread), rework, outPath))
}

// Thread renders every comment a woken run reads, oldest first, each with its author and cut to fit
// a single body limit across the whole section.
func Thread(comments []workflow.Item) string {
	return thread(ThreadHeading, comments)
}

// thread renders comments oldest first under heading, cut to one body limit across the section.
func thread(heading string, comments []workflow.Item) string {
	if len(comments) == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteString(heading)

	budget := workflow.BodyLimitChars

	for _, c := range comments {
		author := c.Author
		if author == "" {
			author = "unknown"
		}

		body := workflow.Truncate(c.Body, budget)
		budget -= utf8.RuneCountInString(body)
		fmt.Fprintf(&b, "\n\n**%s:** %s", author, body)

		if budget <= 0 {
			break
		}
	}

	return b.String()
}

// Reviewer is the Reviewer's prompt: base is the branch to compare with, specPath holds the
// issue, and the JSON verdict goes to outPath.
func Reviewer(docs []Doc, base, specPath, outPath string) string {
	return Build(reviewerRolePrompt, docs, fmt.Sprintf("The fixed point is origin/%s.\n"+
		"The spec is the issue saved at %s.\nWrite the JSON to %s.", base, specPath, outPath))
}

// Researcher is the Researcher's prompt; thread is every comment since the marker when a writer's
// comment woke the ticket, and outPath is where the model writes researcher-output.json.
func Researcher(
	docs []Doc, repo string, issue workflow.Issue, thread []workflow.Item, outPath string,
) string {
	return Build(researcherRolePrompt, docs, fmt.Sprintf("Repository: %s\nIssue: #%d\nTitle: %s\n\n%s%s"+
		"\n\nWrite the JSON to %s.", repo, issue.Number, issue.Title,
		workflow.Truncate(issue.Body, workflow.BodyLimitChars), Thread(thread), outPath))
}

// Curator is the Curator's prompt: the retro log with evidence and the dashboard state under the
// repository docs, and the output file's path.
func Curator(
	docs []Doc, repo string, number int, private bool, reports []workflow.DashboardReport,
	view workflow.DashboardView, outPath string,
) string {
	return Build(curatorRolePrompt, docs, fmt.Sprintf("%s\n\nWrite the JSON to %s.",
		workflow.CuratorBrief(repo, number, private, reports, view), outPath))
}

// retroJSONShape tells the model the exact file foreman validates.
const retroJSONShape = `write retro.json to %s: {"version":1,"findings":[{"target":"skill:<name>" or ` +
	`"docs:<path>" or "factory:<area>","group":"<key>","severity":"high|medium|low",` +
	`"claim":"<one line>","evidence":"<what the session showed>"}]}`

// retroReviewerNote is the Reviewer's extra instruction: its retro may judge the Implementer's skills.
const retroReviewerNote = " You are the Reviewer; findings may target the Implementer's skills, " +
	"saying what should have been caught before review."

// RetroRequest is what the retro prompt needs.
type RetroRequest struct {
	// Role is the agent role the retro reports for.
	Role workflow.Role
	// Repo is owner/name.
	Repo string
	// Issue is the issue the run worked.
	Issue int
	// OutPath is where the model writes retro.json.
	OutPath string
	// Groups are the existing group keys and one-line claims for each target.
	Groups []workflow.RetroGroup
}

// Retro asks the finished session for its retro, showing only group keys and claims, never another
// retro's evidence.
func Retro(r RetroRequest) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Use the `retro` skill on the session you just finished, which worked issue #%d of %s. "+
		"Do not change the repository and do not ask questions. Instead of presenting the candidates to a user, ",
		r.Issue, r.Repo)
	fmt.Fprintf(&b, retroJSONShape, r.OutPath)
	b.WriteString(" Each target is a skill the factory ships, a document under the repository's AGENTS.md or " +
		"docs/agents/, or an area of the factory. Pick a group key from the existing groups for that target " +
		"below, or a new one; the wording of a finding never decides its group.")

	if r.Role == workflow.Reviewer {
		b.WriteString(retroReviewerNote)
	}

	b.WriteString("\n\n# Existing groups\n")

	if len(r.Groups) == 0 {
		b.WriteString("\nNo existing groups.\n")

		return b.String()
	}

	last := workflow.RetroTarget{}
	for _, g := range r.Groups {
		if g.Target != last {
			fmt.Fprintf(&b, "\n## %s\n\n", g.Target)
			last = g.Target
		}

		fmt.Fprintf(&b, "- `%s`: %s\n", g.Key, g.Claim)
	}

	return b.String()
}
