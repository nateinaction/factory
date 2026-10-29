// Package opencode maps foreman's guarantees to opencode v1.19.3: a named agent per agent role, no
// repository config, fresh state per run, a context cap and compaction per agent role, and a usage
// report. It is pure: it builds command lines and environments and reads what opencode printed.
package opencode

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// MaxModelAttempts is one attempt plus the two retries the spec allows for a transient model failure.
const MaxModelAttempts = 3

// Backoff is the wait after the given failed attempt (1-based) and before the next one: 10s, then 30s.
func Backoff(attempt int) time.Duration {
	return [MaxModelAttempts - 1]time.Duration{10 * time.Second, 30 * time.Second}[attempt-1]
}

// InMemoryDB is OPENCODE_DB's value for a run whose state never touches the disk.
const InMemoryDB = ":memory:"

// ProxyAPIKey is the placeholder opencode sends as its provider key. The egress proxy forwards the
// request to the run's key proxy, which replaces the Authorization header with the real OpenRouter
// key. The placeholder is never a secret.
const ProxyAPIKey = "factory-egress-proxy" //nolint:gosec // a placeholder, never a credential

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
	// FailStepCap is reaching the agent role's step cap. Never retried.
	FailStepCap FailureKind = "step cap"
	// FailOther is anything else, including a non-zero exit with no API error. Never retried.
	FailOther FailureKind = "model error"
)

// StepCapReached reports whether a run of steps has used its whole budget of limit. A limit of zero
// is never reached, so an unset cap cannot fail a run.
func StepCapReached(steps, limit int) bool {
	return limit > 0 && steps >= limit
}

// Retryable reports whether the kind is a transient failure.
func (k FailureKind) Retryable() bool {
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

// RetroAllowed reports whether a failed run still gets its retro. A context overflow, running out of
// credits and an unknown model are the exceptions: there is nothing left to resume, or nothing to
// spend on a retro.
func RetroAllowed(err error) bool {
	var me *ModelError
	if !errors.As(err, &me) {
		return true
	}

	switch me.Kind {
	case FailOverflow, FailCredits, FailUnknownModel:
		return false
	case FailRateLimit, FailServer, FailTimeout, FailStepCap, FailOther:
		return true
	}

	return true
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

// Use lets a report from elsewhere win for the totals it carries. PeakContext stays the adapter's:
// the proxy sees responses, not the request context that produced them.
func (u Usage) Use(o Usage) Usage {
	if o.Input != nil {
		u.Input = o.Input
	}

	if o.Output != nil {
		u.Output = o.Output
	}

	if o.Cached != nil {
		u.Cached = o.Cached
	}

	if o.Cost != nil {
		u.Cost = o.Cost
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
	// Steps is how many steps the run took.
	Steps int
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

		s.observe(e)
	}

	return s
}

// observe folds one event into the stream: its session, its step and its usage, or its first failure.
func (s *Stream) observe(e event) {
	if e.SessionID != "" && s.Session == "" {
		s.Session = e.SessionID
	}

	if e.Type == "step_finish" {
		s.Steps++
	}

	switch {
	case e.Type == "step_finish" && e.Part.Tokens != nil:
		s.Usage = s.Usage.Add(stepUsage(e))
	case e.Type == "error" && e.Error != nil && s.Failure == nil:
		k := classify(e.Error.Name, e.Error.Data.StatusCode)
		s.Failure = &k
	}
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

// HTTP statuses the provider reports in an error event. The pure core does not import net/http.
const (
	statusPaymentRequired = 402
	statusNotFound        = 404
	statusRequestTimeout  = 408
	statusTooManyRequests = 429
	statusServerErrorLow  = 500
	statusServerErrorHigh = 599
)

// classify maps an API error to a failure kind.
func classify(name string, status int) FailureKind {
	switch {
	case name == "ContextOverflowError":
		return FailOverflow
	case status == statusTooManyRequests:
		return FailRateLimit
	case status >= statusServerErrorLow && status <= statusServerErrorHigh:
		return FailServer
	case status == statusRequestTimeout || strings.Contains(strings.ToLower(name), "timeout"):
		return FailTimeout
	case status == statusPaymentRequired:
		return FailCredits
	case status == statusNotFound:
		return FailUnknownModel
	}

	return FailOther
}

// Failure decides whether an attempt failed. An API error in the stream fails the run even when
// opencode exits 0; a non-zero exit with no API error is FailOther.
func Failure(s Stream, runErr error) (FailureKind, bool) {
	if s.Failure != nil {
		return *s.Failure, true
	}

	if runErr != nil {
		return FailOther, true
	}

	return "", false
}

// Env is the environment of one opencode process. The inline config turns compaction on or off for
// this agent role and points the OpenRouter provider at the run's proxy; the rest keeps repository
// config and external skills out and state private. The key is never here: the proxy holds it.
func Env(baseURL, stateDB string, a config.AgentConfig) (map[string]string, error) {
	inline, err := json.Marshal(map[string]any{
		"compaction": map[string]bool{"auto": a.Compaction},
		"provider": map[string]any{
			"openrouter": map[string]any{"options": map[string]string{
				// opencode v1.19.3 refuses to call a provider with no key. This placeholder is
				// replaced by the key proxy; the real key is never here.
				"baseURL": baseURL, "apiKey": ProxyAPIKey,
			}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode opencode config: %w", err)
	}

	return map[string]string{
		"OPENCODE_DISABLE_PROJECT_CONFIG":  "1",
		"OPENCODE_DISABLE_EXTERNAL_SKILLS": "1",
		"OPENCODE_DB":                      stateDB,
		"OPENCODE_CONFIG_CONTENT":          string(inline),
	}, nil
}

// Run is one `opencode run` invocation.
type Run struct {
	// Role is the named opencode agent.
	Role workflow.Role
	// Prompt is the whole prompt.
	Prompt string
	// Session resumes this session id; empty starts a new one.
	Session string
	// Timeout is the deadline across every attempt.
	Timeout time.Duration
	// Steps is the step cap across every attempt; zero leaves it uncapped.
	Steps int
}

// Result is what an invocation reported.
type Result struct {
	// Session is the session the model ran in.
	Session string
	// Usage is summed over every attempt, including failed ones.
	Usage Usage
	// Steps is the steps every attempt took together.
	Steps int
}

// Argv is the command line; the prompt is the last argument.
func Argv(r Run) []string {
	argv := []string{"opencode", "run", "--agent", string(r.Role), "--format", "json"}
	if r.Session != "" {
		argv = append(argv, "--session", r.Session)
	}

	return append(argv, r.Prompt)
}

// UsageReport is the one JSON line a run logs to the dagu run log. A pointer or omitted field is one
// the adapter could not report; Missing lists them so absence is never read as zero.
type UsageReport struct {
	Event       string        `json:"event"`
	Repository  string        `json:"repository"`
	Issue       int           `json:"issue"`
	AgentRole   workflow.Role `json:"agent_role"`
	Model       string        `json:"model"`
	WorkerHost  string        `json:"worker_host,omitempty"`
	Input       *int          `json:"input_tokens,omitempty"`
	Output      *int          `json:"output_tokens,omitempty"`
	Cached      *int          `json:"cached_tokens,omitempty"`
	ContextCap  int           `json:"context_cap"`
	PeakContext *int          `json:"peak_context,omitempty"`
	Cost        *float64      `json:"cost,omitempty"`
	Domains     []string      `json:"domains,omitempty"`
	Missing     []string      `json:"missing"`
}

// WithDomains records every domain the run contacted. A nil slice means the adapter could not
// observe egress, and domains stays listed as missing rather than reported as none.
func (r UsageReport) WithDomains(domains []string) UsageReport {
	if domains == nil {
		return r
	}

	r.Domains = domains
	r.Missing = slices.DeleteFunc(r.Missing, func(field string) bool { return field == "domains" })

	return r
}

// NewUsageReport builds the report. Domains stays missing until the egress proxy reports them
// through WithDomains.
func NewUsageReport(
	role workflow.Role, ref workflow.IssueRef, host string, a config.AgentConfig, u Usage,
) UsageReport {
	r := UsageReport{
		Event: "usage", Repository: ref.Repo.String(), Issue: ref.Number, AgentRole: role, Model: a.Model, WorkerHost: host,
		Input: u.Input, Output: u.Output, Cached: u.Cached, ContextCap: a.ContextCap, PeakContext: u.PeakContext,
		Cost: u.Cost, Missing: []string{},
	}
	for _, f := range []struct {
		name    string
		missing bool
	}{
		{"worker_host", host == ""},
		{"input_tokens", u.Input == nil},
		{"output_tokens", u.Output == nil},
		{"cached_tokens", u.Cached == nil},
		{"peak_context", u.PeakContext == nil},
		{"cost", u.Cost == nil},
		{"domains", true},
	} {
		if f.missing {
			r.Missing = append(r.Missing, f.name)
		}
	}

	return r
}
