package workflow

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// PlannerOutputFile is the file the Planner writes into its run's scratch directory.
const PlannerOutputFile = "planner-output.json"

// PlannerKind is what a Planner run produced: a round of questions or a final round with a proposal.
type PlannerKind string

// The two kinds of Planner output.
const (
	// PlannerQuestions is a round of numbered questions awaiting a writer.
	PlannerQuestions PlannerKind = "questions"
	// PlannerFinalRound is the round holding a proposal, which only a writer may approve.
	PlannerFinalRound PlannerKind = "final_round"
)

// ProposalShape is the kind of thing an approved proposal creates.
type ProposalShape string

// The proposal shapes.
const (
	// ShapeSingle hands the issue itself to the Implementer.
	ShapeSingle ProposalShape = "single"
	// ShapeSlices creates one sub-issue per slice.
	ShapeSlices ProposalShape = "slices"
	// ShapeMap rewrites the issue as a wayfinder map.
	ShapeMap ProposalShape = "map"
	// ShapeResolution records a decision-ticket resolution.
	ShapeResolution ProposalShape = "resolution"
)

// ProposalShapes is the closed set of shapes.
func ProposalShapes() []ProposalShape {
	return []ProposalShape{ShapeSingle, ShapeSlices, ShapeMap, ShapeResolution}
}

// TicketType is a wayfinder ticket's type, which derives its queue.
type TicketType string

// The wayfinder ticket types.
const (
	// TicketResearch is a research ticket.
	TicketResearch TicketType = "research"
	// TicketPrototype is a prototype ticket, left for a human.
	TicketPrototype TicketType = "prototype"
	// TicketGrilling is a grilling ticket.
	TicketGrilling TicketType = "grilling"
	// TicketTask is a task ticket.
	TicketTask TicketType = "task"
)

// TicketTypes is the closed set of ticket types.
func TicketTypes() []TicketType {
	return []TicketType{TicketResearch, TicketPrototype, TicketGrilling, TicketTask}
}

// TicketLabels returns the labels a ticket of type t carries: the wayfinder label that names its type
// and the factory label that queues it. foreman derives both; a proposal never picks a label.
func TicketLabels(t TicketType) ([]string, error) {
	switch t {
	case TicketResearch:
		return []string{LabelResearchTicket, LabelResearch}, nil
	case TicketGrilling:
		return []string{LabelGrilling, LabelPlan}, nil
	case TicketPrototype:
		return []string{LabelWayfinderPrototype, LabelPlan}, nil
	case TicketTask:
		return []string{LabelTask, LabelPlan}, nil
	}

	return nil, fmt.Errorf("unknown ticket type %q", t)
}

// PlannedIssue is one issue a proposal creates, with the key that makes a repeat finding it again.
type PlannedIssue struct {
	// Key is the slug the issue's idempotency marker carries.
	Key string `json:"key"`
	// Title is the issue's title.
	Title string `json:"title"`
	// Body is the issue's body, without the marker foreman adds.
	Body string `json:"body"`
	// Type is a wayfinder ticket's type. Only map and resolution proposals may set it.
	Type TicketType `json:"type,omitempty"`
	// BlockedBy names the issues that must close first: another key, or #N for an existing issue.
	BlockedBy []string `json:"blocked_by,omitempty"`
}

// PlannerMap is a proposed wayfinder map's body, excluding the writer's original request.
type PlannerMap struct {
	// Destination is the map's destination.
	Destination string `json:"destination"`
	// Notes are the map's notes.
	Notes string `json:"notes,omitempty"`
	// NotYetSpecified is what remains foggy.
	NotYetSpecified string `json:"not_yet_specified,omitempty"`
	// OutOfScope is what the map rules out.
	OutOfScope string `json:"out_of_scope,omitempty"`
}

// PlannerResolution is a decision ticket's proposed resolution.
type PlannerResolution struct {
	// Answer is the resolution comment.
	Answer string `json:"answer"`
	// DecisionLine is the one-line gist appended to the map's Decisions.
	DecisionLine string `json:"decision_line"`
}

// RenderMap lays out an approved map's body in the wayfinder layout, keeping the writer's original
// request under its own heading. Decisions so far starts empty: resolutions append to it.
func RenderMap(original string, m PlannerMap) string {
	var b strings.Builder

	writeMapSection(&b, "Destination", m.Destination)
	writeMapSection(&b, "Notes", m.Notes)
	writeMapSection(&b, "Decisions so far", "")
	writeMapSection(&b, "Not yet specified", m.NotYetSpecified)
	writeMapSection(&b, "Out of scope", m.OutOfScope)
	writeMapSection(&b, "Original request", original)

	return strings.TrimRight(b.String(), "\n")
}

// writeMapSection writes one map section, leaving an empty one as just its heading.
func writeMapSection(b *strings.Builder, heading, text string) {
	b.WriteString("## " + heading + "\n\n")

	if trimmed := strings.TrimSpace(text); trimmed != "" {
		b.WriteString(trimmed + "\n\n")
	}
}

// PlannerProposal is what a final round asks a writer to approve. A shape fills the fields it uses:
// spec (single, slices, map), issues (slices, map, resolution), map, and resolution.
type PlannerProposal struct {
	// Shape is what approving the proposal does.
	Shape ProposalShape `json:"shape"`
	// Spec is the spec the Implementer reads from the final-round comment.
	Spec string `json:"spec,omitempty"`
	// Issues are the issues the shape creates.
	Issues []PlannedIssue `json:"issues,omitempty"`
	// Map is a map proposal's body.
	Map *PlannerMap `json:"map,omitempty"`
	// Resolution is a decision ticket's proposed outcome.
	Resolution *PlannerResolution `json:"resolution,omitempty"`
}

// PlannerOutput is planner-output.json.
type PlannerOutput struct {
	// Version is OutputFileVersion.
	Version int `json:"version"`
	// Kind is questions or final_round.
	Kind PlannerKind `json:"kind"`
	// Comment is the round foreman posts.
	Comment string `json:"comment"`
	// Proposal is present on a final round only.
	Proposal *PlannerProposal `json:"proposal,omitempty"`
}

// plannedKeyRE matches the slug a planned issue's marker carries.
var plannedKeyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// plannedMarkerRE matches the marker foreman writes into every issue it plans.
var plannedMarkerRE = regexp.MustCompile(`<!-- factory:planned from=#([0-9]{1,9}) key=([a-z0-9][a-z0-9-]{0,39}) -->`)

// ParsePlannerOutput reads planner-output.json strictly.
func ParsePlannerOutput(text string) (PlannerOutput, error) {
	var out PlannerOutput

	problems := decodeStrict(text, &out)
	problems = append(problems, plannerProblems(out)...)

	if len(problems) > 0 {
		return PlannerOutput{}, &OutputError{File: PlannerOutputFile, Problems: problems}
	}

	return out, nil
}

// plannerProblems checks the round and, on a final round, the whole proposal.
func plannerProblems(out PlannerOutput) []string {
	var problems []string

	if strings.TrimSpace(out.Comment) == "" {
		problems = append(problems, "comment is required")
	}

	if out.Proposal == nil {
		if out.Kind == PlannerFinalRound {
			problems = append(problems, "a final round needs a proposal")
		}
	} else if out.Kind == PlannerQuestions {
		problems = append(problems, "a round of questions carries no proposal")
	}

	switch out.Kind {
	case PlannerQuestions:
	case PlannerFinalRound:
		if out.Proposal != nil {
			problems = append(problems, proposalProblems(*out.Proposal)...)
			problems = append(problems, gateProblems(out.Comment, out.Proposal.Shape)...)
		}
	default:
		problems = append(problems, "kind must be questions or final_round")
	}

	return problems
}

// proposalProblems checks the shape, its required fields and every planned issue.
func proposalProblems(p PlannerProposal) []string {
	if !slices.Contains(ProposalShapes(), p.Shape) {
		return []string{"proposal.shape must be single, slices, map or resolution"}
	}

	problems := proposalFields(p)

	return append(problems, plannedIssueProblems(p)...)
}

// proposalFields checks the fields each shape uses and refuses the ones it does not.
func proposalFields(p PlannerProposal) []string {
	problems := specProblems(p)

	problems = append(problems, shapeProblems(p)...)

	if p.Shape == ShapeResolution {
		problems = append(problems, resolutionProblems(p.Resolution)...)
	}

	return problems
}

// specProblems checks the spec every non-resolution shape carries.
func specProblems(p PlannerProposal) []string {
	if p.Shape == ShapeResolution {
		return nil
	}

	switch {
	case strings.TrimSpace(p.Spec) == "":
		return []string{"proposal.spec is required"}
	case !hasDomainChanges(p.Spec):
		return []string{"proposal.spec needs a Domain changes section"}
	}

	return nil
}

// shapeProblems checks the issues and extra fields a non-resolution shape needs.
func shapeProblems(p PlannerProposal) []string {
	switch p.Shape {
	case ShapeSingle:
		if len(p.Issues) > 0 {
			return []string{"a single proposal creates no issues"}
		}
	case ShapeSlices:
		if len(p.Issues) == 0 {
			return []string{"a slices proposal needs issues"}
		}
	case ShapeMap:
		return mapProblems(p)
	case ShapeResolution:
	}

	return nil
}

// mapProblems checks a map's destination and tickets.
func mapProblems(p PlannerProposal) []string {
	var problems []string

	if len(p.Issues) == 0 {
		problems = append(problems, "a map proposal needs issues")
	}

	if p.Map == nil || strings.TrimSpace(p.Map.Destination) == "" {
		problems = append(problems, "a map proposal needs map.destination")
	}

	return problems
}

// resolutionProblems checks a resolution's answer and decision line.
func resolutionProblems(r *PlannerResolution) []string {
	if r == nil {
		return []string{"a resolution proposal needs resolution"}
	}

	var problems []string

	if strings.TrimSpace(r.Answer) == "" {
		problems = append(problems, "proposal.resolution.answer is required")
	}

	if strings.TrimSpace(r.DecisionLine) == "" {
		problems = append(problems, "proposal.resolution.decision_line is required")
	}

	return problems
}

// hasDomainChanges reports whether a spec carries the Planner's Domain changes section.
func hasDomainChanges(spec string) bool {
	for line := range strings.SplitSeq(spec, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") {
			continue
		}

		if strings.EqualFold(strings.TrimSpace(strings.TrimLeft(trimmed, "#")), "Domain changes") {
			return true
		}
	}

	return false
}

// plannedIssueProblems checks every issue's key, title, body, type and blockers.
func plannedIssueProblems(p PlannerProposal) []string {
	var (
		problems []string
		keys     = map[string]bool{}
	)

	for i, issue := range p.Issues {
		at := fmt.Sprintf("proposal.issues[%d]", i)

		if !plannedKeyRE.MatchString(issue.Key) {
			problems = append(problems, at+".key must be a short lowercase slug")
		} else if keys[issue.Key] {
			problems = append(problems, at+".key "+issue.Key+" repeats")
		}

		keys[issue.Key] = true

		if strings.TrimSpace(issue.Title) == "" {
			problems = append(problems, at+".title is required")
		}

		if strings.TrimSpace(issue.Body) == "" {
			problems = append(problems, at+".body is required")
		}

		problems = append(problems, ticketTypeProblems(p.Shape, issue.Type, at)...)
	}

	for i, issue := range p.Issues {
		for _, blocker := range issue.BlockedBy {
			if keys[blocker] || plannedNumberRE.MatchString(blocker) {
				continue
			}

			problems = append(problems, fmt.Sprintf("proposal.issues[%d].blocked_by %q names no issue", i, blocker))
		}
	}

	return problems
}

// ticketTypeProblems checks a ticket type is present exactly where a shape uses it.
func ticketTypeProblems(shape ProposalShape, typ TicketType, at string) []string {
	wantsType := shape == ShapeMap || shape == ShapeResolution

	switch {
	case wantsType && !slices.Contains(TicketTypes(), typ):
		return []string{at + ".type must be research, prototype, grilling or task"}
	case !wantsType && typ != "":
		return []string{at + ".type is only for map and resolution tickets"}
	}

	return nil
}

// plannedNumberRE matches an existing issue named in blocked_by.
var plannedNumberRE = regexp.MustCompile(`^#[0-9]{1,9}$`)

// gateProblems checks the final round ends with the gate question its shape asks.
func gateProblems(comment string, shape ProposalShape) []string {
	want := "Is this ready to build?"
	if shape == ShapeResolution {
		want = "Is this resolved?"
	}

	// Allow the markdown emphasis a model wraps the gate question in.
	if !strings.HasSuffix(strings.TrimRight(strings.TrimSpace(comment), " \t\r\n*_"), want) {
		return []string{"a final round must end with " + want}
	}

	return nil
}

// proposalOpen starts the hidden proposal block in a final-round comment.
const proposalOpen = MarkerPrefix + "proposal\n"

// proposalClose ends the hidden proposal block.
const proposalClose = "\n-->"

// ProposalBlock renders the hidden block that stores p inside the final-round comment.
func ProposalBlock(p PlannerProposal) (string, error) {
	if problems := proposalProblems(p); len(problems) > 0 {
		return "", errors.New("proposal is invalid: " + strings.Join(problems, "; "))
	}

	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode proposal: %w", err)
	}

	return proposalOpen + string(raw) + proposalClose, nil
}

// StoredProposal reads the hidden proposal block out of a final-round comment. found is false when
// the comment carries none; a block that does not parse strictly is an error.
func StoredProposal(body string) (PlannerProposal, bool, error) {
	_, rest, ok := strings.Cut(body, proposalOpen)
	if !ok {
		return PlannerProposal{}, false, nil
	}

	text, _, ok := strings.Cut(rest, proposalClose)
	if !ok {
		return PlannerProposal{}, true, errors.New("stored proposal is not closed")
	}

	var p PlannerProposal

	if err := jsonv2.Unmarshal([]byte(text), &p, jsonv2.RejectUnknownMembers(true)); err != nil {
		return PlannerProposal{}, true, fmt.Errorf("decode stored proposal: %w", err)
	}

	if problems := proposalProblems(p); len(problems) > 0 {
		return PlannerProposal{}, true, errors.New("stored proposal is invalid: " + strings.Join(problems, "; "))
	}

	return p, true, nil
}

// Approval words that recur in more than one rule.
const (
	wordThis  = "this"
	wordBuild = "build"
)

// StoredSpec returns the spec of the latest stored proposal, when that proposal is a single. The
// latest stored proposal wins: another shape keeps its spec where that shape puts it, not in the
// final-round comment.
func StoredSpec(comments []Item, owner string) (string, bool, error) {
	var (
		spec  string
		final bool
	)

	for _, c := range comments {
		if !FactoryAuthored(c.Author, owner) {
			continue
		}

		p, found, err := StoredProposal(c.Body)
		if err != nil {
			return "", false, err
		}

		if found {
			spec, final = p.Spec, p.Shape == ShapeSingle
		}
	}

	return spec, final, nil
}

// Approves reports whether text is a writer's explicit approval of a pending gate: a plain yes or a
// request to build. A negated, questioning or conditional reply is not approval.
func Approves(text string) bool {
	return approval(text, true)
}

// RequestsBuild reports whether text explicitly asks to build. It opens the gate even when the
// Planner asked no final-round question; a plain yes is not a request to build.
func RequestsBuild(text string) bool {
	return approval(text, false)
}

// approvalWords lower-cases text and splits it into words, dropping markdown emphasis and
// punctuation. A question mark is kept as a word so a question can never pass as approval.
func approvalWords(text string) []string {
	reply := strings.ToLower(text)
	reply = strings.ReplaceAll(reply, "’", "'")
	reply = strings.ReplaceAll(reply, "n't", " not")
	reply = strings.ReplaceAll(reply, "i'm", "i am")
	reply = strings.ReplaceAll(reply, "let's", "lets")

	var b strings.Builder

	for _, r := range reply {
		switch {
		case r == '?':
			b.WriteString(" ? ")
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}

	return strings.Fields(b.String())
}

// questionOpener reports whether word starts a question rather than an answer.
func questionOpener(word string) bool {
	switch word {
	case "is", "are", "am", "was", "were", "do", "does", "did", "should", "could",
		"would", "will", "can", "may", "might", "must", "what", "why", "how", "when",
		"where", "who", "which", "whom", "whose":
		return true
	default:
		return false
	}
}

// plainYesWord reports whether word is an explicit affirmative, with no clause added.
func plainYesWord(word string) bool {
	switch word {
	case "yes", "yep", "yeah", "ok", "okay", "lgtm":
		return true
	default:
		return false
	}
}

// approvalFiller reports whether word is a trailing word that does not change an approval.
func approvalFiller(word string) bool {
	switch word {
	case "please", "now", "thanks", "thank", "you":
		return true
	default:
		return false
	}
}

// approvalPrefixWord reports whether word may precede a build request. Anything else is a condition,
// a negation or an unrelated sentence, and is not approval.
func approvalPrefixWord(word string) bool {
	switch word {
	case "i", "am", "we", wordThis, "it", "is", "are", "looks", "look", "good", "great",
		"all", "fine", "yes", "yep", "yeah", "ok", "okay", "definitely", "absolutely",
		"so", "then", "lets", "the", "spec", "plan", "everything", "and", "go", "ahead",
		"lgtm", "ship", "approved", "please":
		return true
	default:
		return false
	}
}

// buildPhraseSuffix returns the length of the build phrase that ends words, if one does.
func buildPhraseSuffix(words []string) (int, bool) {
	phrases := [][]string{
		{"ready", "to", wordBuild},
		{wordBuild, "it"},
		{wordBuild, wordThis},
		{wordBuild, "that"},
		{"implement", "it"},
		{"implement", wordThis},
		{"start", "building"},
		{"begin", "building"},
		{"go", "ahead", "and", wordBuild},
		{"proceed", "to", wordBuild},
	}

	for _, phrase := range phrases {
		if len(words) >= len(phrase) && slices.Equal(words[len(words)-len(phrase):], phrase) {
			return len(phrase), true
		}
	}

	return 0, false
}

// approval reports whether text approves, counting a plain yes only when yesCounts.
func approval(text string, yesCounts bool) bool {
	words := trimApprovalFillers(approvalWords(text))
	if len(words) == 0 || slices.Contains(words, "?") || questionOpener(words[0]) {
		return false
	}

	if yesCounts && len(words) == 1 && plainYesWord(words[0]) {
		return true
	}

	length, ok := buildPhraseSuffix(words)
	if !ok {
		return false
	}

	return !slices.ContainsFunc(words[:len(words)-length], func(word string) bool {
		return !approvalPrefixWord(word)
	})
}

// trimApprovalFillers drops trailing words that do not change an approval.
func trimApprovalFillers(words []string) []string {
	for len(words) > 0 && approvalFiller(words[len(words)-1]) {
		words = words[:len(words)-1]
	}

	return words
}

// plannedMarkerPrefix starts the marker on an issue a proposal created.
const plannedMarkerPrefix = MarkerPrefix + "planned from=#"

// PlannedMarker keys an issue to the issue it was planned from and its proposal slug.
func PlannedMarker(from int, key string) string {
	return fmt.Sprintf("%s%d key=%s -->", plannedMarkerPrefix, from, key)
}

// PlannedFrom reads the planned marker out of an issue body. found is false when it carries none.
func PlannedFrom(body string) (from int, key string, found bool) {
	match := plannedMarkerRE.FindStringSubmatch(body)
	if match == nil {
		return 0, "", false
	}

	number, err := ParseIssueNumber(match[1])
	if err != nil {
		return 0, "", false
	}

	return number, match[2], true
}

// slicesMarkerPrefix starts the comment listing an approved slices proposal.
const slicesMarkerPrefix = MarkerPrefix + "slices from=#"

// SlicesMarker keys the slice list to the issue the slices were planned from.
func SlicesMarker(from int) string {
	return fmt.Sprintf("%s%d -->", slicesMarkerPrefix, from)
}

// SliceRef is one slice in the comment that closes a sliced issue.
type SliceRef struct {
	// Key is the proposal's slug.
	Key string
	// Title is the slice's title.
	Title string
	// Number is the issue the slice became.
	Number int
}

// RenderSlices renders the comment that closes the sliced issue with its slice list.
func RenderSlices(from int, slices []SliceRef) string {
	lines := make([]string, 0, len(slices))
	lines = append(lines, SlicesMarker(from), "## Slices", "")

	for _, s := range slices {
		lines = append(lines, fmt.Sprintf("- #%d %s", s.Number, s.Title))
	}

	return strings.Join(lines, "\n")
}

// appliedMarkerPrefix starts the marker that records a proposal has been applied.
const appliedMarkerPrefix = MarkerPrefix + "planned-applied from=#"

// AppliedMarker keys the application of a proposal to the issue it was planned from.
func AppliedMarker(from int) string {
	return fmt.Sprintf("%s%d -->", appliedMarkerPrefix, from)
}
