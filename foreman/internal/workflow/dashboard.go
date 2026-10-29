package workflow

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// The retro dashboard's rules and bounds.
const (
	// ReadyIssues is how many different issues must report a group before it is ready to promote.
	ReadyIssues = 2
	// WatchingWindow is how long a Watching group stays after its last report.
	WatchingWindow = 60 * 24 * time.Hour
	// MaxDashboardGroups bounds how many groups one dashboard section shows.
	MaxDashboardGroups = 30
	// MaxGroupReports bounds how many reports one group shows.
	MaxGroupReports = 5
	// DashboardBodyLimit bounds the dashboard body, under GitHub's 65,536-character limit.
	DashboardBodyLimit = 60_000
	// dashboardClaimChars is how much of a claim the dashboard shows.
	dashboardClaimChars = 200
	// dashboardDate is how the dashboard writes a date.
	dashboardDate = "2006-01-02"
)

// GroupID names one retro group: what it is about and its group key.
type GroupID struct {
	// Target is what the group is about.
	Target RetroTarget
	// Key is the group key a retro picked from the target's existing groups, or made new.
	Key string
}

// String renders the group as "skill:implement/guide-misses-tests".
func (g GroupID) String() string {
	return g.Target.String() + "/" + g.Key
}

// DashboardActionKind is what a writer did to a group.
type DashboardActionKind string

// The writer actions a dashboard records.
const (
	// ActionPromote turned a group into an issue.
	ActionPromote DashboardActionKind = "promote"
	// ActionIgnore hides a group from promotion.
	ActionIgnore DashboardActionKind = "ignore"
	// ActionMerge merged two groups into one.
	ActionMerge DashboardActionKind = "merge"
	// ActionCurate asked the Curator for a run.
	ActionCurate DashboardActionKind = "curate"
)

// DashboardAction is one writer action, recorded in the dashboard body so a later poll replays it.
// A merge names two groups: the first is the one the dashboard keeps. Applying a merge to the
// reports behind both groups belongs to the writer-command ticket; this draws only what it is told.
type DashboardAction struct {
	// Kind is what the writer did.
	Kind DashboardActionKind
	// Groups are the groups the action names: one, or two for a merge.
	Groups []GroupID
	// By is the writer's login.
	By string
	// At is when the action was recorded.
	At time.Time
	// URL links the record, when there is one.
	URL string
}

// DashboardProposal is a Curator proposal shown under Proposed merges.
type DashboardProposal struct {
	// ID identifies the proposal, so a writer can reject it by id.
	ID string
	// Groups are the groups the proposal would merge.
	Groups []GroupID
	// Reason is the proposal in one line.
	Reason string
}

// DashboardLink is a Curator link between groups, which needs no writer command. A link may cross
// targets, where a merge may not.
type DashboardLink struct {
	// ID identifies the link, so a repeat updates it rather than adding another.
	ID string
	// Groups are the two groups the link relates.
	Groups []GroupID
	// Reason says why they are related, in one line.
	Reason string
}

// DashboardCurator is what the dashboard records about Curator runs: when one last ran, and the
// claim a poll takes when it queues one so a second is never queued at the same time.
type DashboardCurator struct {
	// LastRun is when the last Curator run finished.
	LastRun time.Time
	// Claim is when a poll queued a Curator run that has not finished yet.
	Claim time.Time
}

// DashboardState is what the dashboard body records beyond the retro log: what writers have done and
// what the Curator has proposed.
type DashboardState struct {
	// Actions are the writer actions, oldest first.
	Actions []DashboardAction
	// Proposals are the Curator's merge proposals, in the order it filed them.
	Proposals []DashboardProposal
	// Links are the Curator's links between groups.
	Links []DashboardLink
	// Rejected are the proposals writers refused for good.
	Rejected []DashboardRejection
	// Published are the private lessons writers published to the factory repo.
	Published []DashboardPublication
	// Curator records the Curator's last run and any run a poll has queued.
	Curator DashboardCurator
	// Handled are the command comments foreman has already dealt with, oldest first.
	Handled []int64
}

// DashboardReport is one retro finding with where it came from: the issue, agent role and run that
// reported it.
type DashboardReport struct {
	// Issue is the issue the retro worked.
	Issue int
	// Role is the agent role that reported the finding.
	Role Role
	// Key is the run key the retro is keyed by.
	Key string
	// At is when the report's retro comment was last written.
	At time.Time
	// Target is what the finding is about.
	Target RetroTarget
	// Group is the group key the finding picked.
	Group string
	// Severity is how much the finding matters.
	Severity Severity
	// Claim is the finding in one line.
	Claim string
	// Evidence is what the run showed, kept out of the lesson a promotion shows first.
	Evidence string
}

// DashboardRejection is a Curator proposal a writer refused for good. Foreman never shows the pair
// again, whatever id a later proposal gives it.
type DashboardRejection struct {
	// ID is the id of the proposal that was rejected.
	ID string
	// Groups are the groups the rejected proposal would have merged.
	Groups []GroupID
	// By is the writer's login.
	By string
	// At is when the writer rejected it.
	At time.Time
	// URL links the command comment that rejected it, when there is one.
	URL string
}

// DashboardPublication is a private repository's lesson a writer published to the factory repo.
type DashboardPublication struct {
	// Group is the group whose lesson was published.
	Group GroupID
	// By is the writer's login.
	By string
	// At is when it was published.
	At time.Time
	// URL links the issue the lesson was published as.
	URL string
}

// DashboardGroup is one retro group and every report behind it.
type DashboardGroup struct {
	// ID is the group's target and key.
	ID GroupID
	// Reports are the findings behind it, newest first.
	Reports []DashboardReport
}

// DashboardEntry is a group a writer promoted or ignored, with the reports the log still holds.
type DashboardEntry struct {
	// Action is what the writer did.
	Action DashboardAction
	// Reports are the group's findings, newest first. Empty when the log no longer has the group.
	Reports []DashboardReport
}

// ID is the group the action names.
func (e DashboardEntry) ID() GroupID {
	if len(e.Action.Groups) == 0 {
		return GroupID{}
	}

	return e.Action.Groups[0]
}

// DashboardView is the dashboard's sections, already decided and in drawing order.
type DashboardView struct {
	// Ready are the groups ready to promote, most corroborated first.
	Ready []DashboardGroup
	// Proposals are the Curator's proposals no writer has rejected or merged away.
	Proposals []DashboardProposal
	// Links are the Curator's links no writer has merged away.
	Links []DashboardLink
	// Watching are the groups not ready yet, newest report first.
	Watching []DashboardGroup
	// Promoted are the groups a writer promoted and no later report mentions.
	Promoted []DashboardEntry
	// Ignored are the groups a writer ignored.
	Ignored []DashboardEntry
	// Regressed are the groups a writer promoted that a later report mentions again.
	Regressed []DashboardEntry
	// Published are the private lessons writers published to the factory repo.
	Published []DashboardPublication
	// State is what the body records for the next poll.
	State DashboardState
}

// BuildDashboard decides every section from the retro log, the writer actions and the Curator's
// proposals. now dates the Watching rule, so a test can move the clock.
func BuildDashboard(reports []DashboardReport, state DashboardState, now time.Time) DashboardView {
	groups := groupReports(mergedReports(reports, state.Actions))
	actions := lastActions(state.Actions)
	view := DashboardView{
		Proposals: liveProposals(state), Links: liveLinks(state), Published: state.Published, State: state,
	}
	seen := make(map[string]bool, len(groups))

	for _, g := range groups {
		key := g.ID.String()
		seen[key] = true

		if action, ok := actions[key]; ok {
			view.addEntry(g, action)

			continue
		}

		switch {
		case groupReady(g.Reports):
			view.Ready = append(view.Ready, g)
		case watching(g, now):
			view.Watching = append(view.Watching, g)
		}
	}

	// An action outlives the reports it was taken on: keep drawing it after they expire. A merge is
	// drawn once, under the group its reports were folded into.
	for key, action := range actions {
		if !seen[key] && action.Groups[0].String() == key {
			view.addEntry(DashboardGroup{}, action)
		}
	}

	view.sort()

	return view
}

// addEntry files a group under the section its action names.
func (v *DashboardView) addEntry(g DashboardGroup, action DashboardAction) {
	entry := DashboardEntry{Action: action, Reports: g.Reports}

	switch action.Kind {
	case ActionIgnore:
		v.Ignored = append(v.Ignored, entry)
	case ActionPromote, ActionMerge:
		if regressed(entry) {
			v.Regressed = append(v.Regressed, entry)
		} else {
			v.Promoted = append(v.Promoted, entry)
		}
	case ActionCurate:
		// A request for a Curator run names no group, so it draws in no section.
	}
}

// FindGroup returns the group a command names, with the writers' merges applied. A group no report
// mentions any more is not found.
func FindGroup(reports []DashboardReport, state DashboardState, id GroupID) (DashboardGroup, bool) {
	want := resolveGroup(state.Actions, id)

	for _, g := range groupReports(mergedReports(reports, state.Actions)) {
		if g.ID == want {
			return g, true
		}
	}

	return DashboardGroup{}, false
}

// mergedReports folds every group a writer merged into the group it was merged into, so a merged group
// is reported, counted and promoted as one.
func mergedReports(reports []DashboardReport, actions []DashboardAction) []DashboardReport {
	alias := mergeAliases(actions)

	out := make([]DashboardReport, 0, len(reports))

	for _, r := range reports {
		id := follow(alias, GroupID{Target: r.Target, Key: r.Group})
		r.Target, r.Group = id.Target, id.Key
		out = append(out, r)
	}

	return out
}

// mergeAliases maps each merged group to the group it was merged into, in the order the writers recorded
// the merges: [A, B] folds B into A, and a later [B, C] then folds C into A as well.
func mergeAliases(actions []DashboardAction) map[string]GroupID {
	alias := map[string]GroupID{}

	for _, a := range actions {
		if a.Kind != ActionMerge || len(a.Groups) != 2 {
			continue
		}

		from, into := follow(alias, a.Groups[1]), follow(alias, a.Groups[0])
		if from.String() != into.String() {
			alias[from.String()] = into
		}
	}

	return alias
}

// follow walks the merge aliases to the group no merge folded.
func follow(alias map[string]GroupID, id GroupID) GroupID {
	for range len(alias) + 1 {
		next, ok := alias[id.String()]
		if !ok {
			return id
		}

		id = next
	}

	return id
}

// resolveGroup is the group a merge left, when a command names one.
func resolveGroup(actions []DashboardAction, id GroupID) GroupID {
	return follow(mergeAliases(actions), id)
}

// sort puts every section in a stable, useful order and bounds how many groups it shows.
func (v *DashboardView) sort() {
	slices.SortStableFunc(v.Ready, func(a, b DashboardGroup) int {
		return cmp.Or(
			cmp.Compare(len(b.issues()), len(a.issues())),
			cmp.Compare(a.ID.String(), b.ID.String()),
		)
	})
	v.Ready = bounded(v.Ready)

	slices.SortStableFunc(v.Watching, func(a, b DashboardGroup) int {
		return cmp.Or(
			b.lastAt().Compare(a.lastAt()),
			cmp.Compare(a.ID.String(), b.ID.String()),
		)
	})
	v.Watching = bounded(v.Watching)

	for _, section := range []*[]DashboardEntry{&v.Promoted, &v.Ignored, &v.Regressed} {
		slices.SortStableFunc(*section, func(a, b DashboardEntry) int {
			return cmp.Or(
				b.Action.At.Compare(a.Action.At),
				cmp.Compare(a.ID().String(), b.ID().String()),
			)
		})

		*section = (*section)[:min(len(*section), MaxDashboardGroups)]
	}
}

// bounded keeps at most MaxDashboardGroups groups.
func bounded(groups []DashboardGroup) []DashboardGroup {
	return groups[:min(len(groups), MaxDashboardGroups)]
}

// groupReports collects the reports by target and group key, each group's reports newest first.
func groupReports(reports []DashboardReport) []DashboardGroup {
	byID := map[string]*DashboardGroup{}

	for _, r := range reports {
		id := GroupID{Target: r.Target, Key: r.Group}

		g, ok := byID[id.String()]
		if !ok {
			g = &DashboardGroup{ID: id}
			byID[id.String()] = g
		}

		g.Reports = append(g.Reports, r)
	}

	groups := make([]DashboardGroup, 0, len(byID))
	for g := range maps.Values(byID) {
		slices.SortStableFunc(g.Reports, func(a, b DashboardReport) int {
			return b.At.Compare(a.At)
		})
		groups = append(groups, *g)
	}

	return groups
}

// lastActions keeps the last action per group, in the order the actions were recorded.
func lastActions(actions []DashboardAction) map[string]DashboardAction {
	out := make(map[string]DashboardAction, len(actions))

	for _, a := range actions {
		for _, group := range a.Groups {
			out[group.String()] = a
		}
	}

	return out
}

// liveProposals keeps the proposals no writer has rejected for good, by id or by the pair of groups
// they name: a Curator that proposes a rejected pair again under a new id still shows nothing.
func liveProposals(state DashboardState) []DashboardProposal {
	out := make([]DashboardProposal, 0, len(state.Proposals))

	for _, p := range state.Proposals {
		if proposalRejected(state.Rejected, p) {
			continue
		}

		out = append(out, p)
	}

	return out
}

// proposalRejected reports whether a rejection refuses p.
func proposalRejected(rejections []DashboardRejection, p DashboardProposal) bool {
	return slices.ContainsFunc(rejections, func(r DashboardRejection) bool {
		return r.ID == p.ID || sameGroups(r.Groups, p.Groups)
	})
}

// liveLinks keeps the links whose groups no writer merged away.
func liveLinks(state DashboardState) []DashboardLink {
	merged := mergeAliases(state.Actions)

	links := make([]DashboardLink, 0, len(state.Links))

	for _, l := range state.Links {
		if slices.ContainsFunc(l.Groups, func(g GroupID) bool {
			return merged[g.String()] != GroupID{}
		}) {
			continue
		}

		links = append(links, l)
	}

	if len(links) == 0 {
		return nil
	}

	return links
}

// sameGroups reports whether two non-empty sets name the same groups, in any order.
func sameGroups(a, b []GroupID) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}

	seen := make(map[string]bool, len(a))
	for _, g := range a {
		seen[g.String()] = true
	}

	return !slices.ContainsFunc(b, func(g GroupID) bool { return !seen[g.String()] })
}

// groupReady reports whether the group has reports from ReadyIssues different issues, or one
// high-severity report. Two agent roles on one issue count once.
func groupReady(reports []DashboardReport) bool {
	issues := map[int]bool{}

	for _, r := range reports {
		if r.Severity == SeverityHigh {
			return true
		}

		issues[r.Issue] = true
	}

	return len(issues) >= ReadyIssues
}

// watching reports whether the group is still inside the Watching window.
func watching(g DashboardGroup, now time.Time) bool {
	return g.lastAt().Add(WatchingWindow).After(now)
}

// regressed reports whether a report arrived after the promotion it was supposed to fix.
func regressed(e DashboardEntry) bool {
	if e.Action.Kind != ActionPromote && e.Action.Kind != ActionMerge {
		return false
	}

	return slices.ContainsFunc(e.Reports, func(r DashboardReport) bool { return r.At.After(e.Action.At) })
}

// issues lists the different issues that reported the group, ascending.
func (g DashboardGroup) issues() []int {
	seen := map[int]bool{}

	for _, r := range g.Reports {
		seen[r.Issue] = true
	}

	return slices.Sorted(maps.Keys(seen))
}

// lastAt is when the group was last reported.
func (g DashboardGroup) lastAt() time.Time {
	if len(g.Reports) == 0 {
		return time.Time{}
	}

	return g.Reports[0].At
}

// Dashboard markers.
const (
	// DashboardMarker starts every dashboard body.
	DashboardMarker = MarkerPrefix + "dashboard -->"
	// dashboardJSONOpen starts the machine-readable state inside the body.
	dashboardJSONOpen = MarkerPrefix + "dashboard-json\n"
	// dashboardJSONClose ends it.
	dashboardJSONClose = "\n-->"
	// dashboardVersion is the only version of the state block foreman accepts.
	dashboardVersion = 1
)

// dashboardWire is the state block as it is written and read back.
type dashboardWire struct {
	Version   int                        `json:"version"`
	Actions   []dashboardActionWire      `json:"actions"`
	Proposals []dashboardProposalWire    `json:"proposals"`
	Links     []dashboardLinkWire        `json:"links,omitempty"`
	Rejected  []dashboardRejectionWire   `json:"rejected,omitempty"`
	Published []dashboardPublicationWire `json:"published,omitempty"`
	Curator   *dashboardCuratorWire      `json:"curator,omitempty"`
	Handled   []int64                    `json:"handled,omitempty"`
}

// dashboardCuratorWire is the Curator's run record on the wire.
type dashboardCuratorWire struct {
	LastRun string `json:"last_run,omitempty"`
	Claim   string `json:"claim,omitempty"`
}

// dashboardLinkWire is one Curator link on the wire.
type dashboardLinkWire struct {
	ID     string        `json:"id"`
	Groups []groupIDWire `json:"groups"`
	Reason string        `json:"reason"`
}

// dashboardRejectionWire is one rejected proposal on the wire.
type dashboardRejectionWire struct {
	ID     string        `json:"id"`
	Groups []groupIDWire `json:"groups"`
	By     string        `json:"by"`
	At     string        `json:"at"`
	URL    string        `json:"url,omitempty"`
}

// dashboardPublicationWire is one published lesson on the wire.
type dashboardPublicationWire struct {
	Group groupIDWire `json:"group"`
	By    string      `json:"by"`
	At    string      `json:"at"`
	URL   string      `json:"url,omitempty"`
}

// dashboardActionWire is one writer action on the wire.
type dashboardActionWire struct {
	Kind   DashboardActionKind `json:"kind"`
	Groups []groupIDWire       `json:"groups"`
	By     string              `json:"by"`
	At     string              `json:"at"`
	URL    string              `json:"url,omitempty"`
}

// dashboardProposalWire is one Curator proposal on the wire.
type dashboardProposalWire struct {
	ID     string        `json:"id"`
	Groups []groupIDWire `json:"groups"`
	Reason string        `json:"reason"`
}

// groupIDWire is a group id on the wire.
type groupIDWire struct {
	Target string `json:"target"`
	Group  string `json:"group"`
}

// RenderDashboard draws every section and ends with the state block. It is deterministic: the same
// view renders the same body, so a poll can compare before it writes.
func RenderDashboard(view DashboardView) (string, error) {
	raw, err := marshalDashboardState(view.State)
	if err != nil {
		return "", err
	}

	var b strings.Builder

	b.WriteString(DashboardMarker)
	b.WriteString("\n\n# " + DashboardTitle + "\n\n")
	b.WriteString("foreman redraws this body from the retro log and writes it only when it changes. " +
		"It never comments here.\n")

	renderReady(&b, view.Ready)
	renderProposals(&b, view.Proposals, view.Links)
	renderWatching(&b, view.Watching)
	renderEntries(&b, "Promoted", view.Promoted, view.Published)
	renderEntries(&b, "Ignored", view.Ignored, nil)
	renderRegressed(&b, view.Regressed)

	fmt.Fprintf(&b, "\n%s%s%s", dashboardJSONOpen, raw, dashboardJSONClose)

	if n := utf8.RuneCountInString(b.String()); n > DashboardBodyLimit {
		return "", fmt.Errorf("dashboard body is %d characters, over the %d limit", n, DashboardBodyLimit)
	}

	return b.String(), nil
}

// renderReady draws the Ready to promote section, naming the command that promotes each group.
func renderReady(b *strings.Builder, groups []DashboardGroup) {
	fmt.Fprintf(b, "\n## Ready to promote (%d)\n", len(groups))

	b.WriteString("\nComment `promote <target>/<key>` to file a group, `merge <target>/<key> <target>/<key>` " +
		"to merge two, `ignore <target>/<key>` to drop one, `reject <id>` to refuse a proposal, or " +
		"`publish` on a private repository's promoted issue. Only a writer's command counts.\n")

	if len(groups) == 0 {
		b.WriteString("\n_None._\n")

		return
	}

	for _, g := range groups {
		renderGroup(b, g)
		fmt.Fprintf(b, "\nPromote with `promote %s`.\n", g.ID)
	}
}

// renderWatching draws the Watching section, collapsed.
func renderWatching(b *strings.Builder, groups []DashboardGroup) {
	fmt.Fprintf(b, "\n## Watching (%d)\n", len(groups))

	if len(groups) == 0 {
		b.WriteString("\n_None._\n")

		return
	}

	noun := "groups are"
	if len(groups) == 1 {
		noun = "group is"
	}

	fmt.Fprintf(b, "\n<details>\n<summary>%d %s not ready yet</summary>\n", len(groups), noun)

	for _, g := range groups {
		renderGroup(b, g)
	}

	b.WriteString("\n</details>\n")
}

// renderGroup draws one group: its heading, how its reports vary and each report's provenance.
func renderGroup(b *strings.Builder, g DashboardGroup) {
	issues, claims := len(g.issues()), distinctClaims(g.Reports)

	fmt.Fprintf(b, "\n### `%s` / `%s`\n\n", g.ID.Target, g.ID.Key)
	fmt.Fprintf(b, "%d %s from %d %s; %s; %d different %s.\n",
		len(g.Reports), plural(len(g.Reports), "report"), issues, plural(issues, "issue"),
		severitySpread(g.Reports), claims, plural(claims, "claim"))

	for _, r := range g.Reports[:min(len(g.Reports), MaxGroupReports)] {
		fmt.Fprintf(b, "\n- %s run `%s` on #%d (%s): %s",
			r.Role, r.Key, r.Issue, r.Severity, Truncate(r.Claim, dashboardClaimChars))
	}

	b.WriteString("\n")
}

// plural adds an s unless n is one.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}

	return word + "s"
}

// severitySpread describes how the reports' severities vary, most severe first.
func severitySpread(reports []DashboardReport) string {
	parts := make([]string, 0, len(Severities()))

	for _, severity := range Severities() {
		n := 0

		for _, r := range reports {
			if r.Severity == severity {
				n++
			}
		}

		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, severity))
		}
	}

	return "severities: " + strings.Join(parts, ", ")
}

// distinctClaims counts how many different claims the reports make.
func distinctClaims(reports []DashboardReport) int {
	claims := map[string]bool{}

	for _, r := range reports {
		claims[r.Claim] = true
	}

	return len(claims)
}

// renderProposals draws the Proposed merges section, and the links the Curator found beneath it.
func renderProposals(b *strings.Builder, proposals []DashboardProposal, links []DashboardLink) {
	fmt.Fprintf(b, "\n## Proposed merges (%d)\n", len(proposals))

	if len(proposals) == 0 {
		b.WriteString("\n_None._\n")
	} else {
		for _, p := range proposals {
			groups := make([]string, 0, len(p.Groups))
			for _, g := range p.Groups {
				groups = append(groups, "`"+g.String()+"`")
			}

			fmt.Fprintf(b, "\n- `%s`: merge %s — %s", p.ID, strings.Join(groups, " and "),
				Truncate(p.Reason, dashboardClaimChars))
		}

		b.WriteString("\n")
	}

	if len(links) == 0 {
		return
	}

	fmt.Fprintf(b, "\n### Links (%d)\n\nA link needs no command; it says two groups are related.\n", len(links))

	for _, l := range links {
		groups := make([]string, 0, len(l.Groups))
		for _, g := range l.Groups {
			groups = append(groups, "`"+g.String()+"`")
		}

		fmt.Fprintf(b, "\n- `%s`: %s — %s", l.ID, strings.Join(groups, " and "),
			Truncate(l.Reason, dashboardClaimChars))
	}

	b.WriteString("\n")
}

// renderEntries draws a Promoted or Ignored section. A promoted group a writer published shows where it
// went.
func renderEntries(b *strings.Builder, title string, entries []DashboardEntry, published []DashboardPublication) {
	fmt.Fprintf(b, "\n## %s (%d)\n", title, len(entries))

	if len(entries) == 0 {
		b.WriteString("\n_None._\n")

		return
	}

	for _, e := range entries {
		fmt.Fprintf(b, "\n- `%s` %s by %s on %s%s", e.ID(), actionVerb(e.Action.Kind), e.Action.By,
			e.Action.At.Format(dashboardDate), linkSuffix(e.Action.URL))

		if p, ok := lastPublication(published, e.ID()); ok {
			fmt.Fprintf(b, "; published by %s on %s%s", p.By, p.At.Format(dashboardDate), linkSuffix(p.URL))
		}
	}

	b.WriteString("\n")
}

// actionVerb is how a writer's action reads in the body: a group is promoted, merged or ignored.
func actionVerb(kind DashboardActionKind) string {
	switch kind {
	case ActionMerge:
		return "merged"
	case ActionIgnore:
		return "ignored"
	case ActionPromote:
		return "promoted"
	case ActionCurate:
		return "curated"
	}

	return "promoted"
}

// lastPublication is the newest publication of the group, if a writer published one.
func lastPublication(published []DashboardPublication, id GroupID) (DashboardPublication, bool) {
	var found DashboardPublication

	ok := false

	for _, p := range published {
		if p.Group == id && (!ok || p.At.After(found.At)) {
			found, ok = p, true
		}
	}

	return found, ok
}

// renderRegressed draws the Regressed section.
func renderRegressed(b *strings.Builder, entries []DashboardEntry) {
	fmt.Fprintf(b, "\n## Regressed (%d)\n", len(entries))

	if len(entries) == 0 {
		b.WriteString("\n_None._\n")

		return
	}

	for _, e := range entries {
		latest := e.Reports[0]
		fmt.Fprintf(b, "\n- `%s` promoted by %s on %s, then reported again on %s (#%d %s run `%s`)%s",
			e.ID(), e.Action.By, e.Action.At.Format(dashboardDate), latest.At.Format(dashboardDate),
			latest.Issue, latest.Role, latest.Key, linkSuffix(e.Action.URL))
	}

	b.WriteString("\n")
}

// linkSuffix renders a link to the action's record, or nothing.
func linkSuffix(url string) string {
	if url == "" {
		return ""
	}

	return " ([record](" + url + "))"
}

// ParseDashboard reads the state block out of a dashboard body. An empty body is an empty state: a
// body titled like the dashboard but never drawn yet can be taken over. Any other body without a
// readable block is an error, so foreman never overwrites state it could not read.
func ParseDashboard(body string) (DashboardState, error) {
	if strings.TrimSpace(body) == "" {
		return DashboardState{}, nil
	}

	_, after, ok := strings.Cut(body, dashboardJSONOpen)
	if !ok {
		return DashboardState{}, errors.New("dashboard body has no state block")
	}

	raw, _, ok := strings.Cut(after, dashboardJSONClose)
	if !ok {
		return DashboardState{}, errors.New("dashboard state block is not closed")
	}

	return parseDashboardState(raw)
}

// parseDashboardState reads the state block strictly.
func parseDashboardState(text string) (DashboardState, error) {
	var wire dashboardWire

	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&wire); err != nil {
		return DashboardState{}, fmt.Errorf("dashboard state is not valid: %w", err)
	}

	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return DashboardState{}, errors.New("dashboard state is not valid: trailing data")
	}

	if wire.Version != dashboardVersion {
		return DashboardState{}, fmt.Errorf("dashboard state needs version %d", dashboardVersion)
	}

	return dashboardStateOf(wire)
}

// dashboardStateOf validates a decoded state block's parts.
func dashboardStateOf(wire dashboardWire) (DashboardState, error) {
	actions, err := parseDashboardActions(wire.Actions)
	if err != nil {
		return DashboardState{}, err
	}

	proposals, err := parseDashboardProposals(wire.Proposals)
	if err != nil {
		return DashboardState{}, err
	}

	extras, err := parseDashboardExtras(wire)
	if err != nil {
		return DashboardState{}, err
	}

	handled, err := parseDashboardHandled(wire.Handled)
	if err != nil {
		return DashboardState{}, err
	}

	return DashboardState{
		Actions: actions, Proposals: proposals, Links: extras.links, Rejected: extras.rejected,
		Published: extras.published, Curator: extras.curator, Handled: handled,
	}, nil
}

// dashboardExtras are a state block's optional parts.
type dashboardExtras struct {
	links     []DashboardLink
	rejected  []DashboardRejection
	published []DashboardPublication
	curator   DashboardCurator
}

// parseDashboardExtras validates the links, rejections, publications and Curator record.
func parseDashboardExtras(wire dashboardWire) (dashboardExtras, error) {
	links, err := parseDashboardLinks(wire.Links)
	if err != nil {
		return dashboardExtras{}, err
	}

	rejected, err := parseDashboardRejections(wire.Rejected)
	if err != nil {
		return dashboardExtras{}, err
	}

	published, err := parseDashboardPublications(wire.Published)
	if err != nil {
		return dashboardExtras{}, err
	}

	curator, err := parseDashboardCurator(wire.Curator)

	return dashboardExtras{
		links: links, rejected: rejected, published: published, curator: curator,
	}, err
}

// parseDashboardActions validates every recorded action.
func parseDashboardActions(wires []dashboardActionWire) ([]DashboardAction, error) {
	if len(wires) == 0 {
		return nil, nil
	}

	actions := make([]DashboardAction, 0, len(wires))

	for _, w := range wires {
		action, err := parseDashboardAction(w)
		if err != nil {
			return nil, err
		}

		actions = append(actions, action)
	}

	return actions, nil
}

// parseDashboardAction validates one recorded action.
func parseDashboardAction(w dashboardActionWire) (DashboardAction, error) {
	want, ok := actionGroupCount(w.Kind)
	if !ok {
		return DashboardAction{}, fmt.Errorf("unknown dashboard action %q", w.Kind)
	}

	groups, err := parseGroupIDs(w.Groups)
	if err != nil {
		return DashboardAction{}, err
	}

	if len(groups) != want || len(w.Groups) != want {
		return DashboardAction{}, fmt.Errorf("a %s action names %d groups, want %d",
			w.Kind, len(w.Groups), want)
	}

	if err := validDashboardText("action author", w.By); err != nil {
		return DashboardAction{}, err
	}

	if w.URL != "" {
		if err := validDashboardText("action link", w.URL); err != nil {
			return DashboardAction{}, err
		}
	}

	at, err := time.Parse(time.RFC3339, w.At)
	if err != nil {
		return DashboardAction{}, fmt.Errorf("dashboard action date %q is not valid: %w", w.At, err)
	}

	return DashboardAction{Kind: w.Kind, Groups: groups, By: w.By, At: at, URL: w.URL}, nil
}

// actionGroupCount is how many groups an action names, or false for an unknown action.
func actionGroupCount(kind DashboardActionKind) (int, bool) {
	switch kind {
	case ActionCurate:
		return 0, true
	case ActionIgnore, ActionPromote:
		return 1, true
	case ActionMerge:
		return mergeGroups, true
	}

	return 0, false
}

// parseDashboardProposals validates every Curator proposal.
func parseDashboardProposals(wires []dashboardProposalWire) ([]DashboardProposal, error) {
	if len(wires) == 0 {
		return nil, nil
	}

	proposals := make([]DashboardProposal, 0, len(wires))

	for _, w := range wires {
		if !retroNameRE.MatchString(w.ID) {
			return nil, fmt.Errorf("dashboard proposal id %q is not valid", w.ID)
		}

		groups, err := parseGroupIDs(w.Groups)
		if err != nil {
			return nil, err
		}

		if len(groups) < ReadyIssues {
			return nil, errors.New("a dashboard proposal merges fewer than two groups")
		}

		if err := validDashboardText("proposal reason", w.Reason); err != nil {
			return nil, err
		}

		proposals = append(proposals, DashboardProposal{ID: w.ID, Groups: groups, Reason: w.Reason})
	}

	return proposals, nil
}

// parseGroupIDs validates every group id.
func parseGroupIDs(wires []groupIDWire) ([]GroupID, error) {
	if len(wires) == 0 {
		return nil, nil
	}

	groups := make([]GroupID, 0, len(wires))

	for _, w := range wires {
		target, err := ParseRetroTarget(w.Target)
		if err != nil {
			return nil, err
		}

		if !retroNameRE.MatchString(w.Group) {
			return nil, fmt.Errorf("dashboard group key %q is not valid", w.Group)
		}

		groups = append(groups, GroupID{Target: target, Key: w.Group})
	}

	return groups, nil
}

// parseDashboardLinks validates every Curator link. A link names two groups of different targets: it
// relates them without merging them.
func parseDashboardLinks(wires []dashboardLinkWire) ([]DashboardLink, error) {
	if len(wires) == 0 {
		return nil, nil
	}

	links := make([]DashboardLink, 0, len(wires))

	for _, w := range wires {
		if !retroNameRE.MatchString(w.ID) {
			return nil, fmt.Errorf("dashboard link id %q is not valid", w.ID)
		}

		groups, err := parseGroupIDs(w.Groups)
		if err != nil {
			return nil, err
		}

		if len(groups) != 2 || len(w.Groups) != 2 {
			return nil, errors.New("a dashboard link names exactly two groups")
		}

		if groups[0].Target == groups[1].Target {
			return nil, errors.New("a dashboard link must cross targets")
		}

		if err := validDashboardText("link reason", w.Reason); err != nil {
			return nil, err
		}

		links = append(links, DashboardLink{ID: w.ID, Groups: groups, Reason: w.Reason})
	}

	return links, nil
}

// parseDashboardCurator reads the Curator's run record, if the state has one.
func parseDashboardCurator(w *dashboardCuratorWire) (DashboardCurator, error) {
	if w == nil {
		return DashboardCurator{}, nil
	}

	last, err := parseOptionalDashboardTime("curator last run", w.LastRun)
	if err != nil {
		return DashboardCurator{}, err
	}

	claim, err := parseOptionalDashboardTime("curator claim", w.Claim)
	if err != nil {
		return DashboardCurator{}, err
	}

	return DashboardCurator{LastRun: last, Claim: claim}, nil
}

// parseOptionalDashboardTime reads an absent date as the zero time, or a malformed one as an error.
func parseOptionalDashboardTime(field, text string) (time.Time, error) {
	if text == "" {
		return time.Time{}, nil
	}

	return parseDashboardTime(field, text)
}

// parseDashboardRejections validates every rejected proposal.
func parseDashboardRejections(wires []dashboardRejectionWire) ([]DashboardRejection, error) {
	if len(wires) == 0 {
		return nil, nil
	}

	rejected := make([]DashboardRejection, 0, len(wires))

	for _, w := range wires {
		if !retroNameRE.MatchString(w.ID) {
			return nil, fmt.Errorf("dashboard rejection id %q is not valid", w.ID)
		}

		groups, err := parseGroupIDs(w.Groups)
		if err != nil {
			return nil, err
		}

		if len(groups) < ReadyIssues {
			return nil, errors.New("a dashboard rejection names fewer than two groups")
		}

		at, err := parseDashboardTime("rejection", w.At)
		if err != nil {
			return nil, err
		}

		if err := validDashboardText("rejection author", w.By); err != nil {
			return nil, err
		}

		if err := validDashboardLink(w.URL); err != nil {
			return nil, err
		}

		rejected = append(rejected, DashboardRejection{ID: w.ID, Groups: groups, By: w.By, At: at, URL: w.URL})
	}

	return rejected, nil
}

// parseDashboardPublications validates every published lesson.
func parseDashboardPublications(wires []dashboardPublicationWire) ([]DashboardPublication, error) {
	if len(wires) == 0 {
		return nil, nil
	}

	published := make([]DashboardPublication, 0, len(wires))

	for _, w := range wires {
		groups, err := parseGroupIDs([]groupIDWire{w.Group})
		if err != nil {
			return nil, err
		}

		at, err := parseDashboardTime("publication", w.At)
		if err != nil {
			return nil, err
		}

		if err := validDashboardText("publication author", w.By); err != nil {
			return nil, err
		}

		if err := validDashboardLink(w.URL); err != nil {
			return nil, err
		}

		published = append(published, DashboardPublication{Group: groups[0], By: w.By, At: at, URL: w.URL})
	}

	return published, nil
}

// parseDashboardHandled validates the ids of the command comments already dealt with.
func parseDashboardHandled(ids []int64) ([]int64, error) {
	seen := make(map[int64]bool, len(ids))
	handled := make([]int64, 0, len(ids))

	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("dashboard handled comment id %d is not valid", id)
		}

		if seen[id] {
			return nil, fmt.Errorf("dashboard handled comment id %d is repeated", id)
		}

		seen[id] = true
		handled = append(handled, id)
	}

	if len(handled) == 0 {
		return nil, nil
	}

	return handled, nil
}

// parseDashboardTime reads an RFC 3339 date in the state block.
func parseDashboardTime(field, text string) (time.Time, error) {
	at, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}, fmt.Errorf("dashboard %s date %q is not valid: %w", field, text, err)
	}

	return at, nil
}

// validDashboardLink accepts an absent link or one line of text.
func validDashboardLink(url string) error {
	if url == "" {
		return nil
	}

	return validDashboardText("link", url)
}

// validDashboardText rejects empty or multi-line text in the state block.
func validDashboardText(field, text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("dashboard %s is empty", field)
	}

	if strings.Contains(text, MarkerPrefix) || strings.ContainsAny(text, "\r\n") {
		return fmt.Errorf("dashboard %s must be one line with no marker", field)
	}

	return nil
}

// marshalDashboardState encodes the state block, refusing anything ParseDashboard would refuse.
func marshalDashboardState(state DashboardState) (string, error) {
	wire := dashboardWire{Version: dashboardVersion}

	for _, a := range state.Actions {
		wire.Actions = append(wire.Actions, dashboardActionWire{
			Kind: a.Kind, Groups: groupIDWires(a.Groups), By: a.By,
			At: a.At.Format(time.RFC3339Nano), URL: a.URL,
		})
	}

	for _, p := range state.Proposals {
		wire.Proposals = append(wire.Proposals, dashboardProposalWire{
			ID: p.ID, Groups: groupIDWires(p.Groups), Reason: p.Reason,
		})
	}

	for _, r := range state.Rejected {
		wire.Rejected = append(wire.Rejected, dashboardRejectionWire{
			ID: r.ID, Groups: groupIDWires(r.Groups), By: r.By, At: r.At.Format(time.RFC3339Nano), URL: r.URL,
		})
	}

	for _, p := range state.Published {
		wire.Published = append(wire.Published, dashboardPublicationWire{
			Group: groupIDWire{Target: p.Group.Target.String(), Group: p.Group.Key},
			By:    p.By, At: p.At.Format(time.RFC3339Nano), URL: p.URL,
		})
	}

	for _, l := range state.Links {
		wire.Links = append(wire.Links, dashboardLinkWire{
			ID: l.ID, Groups: groupIDWires(l.Groups), Reason: l.Reason,
		})
	}

	wire.Curator = dashboardCuratorWireOf(state.Curator)
	wire.Handled = state.Handled

	raw, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("encode dashboard state: %w", err)
	}

	// Read back what was written, so a value that would not parse is refused here rather than lost.
	if _, err := parseDashboardState(string(raw)); err != nil {
		return "", err
	}

	return string(raw), nil
}

// dashboardCuratorWireOf encodes the Curator's run record, or nil when it has none.
func dashboardCuratorWireOf(c DashboardCurator) *dashboardCuratorWire {
	if c.LastRun.IsZero() && c.Claim.IsZero() {
		return nil
	}

	w := &dashboardCuratorWire{}
	if !c.LastRun.IsZero() {
		w.LastRun = c.LastRun.Format(time.RFC3339Nano)
	}

	if !c.Claim.IsZero() {
		w.Claim = c.Claim.Format(time.RFC3339Nano)
	}

	return w
}

// groupIDWires encodes group ids.
func groupIDWires(groups []GroupID) []groupIDWire {
	if len(groups) == 0 {
		return nil
	}

	out := make([]groupIDWire, 0, len(groups))
	for _, g := range groups {
		out = append(out, groupIDWire{Target: g.Target.String(), Group: g.Key})
	}

	return out
}
