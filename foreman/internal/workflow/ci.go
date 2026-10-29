package workflow

import (
	"fmt"
	"strings"
	"time"
)

// CI is what GitHub reports about a pull request's head commit: its check runs and its commit
// statuses. The rules below turn that into the gate before a review and the report the Implementer
// reads, with no I/O.

// CheckOutput is what a check run says it found.
type CheckOutput struct {
	// Title is the check's one-line heading.
	Title string `json:"title"`
	// Summary is the check's longer description.
	Summary string `json:"summary"`
}

// CheckRun is one check run GitHub reports for a commit.
type CheckRun struct {
	// ID is the check run's id, which is its Actions job id for an Actions check.
	ID int64 `json:"id"`
	// Name is the check's name.
	Name string `json:"name"`
	// Status is the check's lifecycle: "queued", "in_progress" or "completed".
	Status string `json:"status"`
	// Conclusion is the check's result once completed, such as "success" or "failure".
	Conclusion string `json:"conclusion"`
	// Output is what the check reported.
	Output CheckOutput `json:"output"`
	// App names the App that runs the check, which is "github-actions" for an Actions job.
	App struct {
		Slug string `json:"slug"`
	} `json:"app"`
}

// passed reports whether the check run succeeded: a completed check whose conclusion passes.
func (c CheckRun) passed() bool {
	switch c.Conclusion {
	case "success", "neutral", "skipped":
		return true
	}

	return false
}

// Failed reports whether the check run concluded in failure.
func (c CheckRun) Failed() bool {
	return c.Status == "completed" && c.Conclusion != "" && !c.passed()
}

// finished reports whether the check run is complete, whatever it concluded.
func (c CheckRun) finished() bool {
	return c.Status == "completed"
}

// CommitStatus is one commit status GitHub reports for a commit.
type CommitStatus struct {
	// Context is the status's name.
	Context string `json:"context"`
	// State is "pending", "success", "failure" or "error".
	State string `json:"state"`
	// Description is the status's one-line text.
	Description string `json:"description"`
	// TargetURL links to the status's details.
	TargetURL string `json:"target_url"`
}

// JobLog is the end of one failed Actions job's log.
type JobLog struct {
	// Name is the check the log belongs to.
	Name string
	// Tail is the log's last lines, cut to keep its end.
	Tail string
}

// CIReport is what foreman read about one commit's CI.
type CIReport struct {
	// SHA is the commit the report is about.
	SHA string
	// Checks are the commit's check runs.
	Checks []CheckRun
	// Statuses are the commit's commit statuses.
	Statuses []CommitStatus
	// Logs are the tails of the failed Actions jobs' logs.
	Logs []JobLog
}

// CIState is what a commit's CI amounts to. The zero value is CINone.
type CIState string

// The states a pull request's CI can be in.
const (
	// CINone means the commit has no checks and no statuses, which gates nothing.
	CINone CIState = "none"
	// CIPassed means every check and status passed.
	CIPassed CIState = "passed"
	// CIPending means some check or status has not finished.
	CIPending CIState = "pending"
	// CIFailed means some check or status failed, which outranks a pending one.
	CIFailed CIState = "failed"
)

// The CI state ranks, so the worst state of several reads wins.
const (
	rankNone = iota
	rankPassed
	rankPending
	rankFailed
)

// Classify reads a report into the state of its CI: a failure first, then pending, then passed.
func Classify(report CIReport) CIState {
	state := CINone

	for _, check := range report.Checks {
		state = maxState(state, classifyCheck(check))
		if state == CIFailed {
			return state
		}
	}

	for _, status := range report.Statuses {
		state = maxState(state, classifyStatus(status))
		if state == CIFailed {
			return state
		}
	}

	return state
}

// classifyCheck reads one check run: a completed one passed by its conclusion, an unfinished one is
// pending.
func classifyCheck(check CheckRun) CIState {
	switch {
	case check.Failed():
		return CIFailed
	case check.finished() && check.passed():
		return CIPassed
	}

	return CIPending
}

// classifyStatus reads one commit status. An unknown state is pending, never passed.
func classifyStatus(status CommitStatus) CIState {
	switch status.State {
	case "failure", "error":
		return CIFailed
	case "success":
		return CIPassed
	case "pending", "":
		return CIPending
	}

	return CIPending
}

// maxState keeps the worse of two CI states.
func maxState(a, b CIState) CIState {
	if ciRank(a) >= ciRank(b) {
		return a
	}

	return b
}

// ciRank orders the CI states so the worst one wins.
func ciRank(s CIState) int {
	switch s {
	case CIFailed:
		return rankFailed
	case CIPending:
		return rankPending
	case CIPassed:
		return rankPassed
	case CINone:
		return rankNone
	}

	return rankNone
}

// CIDecision is what the CI gate does with an issue queued for review.
type CIDecision string

// The decisions of the CI gate.
const (
	// CIClaim lets the Reviewer claim the issue: all passed, or there was nothing to read.
	CIClaim CIDecision = "claim"
	// CIWait keeps the issue queued while CI is still pending.
	CIWait CIDecision = "wait"
	// CIBack sends the issue back to the Implementer with no model run.
	CIBack CIDecision = "back"
	// CIFail fails the issue because CI stayed pending past its deadline.
	CIFail CIDecision = "fail"
)

// DecideCI decides the gate from the CI's state and whether the issue outlived the CI pending
// deadline. A failure is always sent back; a pending one waits until its deadline.
func DecideCI(state CIState, pendingExpired bool) CIDecision {
	switch state {
	case CIFailed:
		return CIBack
	case CIPending:
		if pendingExpired {
			return CIFail
		}

		return CIWait
	case CIPassed, CINone:
		return CIClaim
	}

	return CIClaim
}

// PendingExpired reports whether pending CI has waited past its deadline. A zero deadline never
// expires, so a host that does not set one leaves the issue queued.
func PendingExpired(since, now time.Time, deadline time.Duration) bool {
	return deadline > 0 && now.Sub(since) > deadline
}

// RoundLimitReached reports whether rounds has reached limit. A limit that is not positive never
// trips, since a missing limit must not fail an issue.
func RoundLimitReached(rounds, limit int) bool {
	return limit > 0 && rounds >= limit
}

// CI failure report limits.
const (
	// CheckSummaryChars caps one check's output summary in a report.
	CheckSummaryChars = 1_500
	// JobLogTailChars caps one job log's tail in a report.
	JobLogTailChars = 4_000
)

// Tail keeps at most the last n characters of s; none when n is not positive.
func Tail(s string, n int) string {
	if n <= 0 {
		return ""
	}

	r := []rune(s)
	if len(r) <= n {
		return s
	}

	return string(r[len(r)-n:])
}

// CIFailureMarkerPrefix starts every CI failure marker foreman posts.
const CIFailureMarkerPrefix = MarkerPrefix + "ci-failure "

// CIFailureMarker keys a CI failure round to the commit it judged.
func CIFailureMarker(sha string) string {
	return CIFailureMarkerPrefix + "sha=" + sha + " -->"
}

// FailedChecks keeps the check runs that failed, in report order.
func FailedChecks(report CIReport) []CheckRun {
	var out []CheckRun

	for _, check := range report.Checks {
		if check.Failed() {
			out = append(out, check)
		}
	}

	return out
}

// failedStatuses keeps the statuses that failed, in report order.
func failedStatuses(report CIReport) []CommitStatus {
	var out []CommitStatus

	for _, status := range report.Statuses {
		if status.State == "failure" || status.State == "error" {
			out = append(out, status)
		}
	}

	return out
}

// RenderCIFailure renders the marker and the report the Implementer reads: every failed check's
// name, conclusion and output summary, every failing status, and the tail of each failed job's log.
// The whole report is cut to the comment limit.
func RenderCIFailure(report CIReport) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s\nCI failed on %s.\n", CIFailureMarker(report.SHA), report.SHA)

	for _, check := range FailedChecks(report) {
		summary := check.Output.Summary
		if summary == "" {
			summary = check.Output.Title
		}

		fmt.Fprintf(&b, "\n- **%s** (%s): %s", check.Name, check.Conclusion,
			Truncate(strings.TrimSpace(summary), CheckSummaryChars))
	}

	if statuses := failedStatuses(report); len(statuses) > 0 {
		b.WriteString("\n\nFailed statuses:")

		for _, status := range statuses {
			fmt.Fprintf(&b, "\n- **%s** (%s): %s", status.Context, status.State, status.Description)
		}
	}

	for _, log := range report.Logs {
		fmt.Fprintf(&b, "\n\nTail of the %s job log:\n\n```\n%s\n```", log.Name, log.Tail)
	}

	return Truncate(b.String(), CommentLimitChars)
}

// HasCIFailure reports whether a factory comment already carries the CI failure report for sha, so a
// retried poll never posts the same report twice. Another author's marker is not a report.
func HasCIFailure(comments []Item, sha, owner string) bool {
	marker := CIFailureMarker(sha)

	for _, c := range FactoryStartsWith(comments, marker, owner) {
		if c.Body == marker || strings.HasPrefix(c.Body, marker+"\n") {
			return true
		}
	}

	return false
}

// CIFailureText is a CI failure report without its marker line, for a prompt.
func CIFailureText(body string) string {
	_, rest, _ := strings.Cut(body, "\n")

	return strings.TrimSpace(rest)
}

// LatestCIFailure returns the newest factory CI failure report, when it comes after the handoff that
// sent the pull request to review. A report from an earlier round is stale and left out, so a run
// never reads a failure a later round already answered. Another author's reports and markers are
// skipped, so a forged one is never read; a malformed factory handoff marker is an error, never skipped.
func LatestCIFailure(comments []Item, owner string) (Item, bool, error) {
	markers, err := Markers(comments, owner)
	if err != nil {
		return Item{}, false, err
	}

	var (
		latestReview  int64
		latestFailure *Item
	)

	for _, marker := range markers {
		if marker.To == Review {
			latestReview = marker.ID
		}
	}

	for _, c := range FactoryStartsWith(comments, CIFailureMarkerPrefix, owner) {
		latestFailure = &c
	}

	if latestFailure == nil || latestReview == 0 || latestFailure.ID <= latestReview {
		return Item{}, false, nil
	}

	return *latestFailure, true, nil
}
