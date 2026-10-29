package foreman

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"

	"github.com/nateinaction/factory/foreman/internal/telemetry"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// pollInstruments are one poll's measurements. Every method is safe on a no-op meter: a measurement
// never fails or delays the poll.
type pollInstruments struct {
	issues   metric.Int64Counter
	claims   metric.Int64Counter
	capacity metric.Int64Gauge
}

// newPollInstruments builds the poll's instruments. An instrument that cannot be built records
// nowhere rather than returning an error, because telemetry never fails a poll.
func newPollInstruments(d Deps) pollInstruments {
	m := d.meter()

	return pollInstruments{
		issues:   int64Counter(m, telemetry.MetricPollIssues, "Open issues in each factory state, per repository."),
		claims:   int64Counter(m, telemetry.MetricPollClaims, "Issues a poll claimed, per agent role and repository."),
		capacity: int64Gauge(m, telemetry.MetricPollCapacity, "Run capacity left for an agent role."),
	}
}

// int64Counter builds a counter, falling back to a no-op instrument the meter refuses it.
func int64Counter(m metric.Meter, name, description string) metric.Int64Counter {
	counter, err := m.Int64Counter(name, metric.WithDescription(description))
	if err != nil {
		counter, _ = metricnoop.NewMeterProvider().Meter("").Int64Counter(name)

		return counter
	}

	return counter
}

// int64Gauge builds a gauge, falling back to a no-op instrument the meter refuses it.
func int64Gauge(m metric.Meter, name, description string) metric.Int64Gauge {
	gauge, err := m.Int64Gauge(name, metric.WithDescription(description))
	if err != nil {
		gauge, _ = metricnoop.NewMeterProvider().Meter("").Int64Gauge(name)

		return gauge
	}

	return gauge
}

// issuesInState records how many open issues one repository has in a factory state. Issue numbers,
// SHAs and domains are never labels: the dimensions are the repository, the state and the role that
// owns it.
func (i pollInstruments) issuesInState(
	ctx context.Context, repo workflow.Repo, state workflow.State, role workflow.Role, count int64,
) {
	if count == 0 {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String(telemetry.AttrRepo, repo.String()),
		attribute.String(telemetry.AttrState, string(state)),
	}
	if role != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrAgentRole, string(role)))
	}

	i.issues.Add(ctx, count, metric.WithAttributes(attrs...))
}

// claim records one issue claimed for an agent role in a repository.
func (i pollInstruments) claim(ctx context.Context, repo workflow.Repo, role workflow.Role) {
	i.claims.Add(ctx, 1, metric.WithAttributes(
		attribute.String(telemetry.AttrRepo, repo.String()),
		attribute.String(telemetry.AttrAgentRole, string(role)),
	))
}

// capacityLeft records the runs an agent role can still start. Capacity is fleet-wide, so the only
// dimension is the agent role.
func (i pollInstruments) capacityLeft(ctx context.Context, role workflow.Role, left int) {
	i.capacity.Record(ctx, int64(left),
		metric.WithAttributes(attribute.String(telemetry.AttrAgentRole, string(role))))
}

// pollTraces holds one poll's span per assigned repository. The spans stay open through the claim
// phase, so each repository's span carries the claims and enqueues its issues produced.
type pollTraces struct {
	spans map[workflow.Repo]trace.Span
}

// newPollTraces starts an empty trace set.
func newPollTraces() *pollTraces {
	return &pollTraces{spans: map[workflow.Repo]trace.Span{}}
}

// set records a repository's span.
func (t *pollTraces) set(repo workflow.Repo, span trace.Span) {
	t.spans[repo] = span
}

// context is parent with the repository's span in it. A repository without a span keeps parent.
func (t *pollTraces) context(parent context.Context, repo workflow.Repo) context.Context {
	if span, ok := t.spans[repo]; ok {
		return trace.ContextWithSpan(parent, span)
	}

	return parent
}

// fail marks a repository's span as an error.
func (t *pollTraces) fail(repo workflow.Repo, reason string) {
	if span, ok := t.spans[repo]; ok {
		failSpan(span, reason)
	}
}

// end ends every repository span.
func (t *pollTraces) end() {
	for _, span := range t.spans {
		span.End()
	}
}

// startRepoSpan opens the span for one assigned repository's poll.
func startRepoSpan(ctx context.Context, d Deps, repo workflow.Repo) (context.Context, trace.Span) {
	return d.tracer().Start(ctx, telemetry.PollRepoSpanName,
		trace.WithAttributes(attribute.String(telemetry.AttrRepo, repo.String())))
}

// startEnqueueSpan opens the span for one claimed issue's enqueue. A run continues this span's trace
// through the DAG's parameters, so its name and attributes are stable across that handoff.
func startEnqueueSpan(
	ctx context.Context, d Deps, repo workflow.Repo, role workflow.Role, issue int,
) (context.Context, trace.Span) {
	return d.tracer().Start(ctx, telemetry.PollEnqueueSpanName, trace.WithAttributes(
		attribute.String(telemetry.AttrRepo, repo.String()),
		attribute.String(telemetry.AttrAgentRole, string(role)),
		attribute.Int(telemetry.AttrIssue, issue),
	))
}

// failSpan marks a span as failed with a fixed reason, never the underlying error text.
func failSpan(span trace.Span, reason string) {
	span.SetStatus(codes.Error, reason)
}

// queueSkipped records why a poll left an agent role's queue alone. It is an event on the active
// span, so the poll's trace says what the log line says.
func queueSkipped(ctx context.Context, role workflow.Role, reason string) {
	trace.SpanFromContext(ctx).AddEvent(telemetry.EventQueueSkipped, trace.WithAttributes(
		attribute.String(telemetry.AttrAgentRole, string(role)),
		attribute.String(telemetry.AttrSkipReason, reason),
	))
}
