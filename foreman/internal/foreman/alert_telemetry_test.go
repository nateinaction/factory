package foreman

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/openrouter"
	"github.com/nateinaction/factory/foreman/internal/sandbox"
	"github.com/nateinaction/factory/foreman/internal/security"
	"github.com/nateinaction/factory/foreman/internal/telemetry"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// floatPoints is every data point of the named float counter.
func floatPoints(rm metricdata.ResourceMetrics, name string) []metricdata.DataPoint[float64] {
	var out []metricdata.DataPoint[float64]

	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}

			if sum, ok := m.Data.(metricdata.Sum[float64]); ok {
				out = append(out, sum.DataPoints...)
			}
		}
	}

	return out
}

var _ = Describe("alert telemetry", func() {
	var (
		deps   Deps
		reader *sdkmetric.ManualReader
	)

	BeforeEach(func() {
		reader = sdkmetric.NewManualReader()
		deps = Deps{Meter: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")}
	})

	collect := func() metricdata.ResourceMetrics {
		GinkgoHelper()

		var rm metricdata.ResourceMetrics
		Expect(reader.Collect(context.Background(), &rm)).To(Succeed())

		return rm
	}

	It("records a security event by agent role, repository, issue, event and outcome", func() {
		repo, err := workflow.ParseRepo("o/r")
		Expect(err).NotTo(HaveOccurred())

		emitStepCap(context.Background(), deps, newAlertInstruments(deps), workflow.Implementer, repo, 5,
			&opencode.ModelError{Kind: opencode.FailStepCap, Attempts: 1})

		points := metricPoints(collect(), telemetry.MetricSecurityEvents)
		Expect(points).To(HaveLen(1))

		point, ok := pointWith(points,
			attribute.String(telemetry.AttrAgentRole, string(workflow.Implementer)),
			attribute.String(telemetry.AttrRepo, "o/r"),
			attribute.Int(telemetry.AttrIssue, 5),
			attribute.String(telemetry.AttrEvent, security.EventRunCapped),
			attribute.String(telemetry.AttrOutcome, string(security.RunStepCap)),
		)
		Expect(ok).To(BeTrue(), "the point carries the run's role, repository, issue, event and outcome")
		Expect(point.Value).To(Equal(int64(1)))
	})

	It("carries the trace that raised a security event, so its alert links straight to it", func() {
		repo, err := workflow.ParseRepo("o/r")
		Expect(err).NotTo(HaveOccurred())

		tracer := sdktrace.NewTracerProvider().Tracer("test")
		ctx, span := tracer.Start(context.Background(), telemetry.RootSpanName("run"))
		DeferCleanup(span.End)

		newAlertInstruments(deps).securityEvent(
			ctx, workflow.Implementer, repo, 5, security.EventOutputRejected, security.Outcome(workflow.OutputFactoryMarker),
		)

		points := metricPoints(collect(), telemetry.MetricSecurityEvents)
		Expect(points).To(HaveLen(1))

		point, ok := pointWith(points,
			attribute.String(telemetry.AttrTraceID, span.SpanContext().TraceID().String()),
			attribute.Int(telemetry.AttrIssue, 5),
			attribute.String(telemetry.AttrOutcome, string(workflow.OutputFactoryMarker)),
		)
		Expect(ok).To(BeTrue(), "the point carries the trace that raised it")
		Expect(point.Value).To(Equal(int64(1)))
	})

	It("records a run's outcome and cost by agent role and repository", func() {
		repo, err := workflow.ParseRepo("o/r")
		Expect(err).NotTo(HaveOccurred())

		alerts := newAlertInstruments(deps)

		alerts.runOutcome(context.Background(), workflow.Implementer, repo, opencode.OutcomeFailed)

		cost := 0.25
		alerts.runCost(context.Background(), workflow.Implementer, repo, "m", &cost)

		rm := collect()

		outcomes := metricPoints(rm, telemetry.MetricRunOutcomes)
		Expect(outcomes).To(HaveLen(1))
		Expect(attributeValue(outcomes[0].Attributes.ToSlice(), telemetry.AttrOutcome)).
			To(Equal(string(opencode.OutcomeFailed)))

		costs := floatPoints(rm, telemetry.MetricRunCost)
		Expect(costs).To(HaveLen(1))
		Expect(costs[0].Value).To(Equal(0.25))
	})

	It("records no cost when no response reported one", func() {
		repo, err := workflow.ParseRepo("o/r")
		Expect(err).NotTo(HaveOccurred())

		newAlertInstruments(deps).runCost(context.Background(), workflow.Implementer, repo, "m", nil)

		Expect(floatPoints(collect(), telemetry.MetricRunCost)).To(BeEmpty())
	})
})

var _ = Describe("run alert telemetry", func() {
	var (
		h      *harness
		reader *sdkmetric.ManualReader
	)

	BeforeEach(func() {
		h = newHarness()

		reader = sdkmetric.NewManualReader()
		h.deps.Meter = sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
	})

	It("records the run's outcome and cost, and every request its proxies refused", func() {
		cost := 0.5

		proxy := &fakeProxy{
			url: "http://127.0.0.1:9/api/v1", usage: openrouter.Usage{Cost: &cost},
			refusals: map[security.Outcome]int{security.RefusedBadToken: 2},
		}
		h.deps.Proxy = func(string, string, string) (Forwarder, error) { return proxy, nil }

		egressProxy := &fakeEgress{
			url: proxy.url, address: "127.0.0.1:9",
			refusals: map[security.Outcome]int{security.RefusedNotRoutable: 1},
		}
		h.deps.Egress = func(context.Context, sandbox.Network, string, string) (EgressProxy, error) {
			return egressProxy, nil
		}

		Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())

		var rm metricdata.ResourceMetrics
		Expect(reader.Collect(context.Background(), &rm)).To(Succeed())

		points := metricPoints(rm, telemetry.MetricSecurityEvents)
		Expect(points).To(HaveLen(2))

		notRoutable, ok := pointWith(points,
			attribute.String(telemetry.AttrEvent, security.EventEgressRefused),
			attribute.String(telemetry.AttrOutcome, string(security.RefusedNotRoutable)),
			attribute.Int(telemetry.AttrIssue, 5),
		)
		Expect(ok).To(BeTrue())
		Expect(notRoutable.Value).To(Equal(int64(1)))

		badToken, ok := pointWith(points,
			attribute.String(telemetry.AttrEvent, security.EventKeyProxyRefused),
			attribute.String(telemetry.AttrOutcome, string(security.RefusedBadToken)),
			attribute.Int(telemetry.AttrIssue, 5),
		)
		Expect(ok).To(BeTrue())
		Expect(badToken.Value).To(Equal(int64(2)))

		outcomes := metricPoints(rm, telemetry.MetricRunOutcomes)
		Expect(outcomes).To(HaveLen(1))

		costs := floatPoints(rm, telemetry.MetricRunCost)
		Expect(costs).To(HaveLen(1))
		Expect(costs[0].Value).To(Equal(0.5))
	})
})
