package foreman

import (
	"context"
	"slices"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/telemetry"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// stageSpans opens a run's stage spans: children of the run's root span, each carrying the run's
// agent role, repository, issue, model and worker host. A no-op tracer records none of it.
type stageSpans struct {
	tracer trace.Tracer
	attrs  []attribute.KeyValue
}

// runAttrs is what every span of one run carries. An empty worker host or run id stays empty rather
// than being guessed.
func runAttrs(role workflow.Role, ref workflow.IssueRef, model, host, runID string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(telemetry.AttrAgentRole, string(role)),
		attribute.String(telemetry.AttrRepo, ref.Repo.String()),
		attribute.Int(telemetry.AttrIssue, ref.Number),
		attribute.String(telemetry.AttrModel, model),
		attribute.String(telemetry.AttrWorkerHost, host),
	}
	if runID != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrRunID, runID))
	}

	return attrs
}

// spans is the span builder for one run.
func (r run) spans() stageSpans {
	return stageSpans{
		tracer: r.d.tracer(), attrs: runAttrs(r.role, r.spec.ref, r.spec.agent.Model, r.d.Host, r.d.runID()),
	}
}

// start opens a stage span named for the stage. The returned context carries the span, so a stage's
// own steps nest under it.
func (s stageSpans) start(
	ctx context.Context, name string, attrs ...attribute.KeyValue,
) (context.Context, trace.Span) {
	return s.tracer.Start(ctx, name, trace.WithAttributes(append(slices.Clone(s.attrs), attrs...)...))
}

// markFailure marks a span as failed with the run's failure kind. A span leaves the host, so the
// status carries the kind alone: the diagnostic record already has the error text.
func markFailure(span trace.Span, kind opencode.FailureKind) {
	span.SetAttributes(attribute.String(telemetry.AttrFailureKind, string(kind)))
	span.SetStatus(codes.Error, string(kind))
}

// markFailureOf marks a span as failed with the kind the error settles to, the same kind the run's
// outcome reports.
func markFailureOf(span trace.Span, err error) {
	markFailure(span, opencode.KindOf(err))
}

// annotateRun records the run's attributes, its outcome and what it spent on the run's root span. The
// run's root span is the process span for the run verb; a run outside one, or under a no-op tracer,
// records nothing.
func (r run) annotateRun(ctx context.Context, st opencode.RunStats, usage opencode.Usage) {
	attrs := append(runAttrs(r.role, r.spec.ref, r.spec.agent.Model, r.d.Host, r.d.runID()),
		attribute.String(telemetry.AttrOutcome, string(st.Outcome)))

	if st.Attempts > 0 {
		attrs = append(attrs, attribute.Int(telemetry.AttrAttempts, st.Attempts))
	}

	if st.Failure != nil {
		attrs = append(attrs, attribute.String(telemetry.AttrFailureKind, string(*st.Failure)))
	}

	// A token or cost figure no response reported stays absent, never zero.
	if usage.Input != nil {
		attrs = append(attrs, attribute.Int(telemetry.AttrInputTokens, *usage.Input))
	}

	if usage.Output != nil {
		attrs = append(attrs, attribute.Int(telemetry.AttrOutputTokens, *usage.Output))
	}

	if usage.Cached != nil {
		attrs = append(attrs, attribute.Int(telemetry.AttrCachedTokens, *usage.Cached))
	}

	// The peak context is what `status` shows against the cap; the cap itself is config, not telemetry.
	if usage.PeakContext != nil {
		attrs = append(attrs, attribute.Int(telemetry.AttrPeakContext, *usage.PeakContext))
	}

	if usage.Cost != nil {
		attrs = append(attrs, attribute.Float64(telemetry.AttrCost, *usage.Cost))
	}

	trace.SpanFromContext(ctx).SetAttributes(attrs...)
}
