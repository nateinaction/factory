package foreman

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nateinaction/factory/foreman/internal/opencode"
)

// usageDirName is the directory under the work root where each run leaves its usage record.
const usageDirName = "usage"

// usageRecordPath is the usage record of a run with runID under root. A run id that is not a plain
// name is refused, so it cannot name a path outside the usage directory; the run id is dagu's, not
// input from a repository.
func usageRecordPath(root, runID string) (string, error) {
	if !validRunID.MatchString(runID) {
		return "", fmt.Errorf("usage record: run id %q is not a plain name", runID)
	}

	return filepath.Join(root, usageDirName, runID+".json"), nil
}

// recordUsage leaves the run's usage record for `foreman status`, and prints the same record on
// stdout for people reading the run's log. The record is the run's out-of-band one: a line on the
// run's stdout cannot write it, so it cannot forge what status reports. Recording never fails or
// delays a run, because it is telemetry: a write that fails is a warning and the run goes on.
func (d Deps) recordUsage(report opencode.UsageReport) {
	path, err := usageRecordPath(d.Settings.WorkRoot, d.runID())
	if err != nil {
		d.log().Warn("usage record failed", "error", err)
	} else if err := writeUsageRecord(path, report); err != nil {
		d.log().Warn("usage record failed", "error", err)
	}

	d.logUsage(report)
}

// writeUsageRecord writes the record atomically: it writes a private temporary file in the same
// directory and renames it into place, so a reader sees the whole record or none of it, never a
// partial one.
func writeUsageRecord(path string, report opencode.UsageReport) (err error) {
	raw, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("usage record: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, privateDirMode); err != nil {
		return fmt.Errorf("usage record: %w", err)
	}

	f, err := os.CreateTemp(dir, ".usage-*")
	if err != nil {
		return fmt.Errorf("usage record: %w", err)
	}

	tmp := f.Name()

	defer func() {
		if err != nil {
			err = errors.Join(err, os.Remove(tmp)) // nothing is renamed on failure
		}
	}()

	if _, err := f.Write(raw); err != nil {
		return errors.Join(fmt.Errorf("usage record: %w", err), f.Close())
	}

	if err := f.Chmod(privateFileMode); err != nil {
		return errors.Join(fmt.Errorf("usage record: %w", err), f.Close())
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("usage record: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("usage record: %w", err)
	}

	return nil
}
