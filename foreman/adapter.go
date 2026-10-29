package foreman

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// The opencode adapter maps foreman's guarantees to opencode v1.19.3: a named agent per agent role, no
// repository config, fresh state per run, a context cap and compaction per agent role, and a usage report.

// MaxModelAttempts is one attempt plus the two retries the spec allows for a transient model failure.
const MaxModelAttempts = 3

// backoffs are the waits before the second and third attempt.
var backoffs = [MaxModelAttempts - 1]time.Duration{10 * time.Second, 30 * time.Second}

// InMemoryDB is OPENCODE_DB's value for a run whose state never touches the disk.
const InMemoryDB = ":memory:"

// FailureKind says why a model run failed. Its text is safe to put in a comment: it names the kind and
// carries nothing from the response.
type FailureKind string

// The kinds of model failure the spec distinguishes.
const (
	// FailRateLimit is HTTP 429. Retried.
	FailRateLimit FailureKind = "rate limit"
	// FailServer is a 5xx. Retried.
	FailServer FailureKind = "server error"
	// FailTimeout is a provider timeout. Retried.
	FailTimeout FailureKind = "timeout"
	// FailCredits is HTTP 402. Never retried.
	FailCredits FailureKind = "out of credits"
	// FailUnknownModel is HTTP 404. Never retried.
	FailUnknownModel FailureKind = "unknown model"
	// FailOverflow is a context overflow. With compaction off it fails the run, like a step cap.
	FailOverflow FailureKind = "context overflow"
	// FailOther is anything else, including a non-zero exit with no API error. Never retried.
	FailOther FailureKind = "model error"
)

// retryable reports whether the kind is a transient failure.
func (k FailureKind) retryable() bool {
	return k == FailRateLimit || k == FailServer || k == FailTimeout
}

// ModelError is a failed model run.
type ModelError struct {
	// Kind is the failure.
	Kind FailureKind
	// Attempts is how many times the model was called.
	Attempts int
	// Cause is the process error when the model reported no API error: an exit code or a deadline,
	// never response text.
	Cause error
}

// Error names the kind and the attempts, never the response.
func (e *ModelError) Error() string {
	msg := fmt.Sprintf("model run failed: %s after %d attempt(s)", e.Kind, e.Attempts)
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap exposes the cause.
func (e *ModelError) Unwrap() error { return e.Cause }

// AgentConfig is one agent role's model settings, rendered from `factory_models` by Ansible.
type AgentConfig struct {
	// Model is the OpenRouter model id, for the usage report.
	Model string `json:"model"`
	// ContextCap is the input-token cap the model alias carries.
	ContextCap int `json:"context_cap"`
	// Compaction is whether opencode compacts instead of overflowing.
	Compaction bool `json:"compaction"`
}

// Usage is what a run spent. A nil field is one the adapter could not report; it is never zero.
type Usage struct {
	// Input, Output and Cached are token totals over every step.
	Input, Output, Cached *int
	// PeakContext is the largest context any one step used.
	PeakContext *int
	// Cost is OpenRouter's usage.cost summed over the steps.
	Cost *float64
}

// addInt sums two optional ints: missing only when both are.
func addInt(a, b *int) *int {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	sum := *a + *b
	return &sum
}

// maxInt is the larger of two optional ints.
func maxInt(a, b *int) *int {
	if a == nil || (b != nil && *b > *a) {
		return b
	}
	return a
}

// Add combines the usage of two attempts or phases of one run.
func (u Usage) Add(o Usage) Usage {
	u.Input, u.Output, u.Cached = addInt(u.Input, o.Input), addInt(u.Output, o.Output), addInt(u.Cached, o.Cached)
	u.PeakContext = maxInt(u.PeakContext, o.PeakContext)
	switch {
	case u.Cost == nil:
		u.Cost = o.Cost
	case o.Cost != nil:
		sum := *u.Cost + *o.Cost
		u.Cost = &sum
	}
	return u
}

// event is one line of `opencode run --format json`, reduced to what the adapter reads.
type event struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Part      struct {
		Cost   *float64 `json:"cost"`
		Tokens *struct {
			Input  *int `json:"input"`
			Output *int `json:"output"`
			Cache  *struct {
				Read *int `json:"read"`
			} `json:"cache"`
		} `json:"tokens"`
	} `json:"part"`
	Error *struct {
		Name string `json:"name"`
		Data struct {
			StatusCode int `json:"statusCode"`
		} `json:"data"`
	} `json:"error"`
}

// Stream is what one opencode process reported.
type Stream struct {
	// Session is the session id, empty when none was reported.
	Session string
	// Usage is summed over the stream's steps.
	Usage Usage
	// Failure is the first API error in the stream; nil when there was none.
	Failure *FailureKind
}

// maxStreamLine bounds one line of opencode's output.
const maxStreamLine = 4 << 20

// ParseStream reads opencode's JSON lines. Lines that are not events are ignored: the stream is
// best-effort telemetry, and the exit code and error events decide failure.
func ParseStream(out string) Stream {
	var s Stream
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(nil, maxStreamLine)
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue // not an event line
		}
		if e.SessionID != "" && s.Session == "" {
			s.Session = e.SessionID
		}
		switch {
		case e.Type == "step_finish" && e.Part.Tokens != nil:
			s.Usage = s.Usage.Add(stepUsage(e))
		case e.Type == "error" && e.Error != nil && s.Failure == nil:
			k := classify(e.Error.Name, e.Error.Data.StatusCode)
			s.Failure = &k
		}
	}
	return s
}

// stepUsage is one step's usage. A step's context is its input plus the cached tokens it read.
func stepUsage(e event) Usage {
	t := e.Part.Tokens
	u := Usage{Input: t.Input, Output: t.Output, Cost: e.Part.Cost}
	if t.Cache != nil {
		u.Cached = t.Cache.Read
	}
	if t.Input != nil {
		peak := *t.Input
		if u.Cached != nil {
			peak += *u.Cached
		}
		u.PeakContext = &peak
	}
	return u
}

// classify maps an API error to a failure kind.
func classify(name string, status int) FailureKind {
	switch {
	case name == "ContextOverflowError":
		return FailOverflow
	case status == 429:
		return FailRateLimit
	case status >= 500 && status <= 599:
		return FailServer
	case status == 408 || strings.Contains(strings.ToLower(name), "timeout"):
		return FailTimeout
	case status == 402:
		return FailCredits
	case status == 404:
		return FailUnknownModel
	}
	return FailOther
}

// AgentEnv is the environment of one opencode process. The inline config turns compaction on or off
// for this agent role; the rest keeps repository config and external skills out and state private.
func AgentEnv(openrouterKey, stateDB string, a AgentConfig) (map[string]string, error) {
	inline, err := json.Marshal(map[string]any{"compaction": map[string]bool{"auto": a.Compaction}})
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"OPENROUTER_API_KEY":               openrouterKey,
		"OPENCODE_DISABLE_PROJECT_CONFIG":  "1",
		"OPENCODE_DISABLE_EXTERNAL_SKILLS": "1",
		"OPENCODE_DB":                      stateDB,
		"OPENCODE_CONFIG_CONTENT":          string(inline),
	}, nil
}

// AgentRun is one `opencode run` invocation.
type AgentRun struct {
	// Role is the named opencode agent.
	Role Role
	// Prompt is the whole prompt.
	Prompt string
	// Session resumes this session id; empty starts a new one.
	Session string
	// Timeout is the deadline across every attempt.
	Timeout time.Duration
}

// AgentResult is what an invocation reported.
type AgentResult struct {
	// Session is the session the model ran in.
	Session string
	// Usage is summed over every attempt, including failed ones.
	Usage Usage
}

// agentArgv is the command line; the prompt is the last argument.
func agentArgv(r AgentRun) []string {
	argv := []string{"opencode", "run", "--agent", string(r.Role), "--format", "json"}
	if r.Session != "" {
		argv = append(argv, "--session", r.Session)
	}
	return append(argv, r.Prompt)
}

// agent runs the model, retrying transient failures up to twice within r.Timeout.
func (s session) agent(r AgentRun) (AgentResult, error) {
	ctx, cancel := context.WithTimeout(s.ctx, r.Timeout)
	defer cancel()
	deadline, _ := ctx.Deadline() // WithTimeout always sets one
	var res AgentResult
	for attempt := 1; attempt <= MaxModelAttempts; attempt++ {
		stream, err := s.attempt(ctx, r, time.Until(deadline))
		res.Usage = res.Usage.Add(stream.Usage)
		if stream.Session != "" {
			res.Session = stream.Session
		}
		kind, failed := failure(stream, err)
		if !failed {
			return res, nil
		}
		var cause error
		if stream.Failure == nil {
			cause = err
		}
		if ctx.Err() != nil {
			return res, fmt.Errorf("%s: %w", r.Role, ctx.Err())
		}
		if !kind.retryable() || attempt == MaxModelAttempts {
			return res, &ModelError{Kind: kind, Attempts: attempt, Cause: cause}
		}
		if err := s.d.sleep(ctx, backoffs[attempt-1]); err != nil {
			return res, fmt.Errorf("%s: %w", r.Role, err)
		}
	}
	return res, &ModelError{Kind: FailOther, Attempts: MaxModelAttempts} // unreachable: the loop returns
}

// attempt runs opencode once and parses what it printed. The stream goes to the dagu log with the
// OpenRouter key removed.
func (s session) attempt(ctx context.Context, r AgentRun, timeout time.Duration) (Stream, error) {
	out, err := s.d.Runner.Run(ctx, Cmd{
		Argv: agentArgv(r), Dir: s.workdir, Env: s.agentEnv, Timeout: timeout, Capture: true,
	})
	if out != "" {
		s.d.logf("%s", s.redact(out))
	}
	return ParseStream(out), err
}

// redact removes the OpenRouter key from text bound for a log.
func (s session) redact(text string) string {
	key := s.agentEnv["OPENROUTER_API_KEY"]
	if key == "" {
		return text
	}
	return strings.ReplaceAll(text, key, "[redacted]")
}

// failure decides whether an attempt failed. An API error in the stream fails the run even when
// opencode exits 0; a non-zero exit with no API error is FailOther.
func failure(s Stream, runErr error) (FailureKind, bool) {
	if s.Failure != nil {
		return *s.Failure, true
	}
	if runErr != nil {
		return FailOther, true
	}
	return "", false
}

// withState gives fn a fresh state database for the role's run and deletes it afterwards. The
// Reviewer's state is in memory only. A leftover from an earlier run is removed first.
func withState(root string, role Role, number int, fn func(db string) error) (err error) {
	if role == Reviewer {
		return fn(InMemoryDB)
	}
	dir, err := freshPath(root, fmt.Sprintf("state-%s-%d", role, number))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	return fn(dir + "/opencode.db")
}

// UsageReport is the one JSON line a run logs to the dagu run log. A pointer or omitted field is one
// the adapter could not report; Missing lists them so absence is never read as zero.
type UsageReport struct {
	Event       string   `json:"event"`
	Repository  string   `json:"repository"`
	Issue       int      `json:"issue"`
	AgentRole   Role     `json:"agent_role"`
	Model       string   `json:"model"`
	WorkerHost  string   `json:"worker_host,omitempty"`
	Input       *int     `json:"input_tokens,omitempty"`
	Output      *int     `json:"output_tokens,omitempty"`
	Cached      *int     `json:"cached_tokens,omitempty"`
	ContextCap  int      `json:"context_cap"`
	PeakContext *int     `json:"peak_context,omitempty"`
	Cost        *float64 `json:"cost,omitempty"`
	Domains     []string `json:"domains,omitempty"`
	Missing     []string `json:"missing"`
}

// NewUsageReport builds the report. The adapter cannot yet observe contacted domains (the sandbox
// slice will), so they are always listed as missing.
func NewUsageReport(role Role, repo string, number int, host string, a AgentConfig, u Usage) UsageReport {
	r := UsageReport{
		Event: "usage", Repository: repo, Issue: number, AgentRole: role, Model: a.Model, WorkerHost: host,
		Input: u.Input, Output: u.Output, Cached: u.Cached, ContextCap: a.ContextCap, PeakContext: u.PeakContext,
		Cost: u.Cost, Missing: []string{},
	}
	for _, f := range []struct {
		name    string
		missing bool
	}{
		{"worker_host", host == ""}, {"input_tokens", u.Input == nil}, {"output_tokens", u.Output == nil},
		{"cached_tokens", u.Cached == nil}, {"peak_context", u.PeakContext == nil}, {"cost", u.Cost == nil},
		{"domains", true},
	} {
		if f.missing {
			r.Missing = append(r.Missing, f.name)
		}
	}
	return r
}

// logUsage writes the run's single usage line to stdout, which dagu captures in the run log.
func (d Deps) logUsage(r UsageReport) {
	line, err := json.Marshal(r)
	if err != nil {
		d.logf("usage report: %v", err)
		return
	}
	if err := writeLine(d.Out, string(line)); err != nil {
		d.logf("usage report: %v", err)
	}
}

// sleep waits for d or until ctx ends. Tests replace it through Deps.Sleep.
func (d Deps) sleep(ctx context.Context, wait time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, wait)
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
