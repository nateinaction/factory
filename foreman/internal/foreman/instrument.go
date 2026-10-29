package foreman

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/security"
	"github.com/nateinaction/factory/foreman/internal/telemetry"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// pollInstruments are one poll's measurements. Every method is safe on a no-op meter: a measurement
// never fails or delays the poll. The alert instruments are built once here too, so a poll that reaps
// a dead run records it with the same instruments the rest of the poll holds.
type pollInstruments struct {
	issues   metric.Int64Counter
	claims   metric.Int64Counter
	capacity metric.Int64Gauge
	alerts   alertInstruments
}

// newPollInstruments builds the poll's instruments. An instrument that cannot be built records
// nowhere rather than returning an error, because telemetry never fails a poll.
func newPollInstruments(d Deps) pollInstruments {
	m := d.meter()

	return pollInstruments{
		issues: telemetry.Int64Counter(m, telemetry.MetricPollIssues,
			"Open issues in each factory state, per repository."),
		claims: telemetry.Int64Counter(m, telemetry.MetricPollClaims,
			"Issues a poll claimed, per agent role and repository."),
		capacity: telemetry.Int64Gauge(m, telemetry.MetricPollCapacity,
			"Run capacity left for an agent role."),
		alerts: newAlertInstruments(d),
	}
}

// alertInstruments are the measurements the alert rules read. Every instrument is safe on a no-op
// meter, so recording one never fails or delays a run. A per-event measurement carries the issue and
// the trace that raised it, so its alert can name the issue and link straight to the trace; an
// aggregate measurement carries only the agent role and repository.
type alertInstruments struct {
	security metric.Int64Counter
	outcomes metric.Int64Counter
	cost     metric.Float64Counter
	reaped   metric.Int64Counter
}

// newAlertInstruments builds the alert rules' instruments.
func newAlertInstruments(d Deps) alertInstruments {
	m := d.meter()

	return alertInstruments{
		security: telemetry.Int64Counter(m, telemetry.MetricSecurityEvents,
			"Security events by agent role, repository, issue, event, outcome and trace."),
		outcomes: telemetry.Int64Counter(m, telemetry.MetricRunOutcomes,
			"Finished runs by agent role, repository and outcome."),
		cost: telemetry.Float64Counter(m, telemetry.MetricRunCost,
			"Model cost in US dollars by agent role, repository and model."),
		reaped: telemetry.Int64Counter(m, telemetry.MetricRunsReaped,
			"Dead runs a poll called failed by agent role and repository."),
	}
}

// securityEvent records one security event raised by the run working issue.
func (a alertInstruments) securityEvent(
	ctx context.Context, role workflow.Role, repo workflow.Repo, issue int, event string, outcome security.Outcome,
) {
	a.securityEvents(ctx, role, repo, issue, event, outcome, 1)
}

// securityEvents records n refused requests of one event and outcome, raised by one run. The issue
// and the trace id are labels on purpose: #132's acceptance criterion is that a per-event alert names
// the issue and links to the trace that raised it, and a Prometheus rule can template only labels.
// Every other metric keeps the issue out: see telemetry.AttrIssue.
func (a alertInstruments) securityEvents(
	ctx context.Context, role workflow.Role, repo workflow.Repo, issue int, event string,
	outcome security.Outcome, n int64,
) {
	if n <= 0 {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String(telemetry.AttrAgentRole, string(role)),
		attribute.String(telemetry.AttrRepo, repo.String()),
		attribute.String(telemetry.AttrEvent, event),
		attribute.String(telemetry.AttrOutcome, string(outcome)),
	}
	if issue > 0 {
		attrs = append(attrs, attribute.Int(telemetry.AttrIssue, issue))
	}

	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs, attribute.String(telemetry.AttrTraceID, sc.TraceID().String()))
	}

	a.security.Add(ctx, n, metric.WithAttributes(attrs...))
}

// runOutcome records how one run ended.
func (a alertInstruments) runOutcome(
	ctx context.Context, role workflow.Role, repo workflow.Repo, outcome opencode.Outcome,
) {
	a.outcomes.Add(ctx, 1, metric.WithAttributes(
		attribute.String(telemetry.AttrAgentRole, string(role)),
		attribute.String(telemetry.AttrRepo, repo.String()),
		attribute.String(telemetry.AttrOutcome, string(outcome)),
	))
}

// runCost records what the run's model calls cost. A cost no response reported records nothing.
func (a alertInstruments) runCost(
	ctx context.Context, role workflow.Role, repo workflow.Repo, model string, cost *float64,
) {
	if cost == nil || *cost <= 0 {
		return
	}

	a.cost.Add(ctx, *cost, metric.WithAttributes(
		attribute.String(telemetry.AttrAgentRole, string(role)),
		attribute.String(telemetry.AttrRepo, repo.String()),
		attribute.String(telemetry.AttrModel, model),
	))
}

// runReaped records one dead run a poll called failed.
func (a alertInstruments) runReaped(ctx context.Context, role workflow.Role, repo workflow.Repo) {
	a.reaped.Add(ctx, 1, metric.WithAttributes(
		attribute.String(telemetry.AttrAgentRole, string(role)),
		attribute.String(telemetry.AttrRepo, repo.String()),
	))
}

// recordAlerts records what the alert rules read for one finished run: its outcome and cost, and
// every request its two proxies refused, by event and outcome. The alerts instruments are the run's
// own, built once when the run started. The run's issue and trace ride on each security event, so its
// alert reaches both directly.
func (r run) recordAlerts(
	ctx context.Context, st opencode.RunStats, usage opencode.Usage,
	egress, key map[security.Outcome]int,
) {
	alerts := r.alerts

	alerts.runOutcome(ctx, r.role, r.spec.ref.Repo, st.Outcome)
	alerts.runCost(ctx, r.role, r.spec.ref.Repo, r.spec.agent.Model, usage.Cost)

	for outcome, n := range egress {
		alerts.securityEvents(
			ctx, r.role, r.spec.ref.Repo, r.spec.ref.Number, security.EventEgressRefused, outcome, int64(n),
		)
	}

	for outcome, n := range key {
		alerts.securityEvents(
			ctx, r.role, r.spec.ref.Repo, r.spec.ref.Number, security.EventKeyProxyRefused, outcome, int64(n),
		)
	}
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
