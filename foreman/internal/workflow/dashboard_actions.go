package workflow

import (
	"slices"
	"time"
)

// WithAction records one writer action, keeping the actions oldest first.
func (s DashboardState) WithAction(a DashboardAction) DashboardState {
	s.Actions = append(slices.Clone(s.Actions), a)

	return s
}

// WithProposal records one Curator merge proposal. A proposal with the same id, or naming the same
// pair of groups, is replaced rather than shown twice.
func (s DashboardState) WithProposal(p GroupRelation) DashboardState {
	kept := make([]GroupRelation, 0, len(s.Proposals)+1)

	for _, old := range s.Proposals {
		if old.ID == p.ID || sameGroups(old.Groups, p.Groups) {
			continue
		}

		kept = append(kept, old)
	}

	s.Proposals = append(kept, p) //nolint:gocritic // kept is a fresh slice, not s.Proposals

	return s
}

// WithLink records one Curator link, replacing a link with the same id or between the same groups.
func (s DashboardState) WithLink(l GroupRelation) DashboardState {
	kept := make([]GroupRelation, 0, len(s.Links)+1)

	for _, old := range s.Links {
		if old.ID == l.ID || sameGroups(old.Groups, l.Groups) {
			continue
		}

		kept = append(kept, old)
	}

	s.Links = append(kept, l) //nolint:gocritic // kept is a fresh slice, not s.Links

	return s
}

// WithCuratorClaim records that a poll queued a Curator run, so no second one is queued at once.
func (s DashboardState) WithCuratorClaim(at time.Time) DashboardState {
	s.Curator.Claim = at

	return s
}

// WithCuratorRun records that a Curator run finished, clearing the claim.
func (s DashboardState) WithCuratorRun(at time.Time) DashboardState {
	s.Curator = DashboardCurator{LastRun: at}

	return s
}

// DropProposals removes every proposal that names one of groups, so a merge leaves no stale proposal
// pointing at a group that is gone. It also drops one naming a group a writer merged away, and any
// link through those groups.
func (s DashboardState) DropProposals(groups []GroupID) DashboardState {
	kept := make([]GroupRelation, 0, len(s.Proposals))

	for _, p := range s.Proposals {
		if slices.ContainsFunc(p.Groups, func(g GroupID) bool { return slices.Contains(groups, g) }) {
			continue
		}

		kept = append(kept, p)
	}

	if len(kept) == 0 {
		kept = nil
	}

	s.Proposals = kept

	links := make([]GroupRelation, 0, len(s.Links))

	for _, l := range s.Links {
		if slices.ContainsFunc(l.Groups, func(g GroupID) bool { return slices.Contains(groups, g) }) {
			continue
		}

		links = append(links, l)
	}

	if len(links) == 0 {
		links = nil
	}

	s.Links = links

	return s
}

// WithRejection records a proposal a writer refused for good and drops it from the proposals. A later
// proposal naming the same pair is refused too, by BuildDashboard's filter.
func (s DashboardState) WithRejection(r DashboardRejection) DashboardState {
	s.Rejected = append(slices.Clone(s.Rejected), r)

	kept := make([]GroupRelation, 0, len(s.Proposals))

	for _, p := range s.Proposals {
		if p.ID == r.ID || sameGroups(p.Groups, r.Groups) {
			continue
		}

		kept = append(kept, p)
	}

	if len(kept) == 0 {
		kept = nil
	}

	s.Proposals = kept

	return s
}

// WithPublication records a lesson a writer published out of a private repository.
func (s DashboardState) WithPublication(p DashboardPublication) DashboardState {
	s.Published = append(slices.Clone(s.Published), p)

	return s
}

// HasHandled reports whether a command comment has already been dealt with.
func (s DashboardState) HasHandled(id int64) bool {
	return slices.Contains(s.Handled, id)
}

// WithHandled marks a command comment dealt with; ok is false when it already was.
func (s DashboardState) WithHandled(id int64) (DashboardState, bool) {
	if s.HasHandled(id) {
		return s, false
	}

	s.Handled = append(slices.Clone(s.Handled), id)

	return s, true
}

// Proposal returns the Curator proposal with id.
func (s DashboardState) Proposal(id string) (GroupRelation, bool) {
	for _, p := range s.Proposals {
		if p.ID == id {
			return p, true
		}
	}

	return GroupRelation{}, false
}
