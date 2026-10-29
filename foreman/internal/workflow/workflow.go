// Package workflow is foreman's pure core: agent roles, labels, markers and review verdicts. It does
// no I/O, so every rule here can be checked by calling a function with values.
package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Role is an agent role. Only the known set parses; anything else fails closed.
type Role string

// The agent roles named by the spec.
const (
	// Planner turns an issue into questions or a proposal.
	Planner Role = "planner"
	// Researcher answers a research ticket.
	Researcher Role = "researcher"
	// Implementer turns an issue into a pull request.
	Implementer Role = "implementer"
	// Reviewer reviews a pull request.
	Reviewer Role = "reviewer"
	// Curator proposes merges for the retro dashboard.
	Curator Role = "curator"
)

// KnownRoles is the closed set of agent roles, in spec order.
func KnownRoles() []Role {
	return []Role{Planner, Researcher, Implementer, Reviewer, Curator}
}

// Limits shared by the prompts, comments and API calls.
const (
	// BodyLimitChars caps issue and review text put into a prompt.
	BodyLimitChars = 20_000
	// CommentLimitChars caps any comment foreman posts.
	CommentLimitChars = 60_000
	// MaxReviewRounds is how many distinct commits are reviewed before a person is called.
	MaxReviewRounds = 3
)

// Exit codes of `run <agent role>`, read by `start <agent role>`.
const (
	// ReviewClean means no high or medium findings.
	ReviewClean = 0
	// ReviewFindings means high or medium findings were posted.
	ReviewFindings = 10
	// ReviewExhausted means findings remain after MaxReviewRounds rounds.
	ReviewExhausted = 11
	// ExitReported means the factory half already posted the reason on the issue, so `start` only
	// records the failure and adds nothing but the transition marker.
	ExitReported = 12
)

// Labels of the handoff channel. Every factory label is prefixed "factory:".
const (
	// LabelPlan queues an issue for the Planner.
	LabelPlan = "factory:plan"
	// LabelPlanning marks an issue the Planner has claimed.
	LabelPlanning = "factory:planning"
	// LabelResearch queues an issue for the Researcher.
	LabelResearch = "factory:research"
	// LabelResearching marks an issue the Researcher has claimed.
	LabelResearching = "factory:researching"
	// LabelImplement queues an issue for the Implementer.
	LabelImplement = "factory:implement"
	// LabelImplementing marks an issue the Implementer has claimed.
	LabelImplementing = "factory:implementing"
	// LabelReview queues an issue for the Reviewer.
	LabelReview = "factory:review"
	// LabelReviewing marks an issue the Reviewer has claimed.
	LabelReviewing = "factory:reviewing"
	// LabelWaiting marks an issue whose Planner round awaits a writer.
	LabelWaiting = "factory:waiting"
	// LabelDone marks an issue that needs only a human to merge it.
	LabelDone = "factory:done"
	// LabelFailed marks an issue a person needs to look at.
	LabelFailed = "factory:failed"
	// LabelPaused takes an issue out of the factory's hands. It is the only label a human sets.
	LabelPaused = "factory:paused"

	// LabelMap labels a wayfinder map, whose tickets are worked instead.
	LabelMap = "wayfinder:map"
	// LabelResearchTicket marks a ticket for the Researcher.
	LabelResearchTicket = "wayfinder:research"
	// LabelGrilling marks a grilling ticket.
	LabelGrilling = "wayfinder:grilling"
	// LabelPrototype marks a prototype ticket.
	LabelPrototype = "prototype"
	// LabelWayfinderPrototype marks a prototype ticket in the wayfinder's namespace.
	LabelWayfinderPrototype = "wayfinder:prototype"
	// LabelTask marks a task ticket.
	LabelTask = "task"
	// DashboardTitle is the retro dashboard's title; the dashboard is never routed.
	DashboardTitle = "Retro dashboard"
)

// State is a handoff state, carried by the label of the same name.
type State string

// The handoff states, named by the label that holds them.
const (
	// Plan queues an issue for the Planner.
	Plan State = LabelPlan
	// Planning is a Planner claim.
	Planning State = LabelPlanning
	// Research queues an issue for the Researcher.
	Research State = LabelResearch
	// Researching is a Researcher claim.
	Researching State = LabelResearching
	// Implement queues an issue for the Implementer.
	Implement State = LabelImplement
	// Implementing is an Implementer claim.
	Implementing State = LabelImplementing
	// Review queues an issue for the Reviewer.
	Review State = LabelReview
	// Reviewing is a Reviewer claim.
	Reviewing State = LabelReviewing
	// Waiting waits for a writer to answer the Planner.
	Waiting State = LabelWaiting
	// Done waits for a writer to merge the pull request.
	Done State = LabelDone
	// Failed waits for a writer to send the issue back.
	Failed State = LabelFailed
	// Paused waits until a writer removes the label.
	Paused State = LabelPaused
	// Closed is a resolved ticket's terminal state. It has no label: the ticket is closed instead.
	Closed State = "closed"
)

// States is the closed set of handoff states, in spec order.
func States() []State {
	return []State{
		Plan, Planning, Research, Researching, Implement, Implementing, Review, Reviewing,
		Waiting, Done, Failed, Paused, Closed,
	}
}

// ParseState accepts only a known handoff state.
func ParseState(name string) (State, error) {
	if s := State(name); slices.Contains(States(), s) {
		return s, nil
	}

	return "", fmt.Errorf("unknown handoff state %q", name)
}

// Queue is an agent role's pair of handoff labels.
type Queue struct {
	// Role is the agent role that takes the queue.
	Role Role
	// State queues the issue for the role.
	State State
	// Running is the label a claim adds.
	Running State
}

// Queues lists the agent roles that queue work, in scheduling order. The Curator looks after the
// retro dashboard and takes no issue queue.
func Queues() []Queue {
	return []Queue{
		{Role: Reviewer, State: Review, Running: Reviewing},
		{Role: Implementer, State: Implement, Running: Implementing},
		{Role: Planner, State: Plan, Running: Planning},
		{Role: Researcher, State: Research, Running: Researching},
	}
}

// Queued returns the state that queues an issue for role.
func Queued(role Role) (State, bool) {
	for _, q := range Queues() {
		if q.Role == role {
			return q.State, true
		}
	}

	return "", false
}

// Running returns the state that marks a claim by role.
func Running(role Role) (State, bool) {
	for _, q := range Queues() {
		if q.Role == role {
			return q.Running, true
		}
	}

	return "", false
}

// RoleOf names the agent role that owns a handoff state.
func RoleOf(state State) (Role, bool) {
	for _, q := range Queues() {
		if q.State == state || q.Running == state {
			return q.Role, true
		}
	}

	return "", false
}

// FactoryLabels lists every label foreman creates, idempotently, for an assigned repository. A closed
// ticket carries no label, so Closed is left out.
func FactoryLabels() []string {
	out := make([]string, 0, len(States()))

	for _, s := range States() {
		if s == Closed {
			continue
		}

		out = append(out, string(s))
	}

	return out
}

// Severity is how much a review finding matters. Only the known set parses.
type Severity string

// The severities of a review finding.
const (
	// SeverityHigh is a bug, a spec miss or a security problem that must be fixed before merge.
	SeverityHigh Severity = "high"
	// SeverityMedium is a documented-standard breach or a notable defect.
	SeverityMedium Severity = "medium"
	// SeverityLow is a judgement call or a nit.
	SeverityLow Severity = "low"
)

// Severities is the closed set of severities, most severe first.
func Severities() []Severity {
	return []Severity{SeverityHigh, SeverityMedium, SeverityLow}
}

// Blocks reports whether a finding of this severity sends the pull request back.
func (s Severity) Blocks() bool {
	switch s {
	case SeverityHigh, SeverityMedium:
		return true
	case SeverityLow:
		return false
	}

	return false
}

// title is the severity as a heading word: "High".
func (s Severity) title() string {
	return strings.ToUpper(string(s[:1])) + string(s[1:])
}

var (
	// issueNumberRE matches digits only.
	issueNumberRE = regexp.MustCompile(`^[0-9]{1,9}$`)
	// repoRE matches owner/name.
	repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// ParseRole accepts only a known agent role.
func ParseRole(name string) (Role, error) {
	for _, r := range KnownRoles() {
		if string(r) == name {
			return r, nil
		}
	}

	return "", fmt.Errorf("unknown agent role %q", name)
}

// ParseIssueNumber accepts digits only: the number ends up in a path and a branch name.
func ParseIssueNumber(text string) (int, error) {
	if !issueNumberRE.MatchString(text) {
		return 0, fmt.Errorf("not an issue number: %q", text)
	}

	var n int
	if _, err := fmt.Sscanf(text, "%d", &n); err != nil {
		return 0, fmt.Errorf("not an issue number: %q: %w", text, err)
	}

	return n, nil
}

// ValidateRepo checks the owner/name shape, since a repo ends up in paths and URLs.
func ValidateRepo(repo string) error {
	if !repoRE.MatchString(repo) || strings.HasPrefix(repo, "..") || strings.Contains(repo, "/..") {
		return fmt.Errorf("repo must be owner/name, got %q", repo)
	}

	return nil
}

// Issue is the part of a GitHub issue foreman reads.
type Issue struct {
	Number        int
	Title         string
	Body          string
	State         string
	Author        string
	Labels        []string
	Comments      int
	IsPullRequest bool
}

// UnmarshalJSON reads an issue; the issues endpoint marks pull requests with a pull_request key.
func (i *Issue) UnmarshalJSON(data []byte) error {
	var raw struct {
		Number   int    `json:"number"`
		Title    string `json:"title"`
		Body     string `json:"body"`
		State    string `json:"state"`
		Comments int    `json:"comments"`
		User     *struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		PullRequest *json.RawMessage `json:"pull_request"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode issue: %w", err)
	}

	author := ""
	if raw.User != nil {
		author = raw.User.Login
	}

	labels := make([]string, 0, len(raw.Labels))
	for _, l := range raw.Labels {
		labels = append(labels, l.Name)
	}

	*i = Issue{
		Number: raw.Number, Title: raw.Title, Body: raw.Body, State: raw.State, Author: author,
		Labels: labels, Comments: raw.Comments, IsPullRequest: raw.PullRequest != nil,
	}

	return nil
}

// HasLabel reports whether the issue carries label.
func HasLabel(issue Issue, label string) bool {
	return slices.Contains(issue.Labels, label)
}

// FactoryLabelled reports whether the issue already carries a factory label, so it is not new.
func FactoryLabelled(issue Issue) bool {
	for _, l := range issue.Labels {
		if strings.HasPrefix(l, "factory:") {
			return true
		}
	}

	return false
}

// NeverRouted reports whether an issue is one the factory never routes: a wayfinder map or the retro
// dashboard.
func NeverRouted(issue Issue) bool {
	return HasLabel(issue, LabelMap) || issue.Title == DashboardTitle
}

// Route returns the first queue for a new issue, or false for an issue the factory never routes. A
// paused or already-labelled issue is not new. The caller applies the label.
func Route(issue Issue) (State, bool) {
	if issue.IsPullRequest || FactoryLabelled(issue) || NeverRouted(issue) {
		return "", false
	}

	switch {
	case HasLabel(issue, LabelResearchTicket):
		return Research, true
	case HasLabel(issue, LabelGrilling), HasLabel(issue, LabelPrototype),
		HasLabel(issue, LabelWayfinderPrototype), HasLabel(issue, LabelTask):
		return Plan, true
	}

	return Plan, true
}

// BotAuthored reports whether a login is a GitHub App or another bot. A promoted retro group is
// authored by a factory App and waits for a writer's comment, like any other non-writer's issue.
func BotAuthored(login string) bool {
	return strings.HasSuffix(login, "[bot]")
}

// WriterPermission reports whether a collaborator permission grants write access. Anything else,
// including a permission the factory could not read, is not a writer.
func WriterPermission(permission string) bool {
	switch permission {
	case "admin", "maintain", "write", "push":
		return true
	case "read", "triage", "none", "":
		return false
	}

	return false
}

// WriterComment reports whether any comment was written by a writer.
func WriterComment(comments []Item, writers map[string]bool) bool {
	for _, c := range comments {
		if writers[c.Author] {
			return true
		}
	}

	return false
}

// BlockedBy reads the fallback "Blocked by: #1, #2" line where native dependencies are unavailable.
func BlockedBy(body string) []int {
	var (
		out  []int
		seen = map[int]bool{}
	)

	for line := range strings.SplitSeq(body, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Blocked by:")
		if !ok {
			continue
		}

		for field := range strings.FieldsSeq(rest) {
			digits, ok := strings.CutPrefix(strings.TrimSuffix(field, ","), "#")
			if !ok {
				continue
			}

			number, err := ParseIssueNumber(digits)
			if err != nil || seen[number] {
				continue
			}

			seen[number] = true
			out = append(out, number)
		}
	}

	return out
}

// Item is a comment or review: an id, a body, its author and when it was last written.
type Item struct {
	ID     int64  `json:"id"`
	Body   string `json:"body"`
	Author string `json:"-"`
	// UpdatedAt is when the comment or review was last written; zero when the answer has no date.
	UpdatedAt time.Time `json:"-"`
}

// UnmarshalJSON reads a comment or review, whose author is nested under user.login.
func (i *Item) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID        int64  `json:"id"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
		User      *struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode comment: %w", err)
	}

	author := ""
	if raw.User != nil {
		author = raw.User.Login
	}

	when := raw.UpdatedAt
	if when == "" {
		when = raw.CreatedAt
	}

	updated, err := parseOptionalTime(when)
	if err != nil {
		return fmt.Errorf("decode comment: %w", err)
	}

	*i = Item{ID: raw.ID, Body: raw.Body, Author: author, UpdatedAt: updated}

	return nil
}

// parseOptionalTime reads an API timestamp, leaving an absent one zero.
func parseOptionalTime(text string) (time.Time, error) {
	if text == "" {
		return time.Time{}, nil
	}

	at, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", text, err)
	}

	return at, nil
}

// MarkerPrefix starts every marker comment foreman posts, and never appears in a model's text.
const MarkerPrefix = "<!-- factory:"

// transitionPrefix starts a handoff marker.
const transitionPrefix = MarkerPrefix + "handoff "

// Transition is the marker comment that records one handoff between agent roles.
type Transition struct {
	// Role is the agent role that made the transition.
	Role Role
	// RunID identifies the run that made it.
	RunID string
	// From is the state the issue left.
	From State
	// To is the state the issue entered.
	To State
	// SHA is the commit the transition concerns, when there is one.
	SHA string
}

// trans values are tokens that cannot break the marker or inject a second one.
var (
	runIDRE = regexp.MustCompile(`^[A-Za-z0-9._:@+-]{1,128}$`)
	shaRE   = regexp.MustCompile(`^[A-Za-z0-9._-]{0,64}$`)
)

// RenderTransition renders the marker for t, refusing a value that could break the marker.
func RenderTransition(t Transition) (string, error) {
	if err := validTransition(t); err != nil {
		return "", err
	}

	return fmt.Sprintf("%srole=%s run=%s from=%s to=%s sha=%s -->",
		transitionPrefix, t.Role, t.RunID, t.From, t.To, t.SHA), nil
}

// transitionFields is how many key=value fields a marker carries.
const transitionFields = 5

// transitionKeys are the fields of a marker, in the order it renders them.
func transitionKeys() []string {
	return []string{"role", "run", "from", "to", "sha"}
}

// validTransition checks every value against what a marker can carry.
func validTransition(t Transition) error {
	if _, err := ParseRole(string(t.Role)); err != nil {
		return err
	}

	if _, err := ParseState(string(t.From)); err != nil {
		return err
	}

	if _, err := ParseState(string(t.To)); err != nil {
		return err
	}

	if !runIDRE.MatchString(t.RunID) {
		return fmt.Errorf("run id %q is not a marker-safe token", t.RunID)
	}

	if !shaRE.MatchString(t.SHA) {
		return fmt.Errorf("commit %q is not a marker-safe token", t.SHA)
	}

	return nil
}

// ParseTransition reads a marker at the top of body. found is false when body does not start with
// one; a body that does is parsed strictly, so a malformed marker is an error.
func ParseTransition(body string) (Transition, bool, error) {
	if !strings.HasPrefix(body, transitionPrefix) {
		return Transition{}, false, nil
	}

	line, _, _ := strings.Cut(body, "\n")

	rest, ok := strings.CutSuffix(line, " -->")
	if !ok {
		return Transition{}, true, errors.New("transition marker is not closed")
	}

	t, err := transitionFromFields(strings.TrimPrefix(rest, transitionPrefix))
	if err != nil {
		return Transition{}, true, err
	}

	return t, true, nil
}

// transitionFromFields parses and validates a marker's key=value fields.
func transitionFromFields(text string) (Transition, error) {
	fields := strings.Fields(text)
	if len(fields) != transitionFields {
		return Transition{}, fmt.Errorf("transition marker has %d fields, want %d", len(fields), transitionFields)
	}

	values, err := transitionValues(fields)
	if err != nil {
		return Transition{}, err
	}

	t := Transition{
		Role: Role(values["role"]), RunID: values["run"],
		From: State(values["from"]), To: State(values["to"]), SHA: values["sha"],
	}

	if err := validTransition(t); err != nil {
		return Transition{}, err
	}

	return t, nil
}

// transitionValues reads the fields strictly: no unknown or repeated key, and only sha may be empty.
func transitionValues(fields []string) (map[string]string, error) {
	values := make(map[string]string, len(fields))

	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok || !slices.Contains(transitionKeys(), key) {
			return nil, fmt.Errorf("unknown transition field %q", field)
		}

		if _, seen := values[key]; seen {
			return nil, fmt.Errorf("repeated transition field %q", key)
		}

		// Only the commit may be empty: a claim before any commit exists has no SHA yet.
		if value == "" && key != "sha" {
			return nil, fmt.Errorf("empty transition field %q", field)
		}

		values[key] = value
	}

	for _, key := range transitionKeys() {
		if _, ok := values[key]; !ok {
			return nil, fmt.Errorf("transition marker has no %s", key)
		}
	}

	return values, nil
}

// LatestTransition is the last transition marker in API order, which is oldest first.
func LatestTransition(comments []Item) (Transition, bool, error) {
	var (
		latest Transition
		found  bool
	)

	for _, c := range comments {
		t, ok, err := ParseTransition(c.Body)
		if err != nil {
			return Transition{}, false, err
		}

		if ok {
			latest, found = t, true
		}
	}

	return latest, found, nil
}

// Repair returns the state label to re-apply when the latest marker names a state the issue does not
// carry. A paused issue is frozen, so it is never repaired. A closed ticket has no label to apply.
func Repair(issue Issue, latest Transition, found bool) (State, bool) {
	if !found || latest.To == Closed || HasLabel(issue, LabelPaused) ||
		HasLabel(issue, string(latest.To)) {
		return "", false
	}

	return latest.To, true
}

// PickIssue returns the first issue that is not a pull request: oldest first, as the API returns them.
func PickIssue(issues []Issue) (Issue, bool) {
	for _, i := range issues {
		if !i.IsPullRequest {
			return i, true
		}
	}

	return Issue{}, false
}

// BranchName is the branch an issue's work lives on.
func BranchName(number int) string {
	return fmt.Sprintf("agent/issue-%d", number)
}

// PRBody keeps the pull request body the model wrote and makes sure the pull request links the
// issue, so merging it closes the issue.
func PRBody(number int, body string) string {
	link := fmt.Sprintf("#%d", number)
	if strings.Contains(body, link) {
		return body
	}

	return strings.TrimSpace(body) + "\n\nCloses " + link
}

// Truncate keeps at most n characters; none when n is not positive.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}

	r := []rune(s)
	if len(r) <= n {
		return s
	}

	return string(r[:n])
}

// ReviewMarker keys a review to the commit it judged.
func ReviewMarker(sha string) string {
	return fmt.Sprintf("<!-- agent-review:%s -->", sha)
}

// ReviewMarkerPrefix starts every review foreman posts.
const ReviewMarkerPrefix = "<!-- agent-review:"

// Blocking keeps the findings that must be fixed before the pull request is accepted.
func Blocking(findings []ReviewFinding) []ReviewFinding {
	var out []ReviewFinding

	for _, f := range findings {
		if f.Severity.Blocks() {
			out = append(out, f)
		}
	}

	return out
}

// StartsWith keeps the items whose body starts with prefix, in API order.
func StartsWith(items []Item, prefix string) []Item {
	var out []Item

	for _, i := range items {
		if strings.HasPrefix(i.Body, prefix) {
			out = append(out, i)
		}
	}

	return out
}

// ReviewRounds counts the distinct commits reviewed so far, counting sha whether or not it is posted yet.
func ReviewRounds(reviews []Item, sha string) int {
	marked := map[string]bool{ReviewMarker(sha): true}

	for _, r := range StartsWith(reviews, ReviewMarkerPrefix) {
		first, _, _ := strings.Cut(r.Body, "\n")
		marked[first] = true
	}

	return len(marked)
}
