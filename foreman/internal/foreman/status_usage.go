package foreman

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/telemetry"
)

// Ceilings on a read of the telemetry backend. The backend is best effort: a read that fails or is
// slow leaves `status` reporting the local record instead.
const (
	// TraceTimeout bounds one request to the backend.
	TraceTimeout = 10 * time.Second
	// TraceBytes bounds one response, so a misbehaving backend cannot exhaust the host's memory.
	TraceBytes = 8 << 20
	// runSpanName is the run verb's process span: the span that carries the run's usage.
	runSpanName = "run"
)

// tempoClient reads one run's trace from Tempo's HTTP API.
type tempoClient struct {
	base string
	http *http.Client
}

// newTempoClient builds a client for the configured backend. The URL is read at the edge and passed
// inward; an empty URL is never turned into a client.
func newTempoClient(base string) tempoClient {
	return tempoClient{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: TraceTimeout}}
}

// traceUsage reads the usage report the run's root span carries. ok is false when the backend has no
// such trace, which is a run from before telemetry or one the backend has already pruned.
func traceUsage(
	ctx context.Context, d Deps, cfg config.Config, run factoryRun,
) (usageRecord, bool) {
	client := newTempoClient(d.Settings.TelemetryURL)

	trace, err := client.traceFor(ctx, run.RunID)
	if err != nil {
		d.log().Warn("usage record: cannot read the backend trace", "run", run.RunID, "error", err)

		return usageRecord{}, false
	}

	span, ok := runSpan(trace)
	if !ok {
		d.log().Debug("usage record: the backend has no run span", "run", run.RunID)

		return usageRecord{}, false
	}

	report, ok := usageFromSpan(run, cfg, span)
	if !ok {
		d.log().Debug("usage record: the run span carries no outcome", "run", run.RunID)

		return usageRecord{}, false
	}

	return usageRecord{run: run, report: report}, true
}

// tempoSearch is the part of Tempo's search response this reads: the trace a run id belongs to.
type tempoSearch struct {
	Traces []struct {
		TraceID string `json:"traceID"`
	} `json:"traces"`
}

// tempoTrace is one trace, in either shape Tempo serves: the OTLP JSON of /api/v2/traces, or the
// batches object older releases serve from /api/traces. Both are read, so the pinned Tempo and a
// marginally older one both work.
type tempoTrace struct {
	ResourceSpans []tempoScopeHolder `json:"resourceSpans"`
	Batches       []tempoScopeHolder `json:"batches"`
}

// tempoScopeHolder is one resource's spans.
type tempoScopeHolder struct {
	ScopeSpans []struct {
		Spans []tempoSpan `json:"spans"`
	} `json:"scopeSpans"`
}

// tempoSpan is the part of one span this reads. OTLP JSON encodes every 64-bit value as a string.
type tempoSpan struct {
	Name              string           `json:"name"`
	StartTimeUnixNano string           `json:"startTimeUnixNano"`
	EndTimeUnixNano   string           `json:"endTimeUnixNano"`
	Attributes        []tempoAttribute `json:"attributes"`
}

// tempoAttribute is one OTLP JSON attribute.
type tempoAttribute struct {
	Key   string `json:"key"`
	Value struct {
		StringValue *string  `json:"stringValue"`
		IntValue    *string  `json:"intValue"`
		DoubleValue *float64 `json:"doubleValue"`
	} `json:"value"`
}

// traceFor finds the trace one run belongs to and reads it.
func (c tempoClient) traceFor(ctx context.Context, runID string) (tempoTrace, error) {
	if !validRunID.MatchString(runID) {
		return tempoTrace{}, fmt.Errorf("backend trace: run id %q is not a plain name", runID)
	}

	query := url.Values{
		"q":     {fmt.Sprintf("{ span.%s = %q }", telemetry.AttrRunID, runID)},
		"limit": {"1"},
	}

	var found tempoSearch
	if err := c.get(ctx, "/api/search?"+query.Encode(), &found); err != nil {
		return tempoTrace{}, err
	}

	if len(found.Traces) == 0 || found.Traces[0].TraceID == "" {
		return tempoTrace{}, nil
	}

	var trace tempoTrace
	if err := c.get(ctx, "/api/v2/traces/"+url.PathEscape(found.Traces[0].TraceID), &trace); err != nil {
		return tempoTrace{}, err
	}

	return trace, nil
}

// get reads one JSON document from the backend, bounded by TraceBytes.
func (c tempoClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return fmt.Errorf("backend request: %w", err)
	}

	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("backend request: %w", err)
	}

	defer func() {
		// Reading already finished or failed; a close error cannot change the parsed document.
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("backend %s: %s", path, resp.Status)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, TraceBytes))
	if err != nil {
		return fmt.Errorf("read backend response: %w", err)
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("parse backend response: %w", err)
	}

	return nil
}

// runSpan finds the span named for the run verb, the one the run's usage is annotated on.
func runSpan(trace tempoTrace) (tempoSpan, bool) {
	for _, holder := range append(trace.ResourceSpans, trace.Batches...) {
		for _, scope := range holder.ScopeSpans {
			for _, span := range scope.Spans {
				if span.Name == telemetry.RootSpanName(runSpanName) {
					return span, true
				}
			}
		}
	}

	return tempoSpan{}, false
}

// usageFromSpan builds the usage report a run's root span carries. ok is false when the span is not a
// finished run: it has no outcome.
func usageFromSpan(run factoryRun, cfg config.Config, span tempoSpan) (opencode.UsageReport, bool) {
	attrs := span.attributes()

	outcome := attrs.string(telemetry.AttrOutcome)
	if outcome == "" {
		return opencode.UsageReport{}, false
	}

	report := opencode.UsageReport{
		Event:       "usage",
		Repository:  orElse(attrs.string(telemetry.AttrRepo), run.repo),
		Issue:       orElseInt(attrs.int(telemetry.AttrIssue), run.issue),
		AgentRole:   run.role,
		Model:       attrs.string(telemetry.AttrModel),
		WorkerHost:  attrs.string(telemetry.AttrWorkerHost),
		Input:       attrs.optionalInt(telemetry.AttrInputTokens),
		Output:      attrs.optionalInt(telemetry.AttrOutputTokens),
		Cached:      attrs.optionalInt(telemetry.AttrCachedTokens),
		PeakContext: attrs.optionalInt(telemetry.AttrPeakContext),
		Cost:        attrs.optionalFloat(telemetry.AttrCost),
		Attempts:    attrs.optionalInt(telemetry.AttrAttempts),
		FailureKind: opencode.FailureKind(attrs.string(telemetry.AttrFailureKind)),
		Outcome:     opencode.Outcome(outcome),
		ContextCap:  cfg.Agents[string(run.role)].ContextCap,
	}

	if ms, ok := span.durationMS(); ok {
		report.DurationMS = &ms
	}

	// The backend carries what the span carries; a figure the run did not report stays missing rather
	// than reading as zero.
	for _, field := range []struct {
		name    string
		missing bool
	}{
		{"worker_host", report.WorkerHost == ""},
		{"input_tokens", report.Input == nil},
		{"output_tokens", report.Output == nil},
		{"cached_tokens", report.Cached == nil},
		{"peak_context", report.PeakContext == nil},
		{"cost", report.Cost == nil},
		{"duration_ms", report.DurationMS == nil},
		{"attempts", report.Attempts == nil},
		{"domains", true}, // the trace does not carry the egress proxy's domains
	} {
		if field.missing {
			report.Missing = append(report.Missing, field.name)
		}
	}

	return report, true
}

// attributes indexes a span's attributes for reading.
func (s tempoSpan) attributes() tempoAttrs {
	attrs := make(tempoAttrs, len(s.Attributes))

	for _, attr := range s.Attributes {
		attrs[attr.Key] = attr
	}

	return attrs
}

// durationMS is the span's wall-clock time, or false when its timestamps are not readable.
func (s tempoSpan) durationMS() (int64, bool) {
	start, err := strconv.ParseInt(s.StartTimeUnixNano, 10, 64)
	if err != nil {
		return 0, false
	}

	end, err := strconv.ParseInt(s.EndTimeUnixNano, 10, 64)
	if err != nil || end < start {
		return 0, false
	}

	return (end - start) / int64(time.Millisecond), true
}

// tempoAttrs is one span's attributes by key.
type tempoAttrs map[string]tempoAttribute

// string is a string attribute, or empty when it is absent or another type.
func (a tempoAttrs) string(key string) string {
	attr, ok := a[key]
	if !ok || attr.Value.StringValue == nil {
		return ""
	}

	return *attr.Value.StringValue
}

// int is an integer attribute, or zero when it is absent or another type.
func (a tempoAttrs) int(key string) int {
	if value := a.optionalInt(key); value != nil {
		return *value
	}

	return 0
}

// optionalInt is an integer attribute, or nil when it is absent or not an integer.
func (a tempoAttrs) optionalInt(key string) *int {
	attr, ok := a[key]
	if !ok || attr.Value.IntValue == nil {
		return nil
	}

	value, err := strconv.Atoi(*attr.Value.IntValue)
	if err != nil {
		return nil
	}

	return &value
}

// optionalFloat is a float attribute, or nil when it is absent or not a float.
func (a tempoAttrs) optionalFloat(key string) *float64 {
	attr, ok := a[key]
	if !ok || attr.Value.DoubleValue == nil {
		return nil
	}

	return attr.Value.DoubleValue
}

// orElse is text, or fallback when it is empty.
func orElse(text, fallback string) string {
	if text == "" {
		return fallback
	}

	return text
}

// orElseInt is a number, or fallback when it is zero.
func orElseInt(number, fallback int) int {
	if number == 0 {
		return fallback
	}

	return number
}
