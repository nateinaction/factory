package foreman

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/telemetry"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// stageNames are the run stages every agent role's trace names, besides its model attempts.
func stageNames() []string {
	return []string{
		telemetry.SpanProxyBind, telemetry.SpanClone, telemetry.SpanToolchain,
		telemetry.SpanToolchainClone, telemetry.SpanToolchainEnv, telemetry.SpanOutputValidation,
		telemetry.SpanPublishCommit, telemetry.SpanPublishPush, telemetry.SpanPublishPR, telemetry.SpanRetro,
	}
}

// testHost is the worker host every traced run reports.
const testHost = "worker-1"

// traceHarness is a harness whose Deps records the run's spans.
type traceHarness struct {
	*harness
	recorder *tracetest.SpanRecorder
}

// newTraceHarness builds a harness with a tracer that keeps every ended span.
func newTraceHarness() *traceHarness {
	h := newHarness()
	recorder := tracetest.NewSpanRecorder()
	h.deps.Tracer = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)).Tracer("spec")
	h.deps.Host = testHost

	return &traceHarness{harness: h, recorder: recorder}
}

// traced runs the Implementer on o/r#5 under the run's root span, as `foreman run` does, and returns
// the root span's context and the run's result.
func (t *traceHarness) traced(ctx context.Context) (trace.SpanContext, int, error) {
	ctx, root := t.deps.Tracer.Start(ctx, telemetry.RootSpanName("run"))
	code, err := Run(ctx, t.deps, workflow.Implementer, testRef(5), testAgent(), testRetro(), 3)

	root.End()

	return root.SpanContext(), code, err
}

// spansNamed returns every ended span with the name.
func (t *traceHarness) spansNamed(name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan

	for _, span := range t.recorder.Ended() {
		if span.Name() == name {
			out = append(out, span)
		}
	}

	return out
}

// spanNamed returns the one ended span with the name, failing when there is not exactly one.
func (t *traceHarness) spanNamed(name string) sdktrace.ReadOnlySpan {
	GinkgoHelper()

	spans := t.spansNamed(name)
	Expect(spans).To(HaveLen(1), "spans named "+name)

	return spans[0]
}

// names returns every ended span's name, in the order they ended.
func (t *traceHarness) names() []string {
	out := make([]string, 0, len(t.recorder.Ended()))

	for _, span := range t.recorder.Ended() {
		out = append(out, span.Name())
	}

	return out
}

// failFirstRunCall answers the run's first model call with stream and every later call with the
// harness's own answer, so a retried attempt still writes the agent role's output file.
func (t *traceHarness) failFirstRunCall(stream string) {
	failed := false

	t.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
		if !failed && isModel(c) && !strings.Contains(joined(c), "--session") {
			failed = true

			return stream, nil
		}

		return base(c)
	})
}

var _ = Describe("a run's trace", func() {
	var t *traceHarness

	BeforeEach(func() {
		t = newTraceHarness()
	})

	It("has a span for each stage of an Implementer run, all under the run's root span", func() {
		t.implementerGH(nil, nil)
		t.writeToken(workflow.Implementer, "repo-token\n")
		t.roleWrites(workflow.Implementer, 5, firstRunOutput)
		t.roleWritesRetro(workflow.Implementer, validRetro)

		root, code, err := t.traced(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		Expect(t.names()).To(ContainElements(
			telemetry.SpanProxyBind,
			telemetry.SpanClone,
			telemetry.SpanToolchain,
			telemetry.SpanToolchainClone,
			telemetry.SpanToolchainEnv,
			telemetry.SpanModelAttempt,
			telemetry.SpanOutputValidation,
			telemetry.SpanPublishCommit,
			telemetry.SpanPublishPush,
			telemetry.SpanPublishPR,
			telemetry.SpanRetro,
		))

		// Every stage span hangs off the run's root span, except the toolchain's own two steps, which
		// hang off the toolchain span.
		toolchain := t.spanNamed(telemetry.SpanToolchain)

		for _, name := range []string{
			telemetry.SpanProxyBind, telemetry.SpanClone, telemetry.SpanToolchain,
			telemetry.SpanOutputValidation, telemetry.SpanPublishCommit, telemetry.SpanPublishPush,
			telemetry.SpanPublishPR, telemetry.SpanRetro,
		} {
			Expect(t.spanNamed(name).Parent().SpanID()).To(Equal(root.SpanID()), "parent of "+name)
		}

		for _, name := range []string{telemetry.SpanToolchainClone, telemetry.SpanToolchainEnv} {
			Expect(t.spanNamed(name).Parent().SpanID()).To(Equal(toolchain.SpanContext().SpanID()),
				"parent of "+name)
		}

		// Each stage carries the run's own attributes.
		for _, name := range append(stageNames(), telemetry.SpanModelAttempt) {
			for _, span := range t.spansNamed(name) {
				Expect(span.Attributes()).To(ContainElements(
					attribute.String(telemetry.AttrAgentRole, string(workflow.Implementer)),
					attribute.String(telemetry.AttrRepo, "o/r"),
					attribute.Int(telemetry.AttrIssue, 5),
					attribute.String(telemetry.AttrModel, testAgent().Model),
					attribute.String(telemetry.AttrWorkerHost, testHost),
				), name)
			}
		}
	})

	It("puts the run's outcome and usage on its root span", func() {
		t.implementerGH(nil, nil)
		t.writeToken(workflow.Implementer, "repo-token\n")
		t.roleWrites(workflow.Implementer, 5, firstRunOutput)
		t.roleWritesRetro(workflow.Implementer, validRetro)

		root, _, err := t.traced(context.Background())
		Expect(err).NotTo(HaveOccurred())

		span := t.spanNamed(telemetry.RootSpanName("run"))
		Expect(span.SpanContext().SpanID()).To(Equal(root.SpanID()))

		// The model and its retro each answered one okStream: 1000 input tokens, 50 output, 400
		// cached and 0.002 cost apiece. The run's attempts count only its own model calls.
		Expect(span.Attributes()).To(ContainElements(
			attribute.String(telemetry.AttrAgentRole, string(workflow.Implementer)),
			attribute.String(telemetry.AttrRepo, "o/r"),
			attribute.Int(telemetry.AttrIssue, 5),
			attribute.String(telemetry.AttrModel, testAgent().Model),
			attribute.String(telemetry.AttrWorkerHost, testHost),
			attribute.String(telemetry.AttrOutcome, string(opencode.OutcomeHandedOff)),
			attribute.Int(telemetry.AttrAttempts, 1),
			attribute.Int(telemetry.AttrInputTokens, 2000),
			attribute.Int(telemetry.AttrOutputTokens, 100),
			attribute.Int(telemetry.AttrCachedTokens, 800),
			attribute.Float64(telemetry.AttrCost, 0.004),
		))
	})

	It("shows a retried model attempt and its backoff as separate spans", func() {
		t.implementerGH(nil, nil)
		t.writeToken(workflow.Implementer, "repo-token\n")
		t.roleWrites(workflow.Implementer, 5, firstRunOutput)
		t.roleWritesRetro(workflow.Implementer, validRetro)
		t.failFirstRunCall(errStream("APIError", 429))

		root, _, err := t.traced(context.Background())
		Expect(err).NotTo(HaveOccurred())

		var attempts []sdktrace.ReadOnlySpan

		for _, span := range t.spansNamed(telemetry.SpanModelAttempt) {
			if span.Parent().SpanID() == root.SpanID() {
				attempts = append(attempts, span)
			}
		}

		Expect(attempts).To(HaveLen(2), "the failed attempt and the retry")

		failed := attempts[0]
		Expect(failed.Parent().SpanID()).To(Equal(root.SpanID()))
		Expect(failed.Attributes()).To(ContainElement(attribute.Int(telemetry.AttrAttempt, 1)))
		Expect(failed.Status().Code).To(Equal(codes.Error))
		Expect(failed.Attributes()).To(ContainElement(
			attribute.String(telemetry.AttrFailureKind, string(opencode.FailRateLimit))))

		// The retry is a success, so it carries no failure kind.
		retry := attempts[1]
		Expect(retry.Attributes()).To(ContainElement(attribute.Int(telemetry.AttrAttempt, 2)))
		Expect(retry.Status().Code).To(Equal(codes.Unset))

		backoff := t.spanNamed(telemetry.SpanModelBackoff)
		Expect(backoff.Parent().SpanID()).To(Equal(root.SpanID()))
		Expect(backoff.Attributes()).To(ContainElements(
			attribute.Int(telemetry.AttrNextAttempt, 2),
			attribute.Float64(telemetry.AttrBackoffSeconds, opencode.Backoff(1).Seconds()),
		))
	})

	It("marks a failed stage as an error with the failure kind", func() {
		t.implementerGH(nil, nil)
		t.writeToken(workflow.Implementer, "repo-token\n")
		t.roleWrites(workflow.Implementer, 5, firstRunOutput)
		t.answerOpencode(errStream("APIError", 402))

		_, code, err := t.traced(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(code).To(Equal(1))

		attempt := t.spanNamed(telemetry.SpanModelAttempt)
		Expect(attempt.Status().Code).To(Equal(codes.Error))
		Expect(attempt.Attributes()).To(ContainElement(
			attribute.String(telemetry.AttrFailureKind, string(opencode.FailCredits))))

		span := t.spanNamed(telemetry.RootSpanName("run"))
		Expect(span.Status().Code).To(Equal(codes.Unset), "the run's own root span is the process's")
		Expect(span.Attributes()).To(ContainElements(
			attribute.String(telemetry.AttrOutcome, string(opencode.OutcomeFailed)),
			attribute.String(telemetry.AttrFailureKind, string(opencode.FailCredits)),
		))
	})

	It("marks the clone's span when the clone fails", func() {
		t.implementerGH(nil, nil)
		t.writeToken(workflow.Implementer, "repo-token\n")
		t.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
			if len(c.Argv) > 1 && c.Argv[1] == "clone" {
				return "", &proc.ExitError{Program: "git", Code: 128}
			}

			return base(c)
		})

		_, code, err := t.traced(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(code).To(Equal(1))

		clone := t.spanNamed(telemetry.SpanClone)
		Expect(clone.Status().Code).To(Equal(codes.Error))
		Expect(clone.Attributes()).To(ContainElement(attribute.String(telemetry.AttrFailureKind,
			string(opencode.KindOf(&proc.ExitError{Program: "git", Code: 128})))))
	})
})
