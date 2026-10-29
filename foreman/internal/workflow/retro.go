package workflow

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Bounds on a retro, so one comment stays within GitHub's limit.
const (
	// MaxRetroGroups bounds how many existing groups a retro prompt is shown.
	MaxRetroGroups = 100
	// MaxRetroFindings bounds how many findings one retro may report.
	MaxRetroFindings = 50
)

// RetroTargetKind is what a retro finding is about.
type RetroTargetKind string

// The kinds of retro target.
const (
	// TargetSkill is one of the factory's skills.
	TargetSkill RetroTargetKind = "skill"
	// TargetDocs is a repository's AGENTS.md or docs/agents/.
	TargetDocs RetroTargetKind = "docs"
	// TargetFactory is an area of the factory itself.
	TargetFactory RetroTargetKind = "factory"
)

// RetroTargetKinds is the closed set of target kinds.
func RetroTargetKinds() []RetroTargetKind {
	return []RetroTargetKind{TargetSkill, TargetDocs, TargetFactory}
}

var (
	// retroNameRE matches a skill name or factory area.
	retroNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	// retroDocsRE matches only the documents a repository steers runs with.
	retroDocsRE = regexp.MustCompile(`^(AGENTS\.md|docs/agents/[A-Za-z0-9._-]{1,64}\.md)$`)
	// retroKeyRE matches a run key: a commit SHA, a comment id or a findings branch SHA.
	retroKeyRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
)

// RetroTarget names what a finding is about: a skill, a repository document or a factory area.
type RetroTarget struct {
	// Kind is the target kind.
	Kind RetroTargetKind
	// Name is the skill name, document path or factory area.
	Name string
}

// String renders the target as the model writes it: "skill:implement".
func (t RetroTarget) String() string {
	return string(t.Kind) + ":" + t.Name
}

// ParseRetroTarget accepts only a well-formed target, so a finding can never name a path out of the
// repository or a target that does not exist in the spec.
func ParseRetroTarget(text string) (RetroTarget, error) {
	kind, name, ok := strings.Cut(text, ":")
	if !ok {
		return RetroTarget{}, fmt.Errorf("retro target %q has no kind", text)
	}

	k := RetroTargetKind(kind)
	if !slices.Contains(RetroTargetKinds(), k) {
		return RetroTarget{}, fmt.Errorf("unknown retro target kind %q", kind)
	}

	var valid bool

	switch k {
	case TargetDocs:
		valid = retroDocsRE.MatchString(name)
	case TargetSkill, TargetFactory:
		valid = retroNameRE.MatchString(name)
	}

	if !valid || strings.Contains(name, MarkerPrefix) {
		return RetroTarget{}, fmt.Errorf("retro target %q is not valid", text)
	}

	return RetroTarget{Kind: k, Name: name}, nil
}

// RetroFinding is one retro finding: what it is about, its group, how much it matters, a one-line
// claim and the evidence from the run.
type RetroFinding struct {
	// Target is what the finding is about.
	Target RetroTarget
	// Group is the group key, from the existing groups for the target or new.
	Group string
	// Severity is how much the finding matters.
	Severity Severity
	// Claim is the finding in one line.
	Claim string
	// Evidence is what the session showed.
	Evidence string
}

// retroWire is retro.json as the model writes it and as foreman stores it in a comment.
type retroWire struct {
	Version  int                `json:"version"`
	Findings []retroFindingWire `json:"findings"`
}

// retroFindingWire is one finding on the wire.
type retroFindingWire struct {
	Target   string   `json:"target"`
	Group    string   `json:"group"`
	Severity Severity `json:"severity"`
	Claim    string   `json:"claim"`
	Evidence string   `json:"evidence"`
}

// RetroOutputFile names the retro in the errors that reject it.
const RetroOutputFile = "retro.json"

// ParseRetro reads retro.json strictly, through the same parser as every other output file. An
// unknown field, a missing one, a malformed target, group, severity or claim rejects the whole file,
// so an invalid retro can never be posted in part. Every such problem is reported at once.
func ParseRetro(text string) ([]RetroFinding, error) {
	return parseOutput(text, RetroOutputFile, retroFindingsOf)
}

// retroFile is retro.json as the model writes it. Version is the one every output file carries,
// checked by the strict decoder. Findings is a pointer so a file that omits them is a problem of its
// own, where a retro carrying none is one foreman accepts.
type retroFile struct {
	Version  int                 `json:"version"`
	Findings *[]retroFindingWire `json:"findings"`
}

// retroFindingsOf turns a decoded retro into its findings and every problem found in it.
func retroFindingsOf(file retroFile) ([]RetroFinding, []string) {
	if file.Findings == nil {
		return nil, []string{"findings is required"}
	}

	var problems []string

	if len(*file.Findings) > MaxRetroFindings {
		problems = append(problems, fmt.Sprintf("retro has %d findings, at most %d are allowed",
			len(*file.Findings), MaxRetroFindings))
	}

	findings := make([]RetroFinding, 0, min(len(*file.Findings), MaxRetroFindings))

	// A retro over the cap is rejected, so its findings past the cap are not worth checking: the
	// loop stays bounded whatever the model wrote.
	for i, f := range (*file.Findings)[:min(len(*file.Findings), MaxRetroFindings)] {
		finding, more := retroFindingOf(i, f)
		problems = append(problems, more...)
		findings = append(findings, finding)
	}

	return findings, problems
}

// retroFindingOf validates one finding, returning it and every problem it has, each named by where
// the finding sits so a file with several is still fixable in one pass.
func retroFindingOf(index int, f retroFindingWire) (RetroFinding, []string) {
	named := fmt.Sprintf("findings[%d]: ", index)

	var problems []string

	target, err := ParseRetroTarget(f.Target)
	if err != nil {
		problems = append(problems, named+err.Error())
	}

	if !retroNameRE.MatchString(f.Group) {
		problems = append(problems, named+fmt.Sprintf("group key %q is not valid", f.Group))
	}

	if !slices.Contains(Severities(), f.Severity) {
		problems = append(problems, named+fmt.Sprintf("unknown retro severity %q", f.Severity))
	}

	if err := validText("retro claim", f.Claim, true); err != nil {
		problems = append(problems, named+err.Error())
	}

	if err := validText("retro evidence", f.Evidence, false); err != nil {
		problems = append(problems, named+err.Error())
	}

	return RetroFinding{
		Target: target, Group: f.Group, Severity: f.Severity, Claim: f.Claim, Evidence: f.Evidence,
	}, problems
}

// Marker prefixes of a retro comment.
const (
	// RetroMarkerPrefix starts the marker line of a retro comment.
	RetroMarkerPrefix = MarkerPrefix + "retro "
	// retroJSONOpen starts the machine-readable copy of a retro inside its comment.
	retroJSONOpen = MarkerPrefix + "retro-json\n"
	// retroJSONClose ends it.
	retroJSONClose = "\n-->"
	// retroMarkerFields is how many key=value fields the marker line carries.
	retroMarkerFields = 3
)

// RetroMarker keys a retro comment to its issue, agent role and run key.
func RetroMarker(number int, role Role, key string) string {
	return fmt.Sprintf("%sissue=%d role=%s key=%s -->", RetroMarkerPrefix, number, role, key)
}

// RetroComment is a retro comment already on an issue.
type RetroComment struct {
	// Issue is the issue the retro worked.
	Issue int
	// Role is the agent role that reported it.
	Role Role
	// Key is the run key: a commit SHA, a PR head SHA or a comment id.
	Key string
	// Findings are what the retro reported.
	Findings []RetroFinding
}

// RenderRetro renders the comment for markerLine and findings: a readable summary followed by the
// hidden JSON foreman reads back. It refuses anything ParseRetro would refuse, and a comment over
// GitHub's limit, so what it returns always reads back whole.
func RenderRetro(markerLine string, findings []RetroFinding) (string, error) {
	rc, err := parseRetroMarker(markerLine)
	if err != nil {
		return "", err
	}

	wire := retroWire{Version: OutputFileVersion, Findings: make([]retroFindingWire, 0, len(findings))}
	for _, f := range findings {
		wire.Findings = append(wire.Findings, retroFindingWire{
			Target: f.Target.String(), Group: f.Group, Severity: f.Severity, Claim: f.Claim, Evidence: f.Evidence,
		})
	}

	raw, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("encode retro: %w", err)
	}

	// Parse what was rendered, so a value that would not read back is refused here rather than posted.
	if _, err := ParseRetro(string(raw)); err != nil {
		return "", err
	}

	var b strings.Builder

	b.WriteString(markerLine)
	fmt.Fprintf(&b, "\n## Retro: %s at %s\n", rc.Role, rc.Key)

	for _, f := range findings {
		fmt.Fprintf(&b, "\n### %s `%s` / `%s`\n\n%s\n\nEvidence:\n\n%s\n",
			f.Severity.title(), f.Target, f.Group, f.Claim, f.Evidence)
	}

	if len(findings) == 0 {
		b.WriteString("\nNo findings.\n")
	}

	fmt.Fprintf(&b, "\n%s%s%s", retroJSONOpen, raw, retroJSONClose)

	if n := utf8.RuneCountInString(b.String()); n > CommentLimitChars {
		return "", fmt.Errorf("retro comment is %d characters, over the %d limit", n, CommentLimitChars)
	}

	return b.String(), nil
}

// ParseRetroComment reads a retro comment. found is false when the body is not a retro comment; a
// body that starts with one is parsed strictly.
func ParseRetroComment(body string) (RetroComment, bool, error) {
	if !strings.HasPrefix(body, RetroMarkerPrefix) {
		return RetroComment{}, false, nil
	}

	line, rest, _ := strings.Cut(body, "\n")

	rc, err := parseRetroMarker(line)
	if err != nil {
		return RetroComment{}, true, err
	}

	_, after, ok := strings.Cut(rest, retroJSONOpen)
	if !ok {
		return RetroComment{}, true, errors.New("retro comment has no JSON block")
	}

	tail := after

	before0, _, ok0 := strings.Cut(tail, retroJSONClose)
	if !ok0 {
		return RetroComment{}, true, errors.New("retro comment's JSON block is not closed")
	}

	rc.Findings, err = ParseRetro(before0)
	if err != nil {
		return RetroComment{}, true, err
	}

	return rc, true, nil
}

// retroMarkerSpec is the retro marker's wire format.
var retroMarkerSpec = markerSpec{ //nolint:gochecknoglobals // a read-only format
	prefix: RetroMarkerPrefix, fields: retroMarkerFields, what: "retro",
}

// parseRetroMarker parses one complete marker line: its issue, agent role and run key.
func parseRetroMarker(line string) (RetroComment, error) {
	if !strings.HasPrefix(line, RetroMarkerPrefix) {
		return RetroComment{}, fmt.Errorf("retro marker %q does not start with %q", line, RetroMarkerPrefix)
	}

	if strings.Contains(line, "\n") {
		return RetroComment{}, errors.New("retro marker spans lines")
	}

	var rc RetroComment

	_, err := parseMarkerFields(line, retroMarkerSpec, func(key, value string) error {
		return setRetroMarkerField(&rc, key, value)
	})
	if err != nil {
		return RetroComment{}, err
	}

	return rc, nil
}

// setRetroMarkerField fills one field of a retro marker.
func setRetroMarkerField(rc *RetroComment, key, value string) error {
	switch key {
	case "issue":
		issue, err := ParseIssueNumber(value)
		if err != nil {
			return err
		}

		rc.Issue = issue
	case "role":
		role, err := ParseRole(value)
		if err != nil {
			return err
		}

		rc.Role = role
	case "key":
		if !retroKeyRE.MatchString(value) {
			return fmt.Errorf("run key %q is not a marker-safe token", value)
		}

		rc.Key = value
	default:
		return fmt.Errorf("bad retro marker field %q", key)
	}

	return nil
}

// RetroGroup is an existing group shown to a retro: its key and the first one-line claim reported for
// it. Evidence is deliberately left out, so no retro reads another's.
type RetroGroup struct {
	// Target is what the group is about.
	Target RetroTarget
	// Key is the group key.
	Key string
	// Claim is the first claim reported for the group.
	Claim string
}

// RetroGroups collects the repository's existing groups the agent role reported, across every issue,
// one per target and key, in a stable order. reports is the repository's retro log, so a group a retro
// on another issue made is one this retro can pick and the dashboard can count. exceptKey is the
// current run, whose own earlier attempt is not "existing". Another agent role's groups are left out:
// the Reviewer's context never contains the Implementer's retro.
func RetroGroups(reports []DashboardReport, role Role, exceptKey string) []RetroGroup {
	oldest := map[string]DashboardReport{}

	for _, report := range reports {
		if report.Role != role || report.Key == exceptKey {
			continue
		}

		key := report.Target.String() + "\x00" + report.Group

		seen, ok := oldest[key]
		if !ok || reportEarlier(report, seen) {
			oldest[key] = report
		}
	}

	groups := make([]RetroGroup, 0, len(oldest))

	for _, report := range oldest {
		groups = append(groups, RetroGroup{Target: report.Target, Key: report.Group, Claim: report.Claim})
	}

	slices.SortFunc(groups, func(a, b RetroGroup) int {
		return cmp.Or(cmp.Compare(a.Target.String(), b.Target.String()), cmp.Compare(a.Key, b.Key))
	})

	if len(groups) > MaxRetroGroups {
		groups = groups[:MaxRetroGroups]
	}

	return groups
}

// reportEarlier reports whether a's claim is the one its group carries instead of b's: the oldest
// report wins, and the issue, agent role, run key and claim break a tie, so the groups read the same
// whatever order the log arrives in.
func reportEarlier(a, b DashboardReport) bool {
	return cmp.Or(
		a.At.Compare(b.At),
		cmp.Compare(a.Issue, b.Issue),
		cmp.Compare(a.Role, b.Role),
		cmp.Compare(a.Key, b.Key),
		cmp.Compare(a.Claim, b.Claim),
	) < 0
}
