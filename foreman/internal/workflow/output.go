package workflow

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// OutputFileVersion is the only version an output file may carry.
const OutputFileVersion = 1

// The output file each agent role writes into its run's scratch directory.
const (
	// ImplementerOutputFile holds the Implementer's commit, pull request and replies.
	ImplementerOutputFile = "implementer-output.json"
	// ResearcherOutputFile holds the Researcher's branch, resolution and decision line.
	ResearcherOutputFile = "researcher-output.json"
	// ReviewerOutputFile holds the Reviewer's verdict.
	ReviewerOutputFile = "reviewer-output.json"
)

// OutputProblem is the class of problem an output file was rejected for, named for the security event
// the rejection emits.
type OutputProblem string

// The classes of problem an output file can carry, and the security outcome each is recorded under.
// There is one value set: the security event uses the problem's own value, so the two cannot drift.
// A file with a marker is refused for that, whatever else is wrong with it: forging a transition is
// the security-relevant part.
const (
	// OutputUnreadable is a file foreman could not read at all.
	OutputUnreadable OutputProblem = "unreadable_output"
	// OutputFactoryMarker is a file that carries a factory marker.
	OutputFactoryMarker OutputProblem = "factory_marker"
	// OutputInvalid is a file that is readable but does not fit its shape.
	OutputInvalid OutputProblem = "invalid_output"
)

// OutputError is an output file foreman rejected. Nothing in the file is applied, and the issue is
// failed with every problem listed in one comment.
type OutputError struct {
	// File is the output file's name.
	File string
	// Problems is every reason the file was rejected.
	Problems []string
	// Problem is the class of problem the file carried.
	Problem OutputProblem
}

// Error names the file and lists every problem.
func (e *OutputError) Error() string {
	return e.File + " rejected: " + strings.Join(e.Problems, "; ")
}

// ReplyStatus is what the Implementer did with one review finding.
type ReplyStatus string

// The two answers to a review finding.
const (
	// ReplyFixed says the finding was fixed.
	ReplyFixed ReplyStatus = "fixed"
	// ReplyContested says the finding is not a defect, with a reason.
	ReplyContested ReplyStatus = "contested"
)

// Reply answers one review finding.
type Reply struct {
	// Finding names the finding being answered.
	Finding string `json:"finding"`
	// Status is fixed or contested.
	Status ReplyStatus `json:"status"`
	// Reason says what was done, or why the finding is contested.
	Reason string `json:"reason"`
}

// ImplementerOutput is implementer-output.json.
type ImplementerOutput struct {
	// Version is OutputFileVersion.
	Version int `json:"version"`
	// CommitMessage is the message foreman commits the working tree with.
	CommitMessage string `json:"commit_message"`
	// PRTitle is the pull request's title; it is required on a first run only.
	PRTitle string `json:"pr_title"`
	// PRBody is the pull request's body; it is required on a first run only.
	PRBody string `json:"pr_body"`
	// Replies answer the latest review's findings on a review round.
	Replies []Reply `json:"replies"`
}

// Verdict is the Reviewer's judgement on a pull request.
type Verdict string

// The verdicts, each also the event of the native review it posts.
const (
	// Approve accepts the pull request and asks a human to merge it.
	Approve Verdict = "APPROVE"
	// RequestChanges sends the pull request back to the Implementer.
	RequestChanges Verdict = "REQUEST_CHANGES"
)

// Event is the GitHub review event that carries the verdict.
func (v Verdict) Event() string {
	return string(v)
}

// ReviewFinding is one finding of a review.
type ReviewFinding struct {
	// Severity is how much the finding matters.
	Severity Severity `json:"severity"`
	// Path is the file the finding is about.
	Path string `json:"path"`
	// Line is the line within Path, or zero when the finding is not about one line.
	Line int `json:"line"`
	// Claim says what is wrong.
	Claim string `json:"claim"`
}

// Location is the finding's place, as "path:line" or just "path".
func (f ReviewFinding) Location() string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d", f.Path, f.Line)
	}

	return f.Path
}

// ReviewerOutput is reviewer-output.json.
type ReviewerOutput struct {
	// Version is OutputFileVersion.
	Version int `json:"version"`
	// Verdict is APPROVE or REQUEST_CHANGES.
	Verdict Verdict `json:"verdict"`
	// Summary is the review's prose, the two axes included.
	Summary string `json:"summary"`
	// Findings are what the review found.
	Findings []ReviewFinding `json:"findings"`
}

// ParseImplementerOutput reads implementer-output.json strictly. rework says the run had an open pull
// request, so it answers a review and needs a reply per finding; review is the latest rendered review
// it is answering, empty when there is none. A first run needs the pull request text instead.
func ParseImplementerOutput(text string, rework bool, review string) (ImplementerOutput, error) {
	return parseOutput(text, ImplementerOutputFile, func(out ImplementerOutput) (ImplementerOutput, []string) {
		return out, implementerProblems(out, rework, review)
	})
}

// implementerProblems checks the fields an Implementer output carries beyond the strict decode.
func implementerProblems(out ImplementerOutput, rework bool, review string) []string {
	var problems []string

	if out.CommitMessage == "" {
		problems = append(problems, "commit_message is required")
	}

	switch {
	case rework && len(out.Replies) == 0:
		problems = append(problems, "a review round needs a reply to each finding")
	case !rework && len(out.Replies) > 0:
		problems = append(problems, "replies are only for a review round")
	case !rework && out.PRTitle == "":
		problems = append(problems, "pr_title is required on a first run")
	case !rework && out.PRBody == "":
		problems = append(problems, "pr_body is required on a first run")
	}

	return append(problems, replyProblems(out.Replies, ParseReviewFindings(review))...)
}

// ParseReviewFindings reads the findings RenderReview wrote, in the order it grouped them. A body
// that is not foreman's rendered review has none.
func ParseReviewFindings(body string) []ReviewFinding {
	var (
		findings []ReviewFinding
		severity Severity
	)

	for line := range strings.SplitSeq(body, "\n") {
		if title, ok := strings.CutPrefix(line, "### "); ok {
			severity = severityOf(title)

			continue
		}

		rest, ok := strings.CutPrefix(line, "- **`")
		if !ok || !slices.Contains(Severities(), severity) {
			continue
		}

		location, claim, ok := strings.Cut(rest, "`**: ")
		if !ok {
			continue
		}

		findings = append(findings, ReviewFinding{
			Severity: severity, Path: pathOf(location), Line: lineOf(location), Claim: claim,
		})
	}

	return findings
}

// severityOf reads a RenderReview heading like "High (2)".
func severityOf(heading string) Severity {
	name, _, _ := strings.Cut(heading, " (")
	name = strings.ToLower(name)

	for _, s := range Severities() {
		if string(s) == name {
			return s
		}
	}

	return ""
}

// pathOf reads the path out of a finding location like "a.go:3".
func pathOf(location string) string {
	path, _, ok := strings.Cut(location, ":")
	if !ok {
		return location
	}

	return path
}

// lineOf reads the line out of a finding location like "a.go:3", or zero when it names none.
func lineOf(location string) int {
	_, text, ok := strings.Cut(location, ":")
	if !ok {
		return 0
	}

	line, err := strconv.Atoi(text)
	if err != nil {
		return 0
	}

	return line
}

// replyProblems checks every reply names a finding and says fixed or contested, with a reason, and
// that every finding of the review being answered has one.
func replyProblems(replies []Reply, findings []ReviewFinding) []string {
	var problems []string

	for i, r := range replies {
		at := fmt.Sprintf("replies[%d]", i)
		if r.Finding == "" {
			problems = append(problems, at+".finding is required")
		}

		if r.Status != ReplyFixed && r.Status != ReplyContested {
			problems = append(problems, at+".status must be fixed or contested")
		}

		if r.Reason == "" {
			problems = append(problems, at+".reason is required")
		}
	}

	return append(problems, coverageProblems(replies, findings)...)
}

// coverageProblems names each finding of the review that no reply answers. A reply names its finding
// by the location or the claim the rendered review shows.
func coverageProblems(replies []Reply, findings []ReviewFinding) []string {
	var problems []string

	for _, finding := range findings {
		if !slices.ContainsFunc(replies, func(r Reply) bool { return answers(r.Finding, finding) }) {
			problems = append(problems, fmt.Sprintf("replies needs a reply to the %s finding %s: %s",
				finding.Severity, finding.Location(), finding.Claim))
		}
	}

	return problems
}

// answers reports whether reply names finding, by its location or its claim, ignoring case and
// surrounding space.
func answers(reply string, finding ReviewFinding) bool {
	named := strings.TrimSpace(strings.ToLower(reply))

	return named == strings.ToLower(finding.Location()) || named == strings.ToLower(finding.Claim)
}

// ParseReviewerOutput reads reviewer-output.json strictly.
func ParseReviewerOutput(text string) (ReviewerOutput, error) {
	return parseOutput(text, ReviewerOutputFile, func(out ReviewerOutput) (ReviewerOutput, []string) {
		return out, reviewerProblems(out)
	})
}

// reviewerProblems checks the verdict against the findings, and every field of every finding.
func reviewerProblems(out ReviewerOutput) []string {
	return append(verdictProblems(out), findingProblems(out.Findings)...)
}

// verdictProblems checks the verdict, its summary and what it implies about the findings.
func verdictProblems(out ReviewerOutput) []string {
	var problems []string

	switch out.Verdict {
	case Approve:
		if len(Blocking(out.Findings)) > 0 {
			problems = append(problems, "a high or medium finding needs REQUEST_CHANGES")
		}
	case RequestChanges:
		if len(out.Findings) == 0 {
			problems = append(problems, "REQUEST_CHANGES needs at least one finding")
		}
	default:
		problems = append(problems, "verdict must be APPROVE or REQUEST_CHANGES")
	}

	if out.Summary == "" {
		problems = append(problems, "summary is required")
	}

	return problems
}

// findingProblems checks every finding names what is wrong, where, and how much it matters.
func findingProblems(findings []ReviewFinding) []string {
	var problems []string

	for i, f := range findings {
		at := fmt.Sprintf("findings[%d]", i)
		if !slices.Contains(Severities(), f.Severity) {
			problems = append(problems, at+".severity must be high, medium or low")
		}

		if f.Path == "" {
			problems = append(problems, at+".path is required")
		}

		if f.Line < 0 {
			problems = append(problems, at+".line must not be negative")
		}

		if f.Claim == "" {
			problems = append(problems, at+".claim is required")
		}
	}

	return problems
}

// RenderReview is the native review body posted against sha.
func RenderReview(sha string, out ReviewerOutput) string {
	lines := []string{ReviewMarker(sha), "## Agent review", "", out.Summary}

	for _, sev := range Severities() {
		var group []string

		for _, f := range out.Findings {
			if f.Severity != sev {
				continue
			}

			group = append(group, fmt.Sprintf("- **`%s`**: %s", f.Location(), f.Claim))
		}

		if len(group) > 0 {
			lines = append(lines, "", fmt.Sprintf("### %s (%d)", sev.title(), len(group)))
			lines = append(lines, group...)
		}
	}

	if len(out.Findings) == 0 {
		lines = append(lines, "", "No findings.")
	}

	return Truncate(strings.Join(lines, "\n"), CommentLimitChars)
}

// RepliesMarker keys the replies to the commit they answer, so a repeat updates them.
func RepliesMarker(sha string) string {
	return MarkerPrefix + "replies sha=" + sha + " -->"
}

// RenderReplies is the comment that answers review findings, keyed to sha.
func RenderReplies(sha string, replies []Reply) string {
	lines := make([]string, 0, len(replies))
	lines = append(lines, RepliesMarker(sha), "## Replies to review findings", "")

	for _, r := range replies {
		lines = append(lines, fmt.Sprintf("- **%s** `%s`: %s", r.Status, r.Finding, r.Reason))
	}

	return Truncate(strings.Join(lines, "\n"), CommentLimitChars)
}

// validText is the one check for model-written or recorded free text: it rejects text that is
// empty or carries a factory marker, and, for a one-line field, text that spans lines. The retro,
// the Curator and the dashboard state all use it.
func validText(field, text string, oneLine bool) error {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return fmt.Errorf("%s is empty", field)
	}

	if strings.Contains(text, MarkerPrefix) {
		return fmt.Errorf("%s carries a factory marker", field)
	}

	if oneLine && strings.ContainsAny(trimmed, "\r\n") {
		return fmt.Errorf("%s must be one line", field)
	}

	return nil
}

// parseOutput is the one parser behind every agent role's output file. It decodes the file strictly,
// with the checks they all share, then lets check read the decoded file for what only that file has
// to satisfy. check returns the value and every problem it found; the two lists are reported
// together, so a rejected file names all of its problems at once. Nothing built from a rejected
// file is returned.
func parseOutput[D, O any](text, file string, check func(D) (O, []string)) (O, error) {
	var decoded D

	problems, forged := decodeStrict(text, &decoded)
	out, more := check(decoded)
	problems = append(problems, more...)

	if len(problems) > 0 {
		var none O

		return none, &OutputError{File: file, Problems: problems, Problem: problemClass(forged)}
	}

	return out, nil
}

// problemClass is the class of problem a rejected file carried. A file with a marker is refused for
// that, whatever else is wrong with it.
func problemClass(forged bool) OutputProblem {
	if forged {
		return OutputFactoryMarker
	}

	return OutputInvalid
}

// decodeStrict checks what every output file shares, then decodes it into out with unknown members
// refused. It returns every problem, so a rejected file names all of them at once.
//
// encoding/json/v2 refuses a repeated member and trailing data itself. It stops at the first error
// it finds, where the walker this replaced named every repeated key at once: rejecting the file is
// what matters, and naming one place to start is enough to fix it.
func decodeStrict(text string, out any) ([]string, bool) {
	problems, forged, object := sharedProblems(text)
	if !object {
		return problems, forged
	}

	if err := json.Unmarshal([]byte(text), out, json.RejectUnknownMembers(true)); err != nil {
		return append(problems, decodeProblem(err)), forged
	}

	return problems, forged
}

// sharedProblems checks the version and model text of every output file. forged is true when the text
// carries a factory marker; object is false when the text is empty, not JSON, or not a JSON object, so
// a typed decode would only repeat the failure.
func sharedProblems(text string) (problems []string, forged, object bool) {
	if strings.TrimSpace(text) == "" {
		return []string{"is empty"}, false, false
	}

	var raw any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return []string{syntaxProblem(err)}, false, false
	}

	obj, ok := raw.(map[string]any)
	if !ok {
		return []string{"must be a JSON object"}, false, false
	}

	if version, ok := obj["version"].(float64); !ok || version != OutputFileVersion {
		problems = append(problems, fmt.Sprintf("version must be %d", OutputFileVersion))
	}

	markers := markerProblems(raw, "")

	return append(problems, markers...), len(markers) > 0, true
}

// syntaxProblem names why the text is not one JSON object foreman can read. A repeated member is
// the model's mistake, so it is named by its JSON pointer.
func syntaxProblem(err error) string {
	if errors.Is(err, jsontext.ErrDuplicateName) {
		return "repeats the key at " + string(jsonPointer(err))
	}

	return "is not valid JSON: " + err.Error()
}

// decodeProblem names why a JSON object does not fit the output's shape. encoding/json/v2 names a
// shape mismatch after the Go type it was decoding, which is foreman's business, not the model's.
func decodeProblem(err error) string {
	if errors.Is(err, json.ErrUnknownName) {
		return fmt.Sprintf("unknown field %q", jsonPointer(err).LastToken())
	}

	return err.Error()
}

// jsonPointer is the place in the JSON a decode error names, empty when it names none.
func jsonPointer(err error) jsontext.Pointer {
	if semantic, ok := errors.AsType[*json.SemanticError](err); ok {
		return semantic.JSONPointer
	}

	if syntax, ok := errors.AsType[*jsontext.SyntacticError](err); ok {
		return syntax.JSONPointer
	}

	return ""
}

// markerProblems names every place in a decoded file whose text carries a factory marker. foreman's
// markers are its own: a model that writes one could forge a transition or a review.
func markerProblems(node any, path string) []string {
	switch value := node.(type) {
	case map[string]any:
		problems := make([]string, 0, len(value))

		for _, key := range slices.Sorted(maps.Keys(value)) {
			problems = append(problems, markerProblems(value[key], fieldPath(path, key))...)
		}

		return problems
	case []any:
		problems := make([]string, 0, len(value))

		for i, item := range value {
			problems = append(problems, markerProblems(item, fmt.Sprintf("%s[%d]", path, i))...)
		}

		return problems
	case string:
		if strings.Contains(value, MarkerPrefix) {
			return []string{fieldPath(path, "") + " carries a factory marker"}
		}
	}

	return nil
}

// fieldPath joins a parent path and a field, or names just the parent when the field is empty.
func fieldPath(parent, field string) string {
	switch {
	case parent == "":
		return field
	case field == "":
		return parent
	}

	return parent + "." + field
}
