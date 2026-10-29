package foreman

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/nateinaction/factory/foreman/internal/dagu"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// Bounds on a follow, so it cannot run forever when dagu stops reporting.
const (
	// FollowPoll is how often logs --follow asks for new output.
	FollowPoll = time.Second
	// FollowMax bounds one follow. The run's own deadline is the normal end.
	FollowMax = 2 * time.Hour
)

// Logs is `foreman logs`: it prints the latest dagu run log for an issue, and with follow keeps
// printing until the run ends. It reads dagu's state and log files and talks to nothing else.
func Logs(ctx context.Context, d Deps, repo string, number int, follow bool) (int, error) {
	if err := workflow.ValidateRepo(repo); err != nil {
		return 1, err
	}

	if number <= 0 {
		return 1, fmt.Errorf("not an issue number: %d", number)
	}

	runs, err := readHistory(ctx, d)
	if err != nil {
		return 1, err
	}

	run, ok := latestRun(factoryRuns(runs, d.Settings.DAGPrefix), repo, number)
	if !ok {
		return 1, fmt.Errorf("no dagu run for %s issue %d", repo, number)
	}

	state, refs, err := readRunStatus(ctx, d, run.Run)
	if err != nil {
		return 1, err
	}

	offsets, printed, err := writeLogs(d.Out, refs)
	if err != nil {
		return 1, err
	}

	if !printed {
		return 1, fmt.Errorf("run %s for %s issue %d is %s and has no log yet", run.RunID, repo, number, state)
	}

	if !follow {
		return 0, nil
	}

	return 0, followRun(ctx, d, run.Run, state, refs, offsets)
}

// latestRun picks the newest run for the issue. dagu lists runs newest first.
func latestRun(runs []factoryRun, repo string, number int) (factoryRun, bool) {
	for _, run := range runs {
		if run.repo == repo && run.issue == number {
			return run, true
		}
	}

	return factoryRun{}, false
}

// writeLogs prints every log file the run wrote and returns where each one now ends.
func writeLogs(w io.Writer, refs []dagu.LogRef) (map[string]int64, bool, error) {
	offsets := make(map[string]int64, len(refs))
	printed := false

	for _, ref := range refs {
		next, err := appendLog(w, ref.Path, 0)
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue // dagu names both streams, and one may never have been written
		case err != nil:
			return nil, false, err
		}

		offsets[ref.Path] = next
		printed = true
	}

	return offsets, printed, nil
}

// appendLog copies a log file from offset to its end and returns the new offset.
func appendLog(w io.Writer, path string, offset int64) (int64, error) {
	f, err := os.Open(path) //nolint:gosec // the path is dagu's own log file, named in dagu's status output
	if err != nil {
		return offset, fmt.Errorf("open log: %w", err)
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, errors.Join(fmt.Errorf("seek log: %w", err), wrapErr("close log", f.Close()))
	}

	written, err := io.Copy(w, f)

	return offset + written, errors.Join(wrapErr("read log", err), wrapErr("close log", f.Close()))
}

// followRun prints new output until the run reaches a state it cannot leave. The wait is bounded by
// FollowMax and by the caller's context.
func followRun(
	ctx context.Context, d Deps, run dagu.Run, state string, refs []dagu.LogRef, offsets map[string]int64,
) error {
	deadline := d.now().Add(FollowMax)

	for {
		var err error
		if offsets, err = appendNewLogs(d.Out, refs, offsets); err != nil {
			return err
		}

		if !(dagu.Run{Status: state}).Active() {
			return nil
		}

		if d.now().After(deadline) {
			return fmt.Errorf("follow run %s: still %s after %s", run.RunID, state, FollowMax)
		}

		if err := d.sleep(ctx, FollowPoll); err != nil {
			return err
		}

		if state, refs, err = readRunStatus(ctx, d, run); err != nil {
			return err
		}
	}
}

// appendNewLogs copies whatever each log file gained since the last read.
func appendNewLogs(w io.Writer, refs []dagu.LogRef, offsets map[string]int64) (map[string]int64, error) {
	for _, ref := range refs {
		next, err := appendLog(w, ref.Path, offsets[ref.Path])
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue // a stream dagu names before it exists
		case err != nil:
			return nil, err
		}

		offsets[ref.Path] = next
	}

	return offsets, nil
}
