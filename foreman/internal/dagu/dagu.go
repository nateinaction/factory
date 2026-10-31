// Package dagu reads dagu's run state and logs through the dagu CLI. It is pure: it builds the
// command lines and parses what dagu printed, and the shell runs them.
package dagu

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Run statuses foreman reads from dagu. A run is active while dagu can still move it.
const (
	// StatusQueued is a run waiting for a worker.
	StatusQueued = "queued"
	// StatusRunning is a run a worker is executing.
	StatusRunning = "running"
	// StatusWaiting is a run waiting on a human task.
	StatusWaiting = "waiting"
	// StatusNotStarted is a run dagu has recorded but not begun.
	StatusNotStarted = "not_started"
	// StatusRetrying is a run dagu will start again. dagu reports such a run as running, so history
	// never filters on it; it is active because the bash drain check counted it and a run about to
	// restart can still change state.
	StatusRetrying = "retrying"
)

// Statuses of a run dagu can no longer move.
const (
	// StatusSucceeded is a run whose steps all passed.
	StatusSucceeded = "succeeded"
	// StatusFailed is a run with a failed step.
	StatusFailed = "failed"
	// StatusCanceled is a run a person or dagu cancelled.
	StatusCanceled = "canceled"
	// StatusCancelled is the other spelling dagu uses for StatusCanceled.
	StatusCancelled = "cancelled"
	// StatusAborted is a run dagu stopped.
	StatusAborted = "aborted"
	// StatusPartiallySucceeded is a run in which some steps passed and the rest were allowed to fail.
	StatusPartiallySucceeded = "partially_succeeded"
	// StatusRejected is a run a human task turned down.
	StatusRejected = "rejected"
)

// Streams a run writes, as `dagu status` names them.
const (
	// StreamScheduler is dagu's own scheduler log for the run.
	StreamScheduler = "log"
	// StreamStdout is a step's standard output.
	StreamStdout = "stdout"
	// StreamStderr is a step's standard error.
	StreamStderr = "stderr"
)

// Run is one DAG run as `dagu history --format json` reports it.
type Run struct {
	// Name is the DAG name, for example factory-implementer.
	Name string
	// RunID is dagu's id for the run.
	RunID string
	// Status is one of dagu's run statuses, lower case.
	Status string
	// StartedAt is when the run started, in dagu's timestamp format; empty until it starts.
	StartedAt string
	// FinishedAt is when the run ended; empty until it does.
	FinishedAt string
	// Params are the run's parameters, as "KEY=value KEY=value".
	Params string
	// WorkerID is the host that executed the run; empty until a worker takes it.
	WorkerID string
}

// Active reports whether the run can still change state.
func (r Run) Active() bool {
	return slices.Contains(activeStatuses(), r.Status)
}

// activeStatuses are the statuses of a run dagu can still move. It is the one definition: Active,
// ActiveStatuses and Classify all read it.
func activeStatuses() []string {
	return []string{StatusQueued, StatusRunning, StatusWaiting, StatusNotStarted, StatusRetrying}
}

// finishedStatuses are the statuses of a run dagu can no longer move.
func finishedStatuses() []string {
	return []string{
		StatusSucceeded, StatusFailed, StatusCanceled, StatusCancelled, StatusAborted,
		StatusPartiallySucceeded, StatusRejected,
	}
}

// Waiting reports whether the run is waiting for a worker: dagu has it queued but no worker has
// started it.
func (r Run) Waiting() bool {
	return r.Status == StatusQueued || r.Status == StatusNotStarted
}

// ActiveStatuses are the active statuses `dagu history --status` can select: every active status
// but retrying, which dagu reports as running.
func ActiveStatuses() []string {
	return slices.DeleteFunc(activeStatuses(), func(s string) bool { return s == StatusRetrying })
}

// Param reads one parameter from the run's params. A key is present only when it is set.
func (r Run) Param(key string) (string, bool) {
	for field := range strings.FieldsSeq(r.Params) {
		k, v, ok := strings.Cut(field, "=")
		if ok && k == key {
			return v, true
		}
	}

	return "", false
}

// historyEntry is the part of one history record foreman reads.
type historyEntry struct {
	Name       string `json:"name"`
	DAGRunID   string `json:"dagRunId"`
	Status     string `json:"status"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
	Params     string `json:"params"`
	WorkerID   string `json:"workerId"`
}

// ParseHistory reads `dagu history --format json`. Anything but a JSON list is an error, so a dagu
// failure can never read as no runs.
func ParseHistory(out []byte) ([]Run, error) {
	var entries []historyEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("parse dagu history: %w", err)
	}

	runs := make([]Run, 0, len(entries))
	for _, e := range entries {
		runs = append(runs, Run{
			Name: e.Name, RunID: e.DAGRunID, Status: e.Status,
			StartedAt: e.StartedAt, FinishedAt: e.FinishedAt, Params: e.Params, WorkerID: e.WorkerID,
		})
	}

	return runs, nil
}

// HistoryArgv is the command that lists recent runs, newest first.
func HistoryArgv(binary string, limit int) []string {
	return []string{binary, "history", "--format", "json", "--limit", strconv.Itoa(limit)}
}

// ActiveHistoryArgv is the command that lists only the runs dagu can still move: the ones that keep
// an agent role's DAG busy.
func ActiveHistoryArgv(binary string, limit int) []string {
	return []string{
		binary, "history", "--status", strings.Join(ActiveStatuses(), ","),
		"--format", "json", "--limit", strconv.Itoa(limit),
	}
}

// StatusArgv is the command that renders one run's tree.
func StatusArgv(binary, dag, runID string) []string {
	return []string{binary, "status", "--run-id", runID, dag}
}

// LogRef is one log file a run wrote, and the stream it holds.
type LogRef struct {
	// Stream is the stream's name: log, stdout or stderr.
	Stream string
	// Path is the file dagu wrote.
	Path string
}

// treeChars are the glyphs dagu's status tree draws before a line.
const treeChars = "│├└─ \t"

// ParseLogPaths reads the log files from `dagu status` output. The status tree prints each file's
// path, so foreman reads dagu's own logs rather than keeping a pointer of its own.
func ParseLogPaths(status string) []LogRef {
	var refs []LogRef

	for line := range strings.SplitSeq(status, "\n") {
		ref, ok := parseLogLine(strings.TrimLeft(line, treeChars))
		if ok {
			refs = append(refs, ref)
		}
	}

	return refs
}

// parseLogLine reads one line's stream and path, if it is a path line.
func parseLogLine(line string) (LogRef, bool) {
	for _, stream := range []string{StreamScheduler, StreamStdout, StreamStderr} {
		path, ok := strings.CutPrefix(line, stream+": ")
		if ok && path != "" {
			return LogRef{Stream: stream, Path: path}, true
		}
	}

	return LogRef{}, false
}

// ParseStatus reads a run's status from `dagu status` output. It prefers the result line a finished
// run prints and falls back to the header line a live run prints.
func ParseStatus(status string) (string, bool) {
	for line := range strings.SplitSeq(status, "\n") {
		if result, ok := strings.CutPrefix(line, "Result: "); ok {
			return strings.ToLower(strings.TrimSpace(result)), true
		}
	}

	header, _, _ := strings.Cut(status, "\n")

	state, _, found := strings.Cut(strings.TrimSpace(header), " - ")
	if !found {
		return "", false
	}

	state = strings.ToLower(strings.TrimSpace(state))
	if state == "" {
		return "", false
	}

	return state, true
}
