package foreman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/dagu"
	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// Ceilings on what a read-only report reads.
const (
	// DaguTimeout bounds one read-only dagu command.
	DaguTimeout = 60 * time.Second
	// StatusRunLimit bounds how many runs status asks dagu about.
	StatusRunLimit = 200
	// UsageRunLimit bounds how many recent runs status reads usage from.
	UsageRunLimit = 5
	// UsageRecordLimit bounds how many usage records status reports.
	UsageRecordLimit = 10
	// UsageRecordBytes bounds how much of one usage record status reads.
	UsageRecordBytes = 64 << 10
)

// Names a run's parameters and its usage line carry.
const (
	// paramRepo is the run parameter naming the repository.
	paramRepo = "REPO"
	// paramIssue is the run parameter naming the issue.
	paramIssue = "ISSUE"
	// noValue stands in for a field the report could not read.
	noValue = "-"
	// noRows is a section with nothing to show.
	noRows = "(none)\n"
	// headerRole is the agent role column shared by the queue and usage tables.
	headerRole = "ROLE"
	// tablePadding is the gap between a table's columns.
	tablePadding = 2
)

// factoryRun is one dagu run with the work it belongs to.
type factoryRun struct {
	dagu.Run
	// role is the agent role the run's DAG names.
	role workflow.Role
	// repo is the run's repository, empty when it names none.
	repo string
	// issue is the run's issue number, zero when it names none.
	issue int
}

// readHistory reads recent runs from dagu, newest first.
func readHistory(ctx context.Context, d Deps) ([]dagu.Run, error) {
	out, err := d.run(ctx, proc.Cmd{
		Argv: dagu.HistoryArgv(d.Settings.Dagu, StatusRunLimit), Capture: true, Timeout: DaguTimeout,
	})
	if err != nil {
		return nil, err
	}

	return dagu.ParseHistory([]byte(out))
}

// readRunStatus reads one run's status and the log files it wrote.
func readRunStatus(ctx context.Context, d Deps, run dagu.Run) (state string, refs []dagu.LogRef, err error) {
	out, err := d.run(ctx, proc.Cmd{
		Argv: dagu.StatusArgv(d.Settings.Dagu, run.Name, run.RunID), Capture: true, Timeout: DaguTimeout,
	})
	if err != nil {
		return "", nil, err
	}

	state, ok := dagu.ParseStatus(out)
	if !ok {
		return "", nil, fmt.Errorf("dagu status %s: no state in dagu's status output", run.RunID)
	}

	return state, dagu.ParseLogPaths(out), nil
}

// factoryRuns keeps the runs of the factory's DAGs and reads the work each names.
func factoryRuns(runs []dagu.Run, prefix string) []factoryRun {
	items := make([]factoryRun, 0, len(runs))

	for _, run := range runs {
		name, ok := strings.CutPrefix(run.Name, prefix)
		if !ok {
			continue
		}

		role, err := workflow.ParseRole(name)
		if err != nil {
			continue // another DAG that happens to share the prefix
		}

		items = append(items, factoryRun{Run: run, role: role, repo: runRepo(run), issue: runIssue(run)})
	}

	return items
}

// runRepo is the repository a run names, empty when it names none.
func runRepo(run dagu.Run) string {
	repo, _ := run.Param(paramRepo)

	return repo
}

// runIssue is the issue a run names, zero when it names none or names something that is not a number.
func runIssue(run dagu.Run) int {
	text, ok := run.Param(paramIssue)
	if !ok {
		return 0
	}

	number, err := workflow.ParseIssueNumber(text)
	if err != nil {
		return 0
	}

	return number
}

// Status is `foreman status`: a read-only report of active runs, the queues, agent roles with no
// host, and recent usage. It reads dagu's state, the configuration's deadlines for each agent role,
// and the usage records the runs on this host left under the work root. It never reads a run's
// stdout, so a line there cannot forge a usage figure. It talks to nothing else.
func Status(ctx context.Context, d Deps, cfg config.Config) (int, error) {
	runs, err := readHistory(ctx, d)
	if err != nil {
		return 1, err
	}

	items := factoryRuns(runs, d.Settings.DAGPrefix)
	report := statusReport{now: d.now(), runs: items, usage: recentUsage(d, items), cfg: cfg}

	if _, err := io.WriteString(d.Out, report.render()); err != nil {
		return 1, fmt.Errorf("write status: %w", err)
	}

	return 0, nil
}

// usageRecord is one usage report with the run that wrote it.
type usageRecord struct {
	run    factoryRun
	report opencode.UsageReport
}

// recentUsage reads the out-of-band record of the newest runs, the one each run leaves under the
// work root. It never reads a run's stdout: a line there that mimics a record must not be reported.
// It is best effort: a run with no readable record is skipped with a warning, because a read-only
// report must not fail over history.
func recentUsage(d Deps, runs []factoryRun) []usageRecord {
	records := make([]usageRecord, 0, UsageRecordLimit)

	for _, run := range firstRuns(runs, UsageRunLimit) {
		if len(records) >= UsageRecordLimit {
			break
		}

		if record, ok := readUsageRecord(d, run); ok {
			records = append(records, record)
		}
	}

	return firstRecords(records, UsageRecordLimit)
}

// readUsageRecord reads one run's out-of-band record. A record that is missing (a run from before
// records, or one started by hand) or unreadable is skipped, not guessed.
func readUsageRecord(d Deps, run factoryRun) (usageRecord, bool) {
	path, err := usageRecordPath(d.Settings.WorkRoot, run.RunID)
	if err != nil {
		d.log().Warn("usage record: cannot name the record", "run", run.RunID, "error", err)

		return usageRecord{}, false
	}

	raw, err := readFileBounded(path, UsageRecordBytes)
	if errors.Is(err, os.ErrNotExist) {
		return usageRecord{}, false // this run left no record
	}

	if err != nil {
		d.log().Warn("usage record: cannot read the record", "run", run.RunID, "error", err)

		return usageRecord{}, false
	}

	var report opencode.UsageReport
	if err := json.Unmarshal(raw, &report); err != nil {
		d.log().Warn("usage record: cannot parse the record", "run", run.RunID, "error", err)

		return usageRecord{}, false
	}

	return usageRecord{run: run, report: report}, true
}

// readFileBounded reads at most limit bytes of a file.
func readFileBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the path is under the configured work root, built from a vetted name
	if err != nil {
		return nil, fmt.Errorf("open record: %w", err)
	}

	raw, err := io.ReadAll(io.LimitReader(f, limit))

	return raw, errors.Join(wrapErr("read record", err), wrapErr("close record", f.Close()))
}

// firstRuns keeps at most n runs.
func firstRuns(runs []factoryRun, n int) []factoryRun {
	if len(runs) > n {
		return runs[:n]
	}

	return runs
}

// firstRecords keeps at most n usage records.
func firstRecords(records []usageRecord, n int) []usageRecord {
	if len(records) > n {
		return records[:n]
	}

	return records
}

// statusReport is the whole of `foreman status`.
type statusReport struct {
	// now is the clock elapsed times are measured against.
	now time.Time
	// runs are the factory's recent runs, newest first.
	runs []factoryRun
	// usage are the newest usage records.
	usage []usageRecord
	// cfg supplies each agent role's deadline.
	cfg config.Config
}

// render draws the report as plain text, one section per question.
func (r statusReport) render() string {
	var b strings.Builder

	b.WriteString("Active runs\n")
	b.WriteString(renderActive(r.runs, r.now, r.cfg))
	b.WriteString("\nQueues\n")
	b.WriteString(renderQueues(r.runs))
	b.WriteString("\nAgent roles with no host\n")
	b.WriteString(renderNoHost(r.runs, r.cfg))
	b.WriteString("\nRecent usage\n")
	b.WriteString(renderUsage(r.usage))

	return b.String()
}

// renderActive draws the runs dagu can still move.
func renderActive(runs []factoryRun, now time.Time, cfg config.Config) string {
	rows := [][]string{{"HOST", "ROLE", "REPOSITORY", "ISSUE/PR", "ELAPSED", "DEADLINE", "RUN ID"}}

	for _, run := range runs {
		if !run.Active() {
			continue
		}

		rows = append(rows, []string{
			orNoValue(run.WorkerID), string(run.role), orNoValue(run.repo), issueLabel(run.issue),
			elapsed(run, now), deadlineLabel(cfg, run.role), run.RunID,
		})
	}

	if len(rows) == 1 {
		return noRows
	}

	return table(rows)
}

// deadlineLabel is the agent role's configured wall-clock deadline, or a dash when it has none.
func deadlineLabel(cfg config.Config, role workflow.Role) string {
	deadline := cfg.Agents[string(role)].Deadline()
	if deadline <= 0 {
		return noValue
	}

	return deadline.String()
}

// renderQueues draws how much work waits for each agent role, in spec order.
func renderQueues(runs []factoryRun) string {
	queued := map[workflow.Role]int{}

	for _, run := range runs {
		if run.Status == dagu.StatusQueued {
			queued[run.role]++
		}
	}

	rows := make([][]string, 0, 1+len(workflow.KnownRoles()))
	rows = append(rows, []string{headerRole, "QUEUED"})

	for _, role := range workflow.KnownRoles() {
		rows = append(rows, []string{string(role), strconv.Itoa(queued[role])})
	}

	return table(rows)
}

// renderNoHost names the agent roles no worker host takes, with how much work waits for each. A role
// is listed when the config says no host takes it, or when a queued run no worker has ever picked up
// shows the same thing: that is the spec's "no host for an agent role".
func renderNoHost(runs []factoryRun, cfg config.Config) string {
	queued := map[workflow.Role]int{}
	hosted := map[workflow.Role]bool{}

	for _, run := range runs {
		if run.Status == dagu.StatusQueued {
			queued[run.role]++
		}

		if run.WorkerID != "" {
			hosted[run.role] = true
		}
	}

	rows := make([][]string, 0, 1+len(workflow.KnownRoles()))
	rows = append(rows, []string{headerRole, "QUEUED"})

	for _, role := range workflow.KnownRoles() {
		if cfg.Unhosted(role) || (queued[role] > 0 && !hosted[role]) {
			rows = append(rows, []string{string(role), strconv.Itoa(queued[role])})
		}
	}

	if len(rows) == 1 {
		return noRows
	}

	return table(rows)
}

// renderUsage draws the newest usage records.
func renderUsage(records []usageRecord) string {
	rows := make([][]string, 0, 1+len(records))
	rows = append(rows, []string{
		"REPOSITORY", "ISSUE/PR", headerRole, "MODEL", "HOST", "INPUT", "OUTPUT", "CACHED", "PEAK/CAP", "COST",
		"DURATION", "ATTEMPTS", "OUTCOME", "STARTED",
	})

	for _, record := range records {
		rows = append(rows, usageRow(record))
	}

	if len(rows) == 1 {
		return noRows
	}

	return table(rows)
}

// usageRow is one usage record's columns.
func usageRow(record usageRecord) []string {
	u := record.report

	return []string{
		orNoValue(u.Repository), issueLabel(u.Issue), string(u.AgentRole), orNoValue(u.Model),
		orNoValue(u.WorkerHost), intField(u.Input), intField(u.Output), intField(u.Cached),
		peakField(u), costField(u.Cost), durationField(u.DurationMS), intField(u.Attempts),
		orNoValue(string(u.Outcome)), orNoValue(record.run.StartedAt),
	}
}

// table aligns rows into columns. Writes to a strings.Builder cannot fail.
func table(rows [][]string) string {
	var b strings.Builder

	w := tabwriter.NewWriter(&b, 0, 0, tablePadding, ' ', 0)
	for _, row := range rows {
		_, _ = fmt.Fprintln(w, strings.Join(row, "\t")) // cannot fail: the builder's writes succeed
	}

	// Flushing a tabwriter over a strings.Builder cannot fail: the builder's writes always succeed.
	_ = w.Flush()

	return b.String()
}

// elapsed is how long the run has been going, or a dash when it has not started.
func elapsed(run factoryRun, now time.Time) string {
	start, ok := parseDaguTime(run.StartedAt)
	if !ok || now.Before(start) {
		return noValue
	}

	return now.Sub(start).Round(time.Second).String()
}

// parseDaguTime reads the timestamp formats dagu writes.
func parseDaguTime(text string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, text); err == nil {
			return t.UTC(), true
		}
	}

	return time.Time{}, false
}

// issueLabel is an issue number, or a dash when the run names none.
func issueLabel(number int) string {
	if number <= 0 {
		return noValue
	}

	return "#" + strconv.Itoa(number)
}

// orNoValue is text, or a dash when it is empty.
func orNoValue(text string) string {
	if text == "" {
		return noValue
	}

	return text
}

// intField is an optional token count, or a dash when the adapter could not report it.
func intField(value *int) string {
	if value == nil {
		return noValue
	}

	return strconv.Itoa(*value)
}

// peakField is the peak context against the cap.
func peakField(u opencode.UsageReport) string {
	if u.PeakContext == nil {
		return noValue
	}

	return fmt.Sprintf("%d/%d", *u.PeakContext, u.ContextCap)
}

// durationField is the run's wall-clock time, or a dash when the run could not measure it.
func durationField(ms *int64) string {
	if ms == nil {
		return noValue
	}

	return (time.Duration(*ms) * time.Millisecond).String()
}

// costField is the run's cost, or a dash when the adapter could not report it.
func costField(cost *float64) string {
	if cost == nil {
		return noValue
	}

	return strconv.FormatFloat(*cost, 'f', -1, 64)
}
