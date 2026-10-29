package workflow

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// The Curator's bounds and limits.
const (
	// CuratorOutputFile is the one file a Curator run writes.
	CuratorOutputFile = "curator-output.json"
	// CuratorDay is how often a repository's dashboard may get a Curator run.
	CuratorDay = 24 * time.Hour
	// MaxCuratorProposals bounds the merges and links one run may propose.
	MaxCuratorProposals = 30
	// MaxCuratorIssues bounds the issues one run may draft.
	MaxCuratorIssues = MaxDashboardGroups
	// MaxCuratorTitle bounds a drafted issue's title, within GitHub's own limit.
	MaxCuratorTitle = 256
	// CuratorBriefLimit bounds the retro log and dashboard state put into the Curator's prompt.
	CuratorBriefLimit = CommentLimitChars
	// curatorEvidenceChars bounds one report's evidence in the brief.
	curatorEvidenceChars = 1_000
)

// CuratorIssueDraft is the issue one ready group becomes. Body is the public draft's text; a private
// repository's draft carries Lesson and PrivateEvidence instead, so only the lesson can be published.
type CuratorIssueDraft struct {
	// Group is the ready group the draft promotes.
	Group GroupID
	// Title is the issue's title.
	Title string
	// Body is the public draft's body.
	Body string
	// Lesson is a private draft's general lesson.
	Lesson string
	// PrivateEvidence is a private draft's evidence, which never leaves the repository.
	PrivateEvidence string
}

// CuratorOutput is curator-output.json.
type CuratorOutput struct {
	// Merges are the merge proposals, each within one target.
	Merges []GroupRelation
	// Links are the link proposals, which may cross targets.
	Links []GroupRelation
	// Issues is one issue draft per group to promote.
	Issues []CuratorIssueDraft
}

// curatorWire is curator-output.json as the model writes it.
type curatorWire struct {
	Version int                    `json:"version"`
	Merges  *[]curatorProposalWire `json:"merges"`
	Links   *[]curatorProposalWire `json:"links"`
	Issues  *[]curatorIssueWire    `json:"issues"`
}

// curatorProposalWire is one merge or link on the wire.
type curatorProposalWire struct {
	ID     string   `json:"id"`
	Groups []string `json:"groups"`
	Reason string   `json:"reason"`
}

// curatorIssueWire is one issue draft on the wire.
type curatorIssueWire struct {
	Group           string `json:"group"`
	Title           string `json:"title"`
	Body            string `json:"body"`
	Lesson          string `json:"lesson"`
	PrivateEvidence string `json:"private_evidence"`
}

// ParseCuratorOutput reads curator-output.json strictly, all or nothing. It parses each part once,
// collecting values and problems together, so every problem is named and no value is built on the
// say-so of an earlier check.
func ParseCuratorOutput(text string) (CuratorOutput, error) {
	var wire curatorWire

	problems := decodeStrict(text, &wire)
	out, more := curatorOutputOf(wire)
	problems = append(problems, more...)

	if len(problems) > 0 {
		return CuratorOutput{}, &OutputError{File: CuratorOutputFile, Problems: problems}
	}

	return out, nil
}

// curatorOutputOf parses every part of a decoded output file, returning what parsed and every
// problem found. The values are only meant to be used when there are no problems.
func curatorOutputOf(w curatorWire) (CuratorOutput, []string) {
	var problems []string

	if w.Merges == nil {
		problems = append(problems, "merges is required")
	}

	if w.Links == nil {
		problems = append(problems, "links is required")
	}

	if w.Issues == nil {
		problems = append(problems, "issues is required")
	}

	seenIDs := map[string]bool{}
	merges, mergeProblems := curatorRelations(w.Merges, true, seenIDs)
	links, linkProblems := curatorRelations(w.Links, false, seenIDs)

	problems = append(problems, mergeProblems...)
	problems = append(problems, linkProblems...)

	if n := len(merges) + len(links); n > MaxCuratorProposals {
		problems = append(problems, fmt.Sprintf("merges and links name %d proposals, at most %d are allowed",
			n, MaxCuratorProposals))
	}

	issues, issueProblems := curatorIssues(w.Issues)

	return CuratorOutput{Merges: merges, Links: links, Issues: issues}, append(problems, issueProblems...)
}

// curatorRelations parses merge or link proposals, keeping the ones that parse. A merge stays within
// one target, a link crosses targets, and every id is unique across both lists.
func curatorRelations(
	wires *[]curatorProposalWire, merge bool, seenIDs map[string]bool,
) ([]GroupRelation, []string) {
	relations := []GroupRelation{}

	if wires == nil {
		return relations, nil
	}

	var problems []string

	for i, w := range *wires {
		relation, relationProblems := curatorRelation(w, proposalLabel(merge, i), merge, seenIDs)
		problems = append(problems, relationProblems...)

		if len(relationProblems) == 0 {
			relations = append(relations, relation)
		}
	}

	return relations, problems
}

// proposalLabel names a proposal in an error: merges[0] or links[1].
func proposalLabel(merge bool, index int) string {
	if merge {
		return fmt.Sprintf("merges[%d]", index)
	}

	return fmt.Sprintf("links[%d]", index)
}

// curatorRelation parses one merge or link, returning it with every problem found.
func curatorRelation(
	w curatorProposalWire, at string, merge bool, seenIDs map[string]bool,
) (GroupRelation, []string) {
	problems := curatorProposalIDProblems(w, at, seenIDs)

	groups, err := groupIDsOf(w.Groups)
	if err != nil {
		return GroupRelation{}, append(problems, at+".groups: "+err.Error())
	}

	if len(groups) != mergeGroups {
		return GroupRelation{}, append(problems, at+".groups must name exactly two groups")
	}

	problems = append(problems, curatorProposalGroupProblems(groups, at, merge)...)

	if err := validText(at+".reason", w.Reason, true); err != nil {
		problems = append(problems, err.Error())
	}

	return GroupRelation{ID: w.ID, Groups: groups, Reason: w.Reason}, problems
}

// curatorProposalIDProblems checks a proposal's id is a key nobody else claimed.
func curatorProposalIDProblems(w curatorProposalWire, at string, seenIDs map[string]bool) []string {
	var problems []string

	if !retroNameRE.MatchString(w.ID) {
		problems = append(problems, at+".id is not a valid key")
	} else if seenIDs[w.ID] {
		problems = append(problems, at+".id repeats another proposal's id")
	}

	seenIDs[w.ID] = true

	return problems
}

// curatorProposalGroupProblems checks the target rule: a merge stays within one target, a link
// crosses targets, and neither names one group twice.
func curatorProposalGroupProblems(groups []GroupID, at string, merge bool) []string {
	var problems []string

	if groups[0] == groups[1] {
		problems = append(problems, at+".groups names one group twice")
	}

	switch {
	case merge && groups[0].Target != groups[1].Target:
		problems = append(problems, at+" merges groups of different targets")
	case !merge && groups[0].Target == groups[1].Target:
		problems = append(problems, at+" links groups of one target; a link must cross targets")
	}

	return problems
}

// curatorIssues parses every issue draft: one per ready group, with a title and exactly one body
// shape. It keeps the drafts that parse.
func curatorIssues(wires *[]curatorIssueWire) ([]CuratorIssueDraft, []string) {
	drafts := []CuratorIssueDraft{}

	if wires == nil {
		return drafts, nil
	}

	if len(*wires) > MaxCuratorIssues {
		return drafts, []string{fmt.Sprintf("issues names %d drafts, at most %d are allowed",
			len(*wires), MaxCuratorIssues)}
	}

	seen := map[string]bool{}

	var problems []string

	for i, w := range *wires {
		draft, draftProblems := curatorIssue(fmt.Sprintf("issues[%d]", i), w, seen)
		problems = append(problems, draftProblems...)

		if len(draftProblems) == 0 {
			drafts = append(drafts, draft)
		}
	}

	return drafts, problems
}

// curatorIssue parses one issue draft, returning it with every problem found.
func curatorIssue(at string, w curatorIssueWire, seen map[string]bool) (CuratorIssueDraft, []string) {
	var problems []string

	group, err := ParseGroupID(w.Group)
	if err != nil {
		problems = append(problems, at+".group: "+err.Error())
	} else if seen[w.Group] {
		problems = append(problems, at+".group repeats another draft's group")
	}

	seen[w.Group] = true

	if err := validText(at+".title", w.Title, true); err != nil {
		problems = append(problems, err.Error())
	} else if utf8.RuneCountInString(w.Title) > MaxCuratorTitle {
		problems = append(problems, fmt.Sprintf("%s.title is over %d characters", at, MaxCuratorTitle))
	}

	problems = append(problems, curatorDraftProblems(at, w)...)

	return CuratorIssueDraft{
		Group: group, Title: w.Title, Body: w.Body, Lesson: w.Lesson, PrivateEvidence: w.PrivateEvidence,
	}, problems
}

// curatorDraftProblems checks the body shape of one draft: a public body or a private lesson, never
// both, and private evidence only beside a lesson.
func curatorDraftProblems(at string, w curatorIssueWire) []string {
	if w.Body != "" && w.Lesson != "" {
		return []string{at + " has both a public body and a private lesson"}
	}

	if w.Body == "" && w.Lesson == "" {
		return []string{at + " needs a body, or a lesson for a private repository"}
	}

	if w.PrivateEvidence != "" && w.Lesson == "" {
		return []string{at + ".private_evidence needs a lesson"}
	}

	return nil
}

// groupIDsOf parses a list of group ids, naming the first that is not valid.
func groupIDsOf(texts []string) ([]GroupID, error) {
	groups := make([]GroupID, 0, len(texts))

	for _, text := range texts {
		group, err := ParseGroupID(text)
		if err != nil {
			return nil, err
		}

		groups = append(groups, group)
	}

	return groups, nil
}

// CuratorPlan is what foreman applies from one curator-output.json.
type CuratorPlan struct {
	// State is the dashboard state with the accepted proposals and links.
	State DashboardState
	// Issues are the drafts foreman may file without a writer.
	Issues []CuratorIssueDraft
}

// PlanCurator drops what the Curator may not change and records what it may. A merge must stay
// within one target and never touch a group a writer merged; a rejected pair is never proposed
// again, whatever its id; an issue draft must name a ready group the Curator may promote on its own.
func PlanCurator(
	out CuratorOutput, reports []DashboardReport, state DashboardState, private bool,
) CuratorPlan {
	known := curatorKnownGroups(reports, state)
	merged := writerMergedGroups(state.Actions)
	ready := curatorReadyGroups(reports, state)

	plan := CuratorPlan{State: state}

	for _, m := range out.Merges {
		if !curatorMayTouch(m.Groups, known, merged) ||
			proposalRejected(state.Rejected, GroupRelation{Groups: m.Groups}) {
			continue
		}

		plan.State = plan.State.WithProposal(m)
	}

	for _, l := range out.Links {
		if !curatorMayTouch(l.Groups, known, merged) {
			continue
		}

		plan.State = plan.State.WithLink(l)
	}

	for _, d := range out.Issues {
		if writerMerged(d.Group, merged) || !ready[d.Group.String()] ||
			!CuratorPromotable(d.Group.Target, private) {
			continue
		}

		plan.Issues = append(plan.Issues, d)
	}

	return plan
}

// curatorMayTouch reports whether every group is one the dashboard knows and no writer merged.
func curatorMayTouch(groups []GroupID, known, merged map[string]bool) bool {
	return !slices.ContainsFunc(groups, func(g GroupID) bool {
		return !known[g.String()] || merged[g.String()]
	})
}

// writerMerged reports whether a writer merged the group away.
func writerMerged(g GroupID, merged map[string]bool) bool {
	return merged[g.String()]
}

// writerMergedGroups is every group a writer's merge named, on either side.
func writerMergedGroups(actions []DashboardAction) map[string]bool {
	merged := map[string]bool{}

	for _, a := range actions {
		if a.Kind != ActionMerge {
			continue
		}

		for _, g := range a.Groups {
			merged[g.String()] = true
		}
	}

	return merged
}

// curatorKnownGroups is every group the dashboard knows: one with reports, or one a writer acted on.
func curatorKnownGroups(reports []DashboardReport, state DashboardState) map[string]bool {
	known := map[string]bool{}

	for _, g := range groupReports(mergedReports(reports, state.Actions)) {
		known[g.ID.String()] = true
	}

	for _, a := range state.Actions {
		for _, g := range a.Groups {
			known[g.String()] = true
		}
	}

	return known
}

// curatorReadyGroups is every group ready to promote, with the writers' merges applied.
func curatorReadyGroups(reports []DashboardReport, state DashboardState) map[string]bool {
	ready := map[string]bool{}

	for _, g := range groupReports(mergedReports(reports, state.Actions)) {
		if groupReady(g.Reports) {
			ready[g.ID.String()] = true
		}
	}

	return ready
}

// CuratorPromotable reports whether the Curator promotes the group on its own: any document group, or
// a skill or factory group from a public repository. A private repository's skill or factory group
// needs a writer's promote, so only its lesson can ever be published.
func CuratorPromotable(target RetroTarget, private bool) bool {
	if target.Kind == TargetDocs {
		return true
	}

	return !private && (target.Kind == TargetSkill || target.Kind == TargetFactory)
}

// CuratorDue reports whether a repository's dashboard should start a Curator run now: a retro added
// findings, a writer asked with `curate`, or a group became ready. It is false within CuratorDay of
// the last run, and while a queued run holds its claim.
func CuratorDue(
	reports []DashboardReport, state DashboardState, now time.Time, claimTTL time.Duration,
) bool {
	if !curatorMayStart(state, now, claimTTL) {
		return false
	}

	return curatorNewReport(reports, state) || curatorMergedIntoReady(reports, state) ||
		curatorRequested(state)
}

// curatorMayStart reports whether the daily limit and the queued run's claim both allow a run.
func curatorMayStart(state DashboardState, now time.Time, claimTTL time.Duration) bool {
	if !state.Curator.Claim.IsZero() && now.Sub(state.Curator.Claim) < claimTTL {
		return false
	}

	return state.Curator.LastRun.IsZero() || now.Sub(state.Curator.LastRun) >= CuratorDay
}

// curatorNewReport reports whether a retro added a finding since the last run.
func curatorNewReport(reports []DashboardReport, state DashboardState) bool {
	return slices.ContainsFunc(reports, func(r DashboardReport) bool {
		return r.At.After(state.Curator.LastRun)
	})
}

// curatorMergedIntoReady reports whether a writer's merge made a group ready without a new report:
// the two groups' reports are one.
func curatorMergedIntoReady(reports []DashboardReport, state DashboardState) bool {
	ready := curatorReadyGroups(reports, state)

	return slices.ContainsFunc(state.Actions, func(a DashboardAction) bool {
		return a.Kind == ActionMerge && a.At.After(state.Curator.LastRun) &&
			len(a.Groups) > 0 && ready[a.Groups[0].String()]
	})
}

// curatorRequested reports whether a writer asked for a run with `curate`.
func curatorRequested(state DashboardState) bool {
	return slices.ContainsFunc(state.Actions, func(a DashboardAction) bool {
		return a.Kind == ActionCurate && a.At.After(state.Curator.LastRun)
	})
}

// RenderCuratorDraft draws the issue a Curator draft becomes, with its group as the idempotency key.
// A private draft's lesson and evidence stay in separate sections, so `publish` can copy only the
// lesson out.
func RenderCuratorDraft(g GroupID, d CuratorIssueDraft, private bool) (title, body string, err error) {
	if err := validText("title", d.Title, true); err != nil {
		return "", "", err
	}

	if utf8.RuneCountInString(d.Title) > MaxCuratorTitle {
		return "", "", fmt.Errorf("title is over %d characters", MaxCuratorTitle)
	}

	var b strings.Builder

	b.WriteString(PromotionMarker(g))
	fmt.Fprintf(&b, "\n\n# %s\n", d.Title)

	if private {
		if err := renderCuratorPrivate(&b, d); err != nil {
			return "", "", err
		}
	} else {
		if err := renderCuratorPublic(&b, d); err != nil {
			return "", "", err
		}
	}

	if n := utf8.RuneCountInString(b.String()); n > CommentLimitChars {
		return "", "", fmt.Errorf("drafted issue is %d characters, over the %d limit", n, CommentLimitChars)
	}

	return d.Title, b.String(), nil
}

// renderCuratorPrivate draws a private draft's lesson and evidence in separate sections.
func renderCuratorPrivate(b *strings.Builder, d CuratorIssueDraft) error {
	if err := validText("lesson", d.Lesson, false); err != nil {
		return err
	}

	fmt.Fprintf(b, "\n## Lesson\n\n%s\n", strings.TrimSpace(d.Lesson))

	evidence := strings.TrimSpace(d.PrivateEvidence)
	if evidence == "" {
		return nil
	}

	if err := validText("private evidence", evidence, false); err != nil {
		return err
	}

	fmt.Fprintf(b, "\n## Private evidence\n\n%s\n", evidence)

	return nil
}

// renderCuratorPublic draws a public draft's body under its lesson heading.
func renderCuratorPublic(b *strings.Builder, d CuratorIssueDraft) error {
	if err := validText("body", d.Body, false); err != nil {
		return err
	}

	fmt.Fprintf(b, "\n## Lesson\n\n%s\n", strings.TrimSpace(d.Body))

	return nil
}

// CuratorBrief is the material the Curator reads: the retro log with evidence, the dashboard state
// with what writers have done, and the proposals and links already shown. It is deterministic.
func CuratorBrief(
	ref IssueRef, private bool, reports []DashboardReport, view DashboardView,
) string {
	visibility := "public"
	if private {
		visibility = "private"
	}

	var b strings.Builder

	fmt.Fprintf(&b, "Repository: %s (%s)\nDashboard: #%d\n", ref.Repo, visibility, ref.Number)
	b.WriteString("\n# Retro log\n")

	for _, g := range curatorBriefGroups(reports) {
		fmt.Fprintf(&b, "\n## %s / %s\n", g.ID.Target, g.ID.Key)

		for _, r := range g.Reports[:min(len(g.Reports), MaxGroupReports)] {
			fmt.Fprintf(&b, "\n- %s, %s run `%s` on #%d: %s\n", r.Severity, r.Role, r.Key, r.Issue,
				Truncate(r.Claim, dashboardClaimChars))

			if evidence := strings.TrimSpace(r.Evidence); evidence != "" {
				fmt.Fprintf(&b, "\n  Evidence: %s\n", Truncate(evidence, curatorEvidenceChars))
			}
		}
	}

	b.WriteString("\n# Dashboard state\n")

	renderBriefSections(&b, view)
	renderBriefActions(&b, view.State.Actions)
	renderBriefProposals(&b, view.State)
	renderBriefRejections(&b, view.State.Rejected)

	return Truncate(b.String(), CuratorBriefLimit)
}

// curatorBriefGroups groups the log by target and key, in a stable order.
func curatorBriefGroups(reports []DashboardReport) []DashboardGroup {
	groups := groupReports(reports)

	slices.SortStableFunc(groups, func(a, b DashboardGroup) int {
		return cmp.Or(
			cmp.Compare(a.ID.Target.String(), b.ID.Target.String()),
			cmp.Compare(a.ID.Key, b.ID.Key),
		)
	})

	return groups
}

// renderBriefSections lists each section's groups.
func renderBriefSections(b *strings.Builder, view DashboardView) {
	for _, section := range []struct {
		name   string
		groups []DashboardGroup
	}{
		{"Ready to promote", view.Ready},
		{"Watching", view.Watching},
	} {
		fmt.Fprintf(b, "\n## %s\n", section.name)

		if len(section.groups) == 0 {
			b.WriteString("\nNone.\n")

			continue
		}

		for _, g := range section.groups {
			fmt.Fprintf(b, "\n- `%s`\n", g.ID)
		}
	}

	for _, section := range []struct {
		name    string
		entries []DashboardEntry
	}{
		{"Promoted", view.Promoted},
		{"Ignored", view.Ignored},
		{"Regressed", view.Regressed},
	} {
		fmt.Fprintf(b, "\n## %s\n", section.name)

		if len(section.entries) == 0 {
			b.WriteString("\nNone.\n")

			continue
		}

		for _, e := range section.entries {
			fmt.Fprintf(b, "\n- `%s`\n", e.ID())
		}
	}
}

// renderBriefActions lists what writers have done, oldest first.
func renderBriefActions(b *strings.Builder, actions []DashboardAction) {
	b.WriteString("\n## Writer actions\n")

	if len(actions) == 0 {
		b.WriteString("\nNone.\n")

		return
	}

	for _, a := range actions {
		groups := make([]string, 0, len(a.Groups))
		for _, g := range a.Groups {
			groups = append(groups, "`"+g.String()+"`")
		}

		target := "the dashboard"
		if len(groups) > 0 {
			target = strings.Join(groups, " and ")
		}

		fmt.Fprintf(b, "\n- %s %s by @%s on %s\n", a.Kind, target, a.By, a.At.Format(dashboardDate))
	}
}

// renderBriefProposals lists the proposals and links already shown.
func renderBriefProposals(b *strings.Builder, state DashboardState) {
	b.WriteString("\n## Proposals already shown\n")

	if len(state.Proposals) == 0 {
		b.WriteString("\nNone.\n")
	}

	for _, p := range state.Proposals {
		fmt.Fprintf(b, "\n- `%s`: %s — %s\n", p.ID, groupList(p.Groups), p.Reason)
	}

	if len(state.Links) == 0 {
		return
	}

	b.WriteString("\n## Links already shown\n")

	for _, l := range state.Links {
		fmt.Fprintf(b, "\n- `%s`: %s — %s\n", l.ID, groupList(l.Groups), l.Reason)
	}
}

// renderBriefRejections lists the pairs a writer refused for good.
func renderBriefRejections(b *strings.Builder, rejections []DashboardRejection) {
	b.WriteString("\n## Rejections: never propose these pairs again\n")

	if len(rejections) == 0 {
		b.WriteString("\nNone.\n")

		return
	}

	for _, r := range rejections {
		fmt.Fprintf(b, "\n- `%s`: %s\n", r.ID, groupList(r.Groups))
	}
}

// groupList renders groups for the brief.
func groupList(groups []GroupID) string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, "`"+g.String()+"`")
	}

	return strings.Join(out, " and ")
}
