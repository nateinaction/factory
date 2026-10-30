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
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0" // pinned: the GenAI conventions are still in development
	"go.opentelemetry.io/otel/trace"
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
	// AttrIssue is the issue number a span or event belongs to. It is not a metric label, with two
	// deliberate exceptions: a security event's metric and a reaped run's metric carry it, so their
	// alerts can name the issue a human needs to look at (see instrument.go's securityEvents and
	// runReaped, and #132). No other metric carries it.
	AttrIssue = "factory.issue"
	// AttrState is the factory handoff state an issue is in.
	AttrState = "factory.state"
	// AttrSkipReason is why a poll left a queue alone.
	AttrSkipReason = "factory.skip_reason"
	// AttrModel is the model an agent role's run calls. The GenAI conventions name it, so foreman
	// uses their key against the pinned semconv version instead of inventing its own.
	AttrModel = string(semconv.GenAIRequestModelKey)
	// AttrWorkerHost is the host the run ran on.
	AttrWorkerHost = "factory.worker_host"
	// AttrOutcome is how a run ended.
	AttrOutcome = "factory.outcome"
	// AttrEvent is the security event a measurement counts.
	AttrEvent = "factory.event"
	// AttrFailureKind is why a failed run, or a failed stage of one, failed.
	AttrFailureKind = "factory.failure_kind"
	// AttrAttempt is the 1-based number of one model attempt.
	AttrAttempt = "factory.attempt"
	// AttrNextAttempt is the attempt a backoff wait precedes.
	AttrNextAttempt = "factory.next_attempt"
	// AttrBackoffSeconds is the length of a backoff wait.
	AttrBackoffSeconds = "factory.backoff_seconds"
	// AttrAttempts is how many times a run called the model.
	AttrAttempts = "factory.attempts"
	// AttrInputTokens, AttrOutputTokens and AttrCachedTokens are the run's token totals. The first two
	// are the GenAI conventions' own keys; the conventions name a cache read's input tokens too.
	AttrInputTokens  = string(semconv.GenAIUsageInputTokensKey)
	AttrOutputTokens = string(semconv.GenAIUsageOutputTokensKey)
	AttrCachedTokens = string(semconv.GenAIUsageCacheReadInputTokensKey)
	// AttrCost is what the run's responses cost, in US dollars.
	AttrCost = "factory.cost"
	// AttrPeakContext is the largest context any one model step used.
	AttrPeakContext = "factory.peak_context"
	// AttrRunID is the dagu run id one run's spans belong to. It is how `status` finds a run's trace.
	AttrRunID = "factory.run_id"
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
	// MetricPollIssues is the number of open issues in each factory state, per repository. It is a
	// gauge, so a state that has emptied reports zero.
	MetricPollIssues = "factory.poll.issues"
	// MetricPollClaims counts the issues a poll claimed, per agent role and repository.
	MetricPollClaims = "factory.poll.claims"
	// MetricPollCapacity is the run capacity left for an agent role after the poll's claims.
	MetricPollCapacity = "factory.poll.capacity_left"
	// MetricSecurityEvents counts the security events of each agent role and repository, by issue,
	// event and outcome. It is the metric an alert reads, and the issue is a label so the alert can
	// name it; no trace id is.
	MetricSecurityEvents = "factory.security.events"
	// MetricRunOutcomes counts finished runs by agent role, repository and outcome. A failed run's
	// share of the total is the agent role's failure rate.
	MetricRunOutcomes = "factory.run.outcomes"
	// MetricRunCost is what model calls cost, in US dollars, by agent role, repository and model.
	MetricRunCost = "factory.run.cost"
	// MetricRunsReaped counts dead runs a poll called failed, by agent role, repository and issue.
	// Like a security event, it carries the issue so its alert can name it; no trace id is.
	MetricRunsReaped = "factory.runs.reaped"
)

// Run span names. Each stage span is a child of the process root span, which names the run verb.
const (
	// SpanClone clones the repository the run works.
	SpanClone = "foreman.run.clone"
	// SpanToolchain resolves the default branch's dev environment.
	SpanToolchain = "foreman.run.toolchain"
	// SpanToolchainClone is the shallow clone the dev environment is resolved from.
	SpanToolchainClone = "foreman.run.toolchain.clone"
	// SpanToolchainEnv is the dev-environment evaluation itself.
	SpanToolchainEnv = "foreman.run.toolchain.env"
	// SpanProxyBind starts the run's OpenRouter proxy.
	SpanProxyBind = "foreman.run.proxy.bind"
	// SpanModelAttempt is one call to the model.
	SpanModelAttempt = "foreman.run.model.attempt"
	// SpanModelBackoff is the wait between two model attempts.
	SpanModelBackoff = "foreman.run.model.backoff"
	// SpanOutputValidation reads and parses the agent role's output file.
	SpanOutputValidation = "foreman.run.output.validate"
	// SpanPublishCommit commits the agent's working tree.
	SpanPublishCommit = "foreman.run.publish.commit"
	// SpanPublishPush pushes the branch.
	SpanPublishPush = "foreman.run.publish.push"
	// SpanPublishPR opens or updates the pull request.
	SpanPublishPR = "foreman.run.publish.pull_request"
	// SpanPublishReview posts the review's verdict.
	SpanPublishReview = "foreman.run.publish.review"
	// SpanRetro runs the run's retro.
	SpanRetro = "foreman.run.retro"
)

// RootSpanName is the process root span's name for a foreman verb.
func RootSpanName(verb string) string {
	return "foreman." + verb
}

// TraceParentHeader is the W3C trace context header foreman carries between processes.
const TraceParentHeader = "traceparent"

// TraceParentVar is the environment variable and DAG parameter that carries the W3C trace context
// between foreman's processes.
const TraceParentVar = "TRACEPARENT"

// TraceParent is the W3C traceparent value for the span active in ctx, or empty when ctx has no real
// span. It is how one process hands its trace to the next.
func TraceParent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)

	return carrier.Get(TraceParentHeader)
}

// ContinueTrace makes value the remote parent in ctx. It returns ctx unchanged and false for a
// malformed value, so the caller can log a diagnostic and start a new trace. An empty value is not
// malformed: it simply carries no trace context.
func ContinueTrace(ctx context.Context, value string) (context.Context, bool) {
	if value == "" {
		return ctx, true
	}

	carrier := propagation.MapCarrier{TraceParentHeader: value}
	continued := propagation.TraceContext{}.Extract(ctx, carrier)
	sc := trace.SpanContextFromContext(continued)

	if !sc.IsValid() || !sc.IsRemote() {
		return ctx, false
	}

	return continued, true
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

// Noop is telemetry that records and exports nothing. It is the one no-op tracer and meter, the
// same fallbacks a Deps built without telemetry gets.
func Noop() *Telemetry {
	return &Telemetry{
		Tracer: TracerOr(nil),
		Meter:  MeterOr(nil),
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
