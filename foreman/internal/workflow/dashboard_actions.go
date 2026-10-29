package workflow

import "slices"

// WithAction records one writer action, keeping the actions oldest first.
func (s DashboardState) WithAction(a DashboardAction) DashboardState {
	s.Actions = append(slices.Clone(s.Actions), a)

	return s
}

// DropProposals removes every proposal that names one of groups, so a merge leaves no stale proposal
// pointing at a group that is gone. It also drops one naming a group a writer merged away.
func (s DashboardState) DropProposals(groups []GroupID) DashboardState {
	kept := make([]DashboardProposal, 0, len(s.Proposals))

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

	return s
}

// WithRejection records a proposal a writer refused for good and drops it from the proposals. A later
// proposal naming the same pair is refused too, by BuildDashboard's filter.
func (s DashboardState) WithRejection(r DashboardRejection) DashboardState {
	s.Rejected = append(slices.Clone(s.Rejected), r)

	kept := make([]DashboardProposal, 0, len(s.Proposals))

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
func (s DashboardState) Proposal(id string) (DashboardProposal, bool) {
	for _, p := range s.Proposals {
		if p.ID == id {
			return p, true
		}
	}

	return DashboardProposal{}, false
}
