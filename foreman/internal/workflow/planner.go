package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// The proposal shapes. Only Single and Slices are implemented by this slice; Map and Resolution are
// part of ticket #47 and slot into the same type.
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

	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&p); err != nil {
		return PlannerProposal{}, true, fmt.Errorf("decode stored proposal: %w", err)
	}

	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return PlannerProposal{}, true, errors.New("stored proposal has trailing data")
	}

	if problems := proposalProblems(p); len(problems) > 0 {
		return PlannerProposal{}, true, errors.New("stored proposal is invalid: " + strings.Join(problems, "; "))
	}

	return p, true, nil
}

// Approves reports whether a writer's reply is approval of the latest final round: "yes", or a
// comment asking to build.
func Approves(text string) bool {
	reply := strings.ToLower(strings.TrimSpace(text))
	reply = strings.TrimRight(reply, " \t\r\n.!?")

	return reply == "yes" || strings.Contains(reply, "ready to build")
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
