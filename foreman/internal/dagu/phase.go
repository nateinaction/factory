package dagu

import "strings"

// Phase is what a free-text status report says about a run.
type Phase int

// The phases Classify returns. The zero value is PhaseUnknown, so a missed case fails closed.
const (
	// PhaseUnknown is a report that names no status foreman knows.
	PhaseUnknown Phase = iota
	// PhaseActive is a report of a run dagu can still move.
	PhaseActive
	// PhaseFinished is a report of a run dagu can no longer move.
	PhaseFinished
)

// Classify reads the status word out of `dagu status` output, which is free text. It matches the
// run model's own status lists, ignoring case and treating an underscore as a space, and an active
// status wins over a finished one so that a report mentioning both blocks. Text naming neither is
// PhaseUnknown.
func Classify(report string) Phase {
	text := normalize(report)

	if mentionsAny(text, activeStatuses()) {
		return PhaseActive
	}

	if mentionsAny(text, finishedStatuses()) {
		return PhaseFinished
	}

	return PhaseUnknown
}

// normalize lower-cases text and turns underscores into spaces, so not_started and "not started"
// compare equal.
func normalize(text string) string {
	return strings.ReplaceAll(strings.ToLower(text), "_", " ")
}

// mentionsAny reports whether text, already normalized, contains any status.
func mentionsAny(text string, statuses []string) bool {
	for _, status := range statuses {
		if strings.Contains(text, normalize(status)) {
			return true
		}
	}

	return false
}
