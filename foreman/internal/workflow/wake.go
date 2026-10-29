package workflow

import (
	"slices"
	"time"
)

// Marker is a handoff marker and the comment that carries it.
type Marker struct {
	Item
	Transition
}

// Markers reads every factory-authored handoff marker in comments, oldest first. A comment from
// another author is skipped before its body is read, so a forged marker moves nothing and a forged
// malformed one does not block. A factory marker that does not parse is an error, never skipped.
func Markers(comments []Item, owner string) ([]Marker, error) {
	out := make([]Marker, 0, len(comments))

	for _, c := range comments {
		if !FactoryAuthored(c.Author, owner) {
			continue
		}

		t, ok, err := ParseTransition(c.Body)
		if err != nil {
			return nil, err
		}

		if ok {
			out = append(out, Marker{Item: c, Transition: t})
		}
	}

	return out, nil
}

// LatestMarker is the last handoff marker in API order, which is oldest first.
func LatestMarker(comments []Item, owner string) (Marker, bool, error) {
	markers, err := Markers(comments, owner)
	if err != nil || len(markers) == 0 {
		return Marker{}, false, err
	}

	return markers[len(markers)-1], true, nil
}

// AfterMarker returns the comments posted after marker, oldest first, with factory handoff markers
// left out. Another author's comment that merely looks like a marker is not a factory record, so it
// stays in the thread a run reads.
func AfterMarker(comments []Item, marker Marker, owner string) []Item {
	var out []Item

	for _, c := range comments {
		if c.ID <= marker.ID {
			continue
		}

		// A factory handoff marker is foreman's own record, not part of the thread a run reads.
		if FactoryAuthored(c.Author, owner) {
			if _, ok, err := ParseTransition(c.Body); ok || err != nil {
				continue
			}
		}

		out = append(out, c)
	}

	return out
}

// WakeState returns the state a writer's comment after the latest marker sends the issue to. Only
// the state the latest marker names can be woken: a waiting issue goes back to planning, a done issue
// back to implementation, and a failed one back to the agent role that failed. A non-writer or a
// state no comment moves wakes nothing.
func WakeState(latest Transition, found, writer bool) (State, bool) {
	if !found || !writer {
		return "", false
	}

	// A waiting issue goes back to planning and a done one back to implementation.
	queued := map[State]State{Waiting: Plan, Done: Implement}
	if state, ok := queued[latest.To]; ok {
		return state, true
	}

	// A failed issue goes back to the agent role that failed.
	if latest.To == Failed {
		state, ok := Queued(latest.Role)
		return state, ok
	}

	return "", false
}

// waitingOnWriter reports whether state is one only a writer's comment can leave.
func waitingOnWriter(state State) bool {
	return state == Waiting || state == Done || state == Failed
}

// Guidance returns the comments a woken run must read: every comment posted after the handoff marker
// that left the issue waiting on a writer, when a run was woken out of that state. Comments are
// oldest first, with handoff markers left out. A run that was not woken reads nothing.
func Guidance(comments []Item, owner string) []Item {
	markers, err := Markers(comments, owner)
	if err != nil || len(markers) == 0 {
		return nil
	}

	wake := wakeMarker(markers)
	if !waitingOnWriter(wake.From) {
		return nil
	}

	for _, marker := range slices.Backward(markers) {
		if marker.ID < wake.ID && marker.To == wake.From {
			return AfterMarker(comments, marker, owner)
		}
	}

	return nil
}

// wakeMarker is the marker that woke the run: the one before the run's claim, or the latest marker
// when the run was started without a claim.
func wakeMarker(markers []Marker) Marker {
	last := markers[len(markers)-1]
	if len(markers) == 1 {
		return last
	}

	before := markers[len(markers)-2]
	if last.From == before.To && waitingOnWriter(before.From) {
		return before
	}

	return last
}

// RunningState names the agent role and running label of an issue a run has claimed.
func RunningState(issue Issue) (Role, State, bool) {
	for _, q := range Queues() {
		if HasLabel(issue, string(q.Running)) {
			return q.Role, q.Running, true
		}
	}

	return "", "", false
}

// TimelineLabel is the label an issue timeline event concerns.
type TimelineLabel struct {
	// Name is the label's text.
	Name string `json:"name"`
}

// TimelineEvent is the part of an issue timeline event foreman reads.
type TimelineEvent struct {
	// Event is the event's kind, such as "labeled".
	Event string `json:"event"`
	// Label is the label a labeled event added.
	Label *TimelineLabel `json:"label"`
	// CreatedAt is when the event happened.
	CreatedAt time.Time `json:"created_at"`
}

// LabeledAt returns when label was most recently added, from the timeline's labeled events.
func LabeledAt(events []TimelineEvent, label string) (time.Time, bool) {
	var (
		found bool
		at    time.Time
	)

	for _, e := range events {
		if e.Event != "labeled" || e.Label == nil || e.Label.Name != label {
			continue
		}

		if !found || e.CreatedAt.After(at) {
			at, found = e.CreatedAt, true
		}
	}

	return at, found
}

// DeadRun reports whether a run whose running label was added at since has outlived the agent role's
// deadline plus the dead-run margin. Nothing retries a dead run automatically.
func DeadRun(since, now time.Time, deadline, margin time.Duration) bool {
	return now.Sub(since) > deadline+margin
}
