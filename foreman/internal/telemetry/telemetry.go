// Package telemetry is foreman's OpenTelemetry edge: it builds the tracer and meter that Deps carries,
// exports over OTLP/HTTP as the standard OTEL_* environment variables configure, and flushes on exit
// under a fixed bound. Telemetry never fails or delays a run: every failure here degrades to a no-op
// and a debug record.
package telemetry

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0" // pinned: the GenAI conventions are still in development
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// FlushBound is the longest the exit flush waits for the collector, however many exporters there are.
const FlushBound = 5 * time.Second

// scope names the instrumentation foreman's tracer and meter report under.
const scope = "github.com/nateinaction/factory/foreman"

// serviceName is the service.name unless OTEL_SERVICE_NAME overrides it.
const serviceName = "foreman"

// Attribute names. Domain attributes live in the factory.* namespace; where a GenAI semantic
// convention exists, use it from the pinned semconv package instead of inventing a name.
const (
	// AttrVerb is the foreman verb the process ran.
	AttrVerb = "factory.verb"
	// AttrExitCode is the process's exit code.
	AttrExitCode = "factory.exit_code"
	// AttrAgentRole is the agent role a span, event or measurement belongs to.
	AttrAgentRole = "factory.agent_role"
	// AttrRepo is the repository a span, event or measurement belongs to.
	AttrRepo = "factory.repo"
	// AttrIssue is the issue number a span or event belongs to. It is never a metric label.
	AttrIssue = "factory.issue"
	// AttrState is the factory handoff state an issue is in.
	AttrState = "factory.state"
	// AttrSkipReason is why a poll left a queue alone.
	AttrSkipReason = "factory.skip_reason"
)

// Poll span names. Enqueue is separate from its repository span so a run can continue its trace.
const (
	// PollRepoSpanName is the span for one assigned repository's poll.
	PollRepoSpanName = "foreman.poll.repo"
	// PollEnqueueSpanName is the span for queueing one claimed issue's run.
	PollEnqueueSpanName = "foreman.poll.enqueue"
)

// Poll event names and skip reasons.
const (
	// EventQueueSkipped is recorded when a poll leaves a queue alone.
	EventQueueSkipped = "factory.poll.queue_skipped"
	// EventClaim is recorded on a repository span when the poll claims one of its issues.
	EventClaim = "factory.poll.claim"
	// SkipDAGBusy means the agent role's DAG already has a run waiting for a worker.
	SkipDAGBusy = "dag_busy"
	// SkipCapacityFull means the agent role's runs fill its capacity.
	SkipCapacityFull = "capacity_full"
	// SkipUnhosted means no worker host takes the agent role.
	SkipUnhosted = "unhosted"
)

// Metric names.
const (
	// MetricPollIssues counts open issues in each factory state, per repository.
	MetricPollIssues = "factory.poll.issues"
	// MetricPollClaims counts the issues a poll claimed, per agent role and repository.
	MetricPollClaims = "factory.poll.claims"
	// MetricPollCapacity is the run capacity left for an agent role after the poll's claims.
	MetricPollCapacity = "factory.poll.capacity_left"
)

// RootSpanName is the process root span's name for a foreman verb.
func RootSpanName(verb string) string {
	return "foreman." + verb
}

// Telemetry is the tracer and meter for one process, and the way to flush them.
type Telemetry struct {
	// Tracer opens spans; a no-op when no endpoint is configured.
	Tracer trace.Tracer
	// Meter records instruments; a no-op when no endpoint is configured.
	Meter metric.Meter

	shutdowns []func(context.Context) error
	log       *slog.Logger
}

// Noop is telemetry that records and exports nothing.
func Noop() *Telemetry {
	return &Telemetry{
		Tracer: tracenoop.NewTracerProvider().Tracer(scope),
		Meter:  metricnoop.NewMeterProvider().Meter(scope),
		log:    slog.New(slog.DiscardHandler),
	}
}

// enabled reports whether the environment asks for an export: some OTLP endpoint is set and the SDK is
// not disabled. Without one, foreman exports nothing rather than guessing localhost:4318.
func enabled(lookup func(string) string) bool {
	if lookup("OTEL_SDK_DISABLED") == "true" {
		return false
	}

	for _, name := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	} {
		if lookup(name) != "" {
			return true
		}
	}

	return false
}

// Start builds the process's telemetry from the OTEL_* environment. It never fails: an exporter that
// cannot be built leaves a no-op and a debug record. Export errors afterwards are debug records too, so
// an unreachable collector changes neither the exit code nor what a run prints. A nil log logs nowhere.
func Start(ctx context.Context, log *slog.Logger) *Telemetry {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	t := Noop()
	t.log = log

	if !enabled(os.Getenv) {
		return t
	}

	// The SDK reports export failures through a process-wide handler; route them to a debug record.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.DebugContext(ctx, "telemetry export failed", "error", err)
	}))

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
		resource.WithFromEnv(),
	)
	if err != nil {
		log.DebugContext(ctx, "telemetry resource incomplete", "error", err)
	}

	t.startTraces(ctx, res)
	t.startMetrics(ctx, res)

	return t
}

// startTraces installs a batching OTLP/HTTP trace exporter, if one can be built.
func (t *Telemetry) startTraces(ctx context.Context, res *resource.Resource) {
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		t.log.DebugContext(ctx, "telemetry trace exporter unavailable", "error", err)
		return
	}

	provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	t.Tracer = provider.Tracer(scope)
	t.shutdowns = append(t.shutdowns, provider.Shutdown)
}

// startMetrics installs a periodic OTLP/HTTP metric exporter, if one can be built.
func (t *Telemetry) startMetrics(ctx context.Context, res *resource.Resource) {
	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		t.log.DebugContext(ctx, "telemetry metric exporter unavailable", "error", err)
		return
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)), sdkmetric.WithResource(res),
	)
	t.Meter = provider.Meter(scope)
	t.shutdowns = append(t.shutdowns, provider.Shutdown)
}

// Shutdown flushes and stops the exporters, together waiting no longer than FlushBound. It ignores ctx's
// cancellation (a SIGTERM must still flush) but not its values. A failed flush is a debug record.
func (t *Telemetry) Shutdown(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), FlushBound)
	defer cancel()

	var wg sync.WaitGroup

	for _, shutdown := range t.shutdowns {
		wg.Go(func() {
			if err := shutdown(ctx); err != nil {
				t.log.DebugContext(ctx, "telemetry flush failed", "error", err)
			}
		})
	}

	wg.Wait()
}

// LogHandler wraps inner so a record logged with a context inside a span carries the span's trace_id
// and span_id.
func LogHandler(inner slog.Handler) slog.Handler {
	return spanHandler{inner}
}

// spanHandler adds the active span's ids to each record.
type spanHandler struct{ slog.Handler }

// Handle adds trace_id and span_id when ctx holds a valid span, then passes the record on.
func (h spanHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}

	return h.Handler.Handle(ctx, r) //nolint:wrapcheck // a handler's error is its own
}

// WithAttrs keeps the wrapper around the derived handler.
func (h spanHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return spanHandler{h.Handler.WithAttrs(attrs)}
}

// WithGroup keeps the wrapper around the derived handler.
func (h spanHandler) WithGroup(name string) slog.Handler {
	return spanHandler{h.Handler.WithGroup(name)}
}
