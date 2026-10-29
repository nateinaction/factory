package foreman

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/proc"
	"github.com/nateinaction/factory/foreman/internal/telemetry"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// metricPoints is every data point of the named metric, keyed by its attributes.
func metricPoints(rm metricdata.ResourceMetrics, name string) []metricdata.DataPoint[int64] {
	var out []metricdata.DataPoint[int64]

	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}

			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				out = append(out, data.DataPoints...)
			case metricdata.Gauge[int64]:
				out = append(out, data.DataPoints...)
			}
		}
	}

	return out
}

// attributeValue is the string value of key in attrs, or empty when absent.
func attributeValue(attrs []attribute.KeyValue, key string) string {
	for _, a := range attrs {
		if string(a.Key) == key {
			return a.Value.AsString()
		}
	}

	return ""
}

// pointWith finds the data point whose attributes include every wanted pair.
func pointWith(points []metricdata.DataPoint[int64], want ...attribute.KeyValue) (metricdata.DataPoint[int64], bool) {
	for _, p := range points {
		has := map[attribute.Key]attribute.Value{}
		for _, a := range p.Attributes.ToSlice() {
			has[a.Key] = a.Value
		}

		ok := true

		for _, w := range want {
			ok = ok && has[w.Key] == w.Value
		}

		if ok {
			return p, true
		}
	}

	return metricdata.DataPoint[int64]{}, false
}

var _ = Describe("poll telemetry", func() {
	var h *harness

	BeforeEach(func() {
		h = newHarness()
	})

	// traced polls under a parent span, with every span and measurement recorded.
	traced := func(cfgPrefix ...workflow.Role) (*tracetest.SpanRecorder, *sdkmetric.ManualReader, int, error) {
		GinkgoHelper()

		spans := tracetest.NewSpanRecorder()
		tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("test")
		reader := sdkmetric.NewManualReader()
		meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")

		h.deps.Tracer = tracer
		h.deps.Meter = meter

		ctx, parent := tracer.Start(context.Background(), telemetry.RootSpanName("poll"))

		code, err := Poll(ctx, h.deps, pollConfig(cfgPrefix...))

		parent.End()

		return spans, reader, code, err
	}

	It("exports a span per assigned repository with its claim and enqueue", func() {
		h.queue(map[string][]map[string]any{
			workflow.LabelImplement: {{"number": 5}},
		})

		spans, reader, _, err := traced(workflow.Implementer)
		Expect(err).NotTo(HaveOccurred())

		var repo, enqueue sdktrace.ReadOnlySpan

		for _, s := range spans.Ended() {
			switch s.Name() {
			case telemetry.PollRepoSpanName:
				repo = s
			case telemetry.PollEnqueueSpanName:
				enqueue = s
			}
		}

		Expect(repo).NotTo(BeNil(), "no span per assigned repository")
		Expect(repo.Attributes()).To(ContainElement(attribute.String(telemetry.AttrRepo, "o/r")))
		Expect(repo.Status().Code).To(Equal(codes.Unset))

		Expect(enqueue).NotTo(BeNil(), "no span for the enqueue")
		Expect(enqueue.Parent().SpanID()).To(Equal(repo.SpanContext().SpanID()), "the enqueue is not under its repo span")
		Expect(enqueue.Attributes()).To(ContainElement(attribute.String(telemetry.AttrAgentRole, "implementer")))
		Expect(enqueue.Attributes()).To(ContainElement(attribute.String(telemetry.AttrRepo, "o/r")))
		Expect(enqueue.Attributes()).To(ContainElement(attribute.Int(telemetry.AttrIssue, 5)))

		var rm metricdata.ResourceMetrics
		Expect(reader.Collect(context.Background(), &rm)).To(Succeed())

		issues, ok := pointWith(metricPoints(rm, telemetry.MetricPollIssues),
			attribute.String(telemetry.AttrRepo, "o/r"),
			attribute.String(telemetry.AttrState, string(workflow.Implement)),
			attribute.String(telemetry.AttrAgentRole, "implementer"))
		Expect(ok).To(BeTrue(), "no issues-in-state measurement")
		Expect(issues.Value).To(Equal(int64(1)))

		claims, ok := pointWith(metricPoints(rm, telemetry.MetricPollClaims),
			attribute.String(telemetry.AttrRepo, "o/r"),
			attribute.String(telemetry.AttrAgentRole, "implementer"))
		Expect(ok).To(BeTrue(), "no claims measurement")
		Expect(claims.Value).To(Equal(int64(1)))

		capacity, ok := pointWith(metricPoints(rm, telemetry.MetricPollCapacity),
			attribute.String(telemetry.AttrAgentRole, "implementer"))
		Expect(ok).To(BeTrue(), "no capacity measurement")
		Expect(capacity.Value).To(Equal(int64(1)))
	})

	It("marks a repository it cannot read as an error and still works the others", func() {
		h.installed = map[workflow.Role][]string{workflow.Implementer: {"o/good", "o/broken"}}
		h.queueRepo("o/good", map[string][]map[string]any{
			workflow.LabelImplement: {{"number": 5}},
		})
		h.queueRepo("o/broken", nil)
		h.gh.Status("GET /repos/o/broken/issues", http.StatusInternalServerError)

		spans, _, code, err := traced(workflow.Implementer)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(BeZero())

		status := map[string]codes.Code{}

		for _, s := range spans.Ended() {
			if s.Name() == telemetry.PollRepoSpanName {
				status[attributeValue(s.Attributes(), telemetry.AttrRepo)] = s.Status().Code
			}
		}

		Expect(status).To(HaveKeyWithValue("o/broken", codes.Error))
		Expect(status).To(HaveKeyWithValue("o/good", codes.Unset))

		enqueued := h.runner.find("enqueue")
		Expect(enqueued).To(HaveLen(1), "the readable repository was not worked")
		Expect(joined(enqueued[0])).To(ContainSubstring("REPO=o/good ISSUE=5"))
	})

	It("records a queue left alone because its DAG is busy as an event", func() {
		h.queue(map[string][]map[string]any{
			workflow.LabelImplement: {{"number": 5}},
		})
		h.runner.wrap(func(c proc.Cmd, base func(proc.Cmd) (string, error)) (string, error) {
			if len(c.Argv) > 1 && c.Argv[1] == "history" {
				return `[{"name":"factory-implementer","dagRunId":"r1","status":"queued"}]`, nil
			}

			return base(c)
		})

		spans, _, _, err := traced(workflow.Implementer)
		Expect(err).NotTo(HaveOccurred())

		var events []sdktrace.Event

		for _, s := range spans.Ended() {
			if s.Name() == telemetry.RootSpanName("poll") {
				events = s.Events()
			}
		}

		Expect(events).To(ContainElement(HaveField("Name", telemetry.EventQueueSkipped)))
		Expect(events[0].Attributes).To(ContainElement(attribute.String(telemetry.AttrAgentRole, "implementer")))
		Expect(events[0].Attributes).To(ContainElement(
			attribute.String(telemetry.AttrSkipReason, telemetry.SkipDAGBusy)))
	})
})
