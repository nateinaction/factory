package workflow

import (
	"slices"
	"time"
)

// StateTime is how long an issue carried one handoff label.
type StateTime struct {
	// State is the handoff state the label names.
	State State
	// Duration is the total time the label was on the issue.
	Duration time.Duration
}

// History is what GitHub records of one issue's lifecycle in the factory.
type History struct {
	// Number is the issue number.
	Number int
	// Times are the states the issue carried, in spec order, each with its total time.
	Times []StateTime
	// Gate is the time the issue waited at the human gate: for a writer to answer the Planner.
	Gate time.Duration
	// Rework counts the times a review sent the issue back to the Implementer.
	Rework int
	// Outcome is the state the issue's latest handoff left it in.
	Outcome State
}

// Timeline event kinds that move a label or end the issue's open spans.
const (
	eventLabeled   = "labeled"
	eventUnlabeled = "unlabeled"
	eventClosed    = "closed"
)

// BuildHistory reads one issue's lifecycle from its factory handoff markers and its timeline. The
// time in a state is the time its label was on the issue, so only a label that names a handoff state
// counts and any other label is ignored. A label still on the issue at the end, or never removed
// before the issue closed, is counted up to the close or to now. It reports false when the factory
// left no trace on the issue: no marker and no handoff label.
func BuildHistory(number int, markers []Marker, events []TimelineEvent, now time.Time) (History, bool) {
	times := stateTimes(events, now)
	if len(markers) == 0 && len(times) == 0 {
		return History{}, false
	}

	h := History{Number: number, Times: times, Rework: reworkRounds(markers)}

	if last := len(markers) - 1; last >= 0 {
		h.Outcome = markers[last].To
	}

	for _, t := range times {
		if t.State == Waiting {
			h.Gate = t.Duration
		}
	}

	return h, true
}

// reworkRounds counts the handoffs that sent an issue from review back to the Implementer.
func reworkRounds(markers []Marker) int {
	n := 0

	for _, m := range markers {
		if (m.From == Reviewing || m.From == Review) && m.To == Implement {
			n++
		}
	}

	return n
}

// stateTimes adds up how long each handoff label was on the issue, in spec order, leaving out a state
// the issue never carried.
func stateTimes(events []TimelineEvent, now time.Time) []StateTime {
	sorted := slices.SortedStableFunc(slices.Values(events), func(a, b TimelineEvent) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})

	spans := labelSpans{total: map[State]time.Duration{}, since: map[State]time.Time{}}
	seen := map[State]bool{}

	for _, e := range sorted {
		if e.Event == eventClosed {
			spans.closeAll(e.CreatedAt)

			continue
		}

		state, ok := handoffLabel(e)
		if !ok {
			continue
		}

		seen[state] = true

		if e.Event == eventLabeled {
			spans.open(state, e.CreatedAt)
		} else {
			spans.close(state, e.CreatedAt)
		}
	}

	spans.closeAll(now)

	out := make([]StateTime, 0, len(seen))

	for _, s := range States() {
		if seen[s] {
			out = append(out, StateTime{State: s, Duration: spans.total[s]})
		}
	}

	return out
}

// labelSpans is the running tally of how long each label has been on an issue.
type labelSpans struct {
	// total is the time of the spans already closed.
	total map[State]time.Duration
	// since is when each label still on the issue was added.
	since map[State]time.Time
}

// open starts a span, unless the label is already on the issue.
func (l labelSpans) open(s State, at time.Time) {
	if _, on := l.since[s]; !on {
		l.since[s] = at
	}
}

// close ends the span of a label that is on the issue.
func (l labelSpans) close(s State, at time.Time) {
	if start, on := l.since[s]; on {
		l.total[s] += max(0, at.Sub(start))

		delete(l.since, s)
	}
}

// closeAll ends every open span.
func (l labelSpans) closeAll(at time.Time) {
	for s := range l.since {
		l.close(s, at)
	}
}

// handoffLabel is the handoff state a label event concerns, or false for any other event or label.
func handoffLabel(e TimelineEvent) (State, bool) {
	if e.Label == nil || (e.Event != eventLabeled && e.Event != eventUnlabeled) {
		return "", false
	}

	s, err := ParseState(e.Label.Name)
	if err != nil || s == Closed {
		return "", false // a label that is not the factory's; Closed is a state without a label
	}

	return s, true
}
