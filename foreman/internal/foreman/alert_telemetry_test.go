package foreman

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"

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

		r := run{
			d: deps, role: workflow.Implementer,
			spec: runSpec{ref: workflow.IssueRef{Repo: repo, Number: 5}}, alerts: newAlertInstruments(deps),
		}
		r.emitStepCap(context.Background(), &opencode.ModelError{Kind: opencode.FailStepCap, Attempts: 1})

		points := metricPoints(collect(), telemetry.MetricSecurityEvents)
		Expect(points).To(HaveLen(1))

		point, ok := pointWith(points,
			attribute.String(telemetry.AttrAgentRole, string(workflow.Implementer)),
			attribute.String(telemetry.AttrRepo, "o/r"),
			attribute.Int(telemetry.AttrIssue, 5),
			attribute.String(telemetry.AttrEvent, string(security.EventRunCapped)),
			attribute.String(telemetry.AttrOutcome, string(security.RunStepCap)),
		)
		Expect(ok).To(BeTrue(), "the point carries the run's role, repository, issue, event and outcome")
		Expect(point.Value).To(Equal(int64(1)))
	})

	It("raises a security event as a log record and a measurement in one call", func() {
		var logged bytes.Buffer

		newAlertInstruments(deps).securityEvent(
			context.Background(), slog.New(slog.NewJSONHandler(&logged, nil)),
			security.Run{Role: string(workflow.Implementer), Repository: "o/r", Issue: 5},
			security.EventOutputRejected, security.Outcome(workflow.OutputFactoryMarker), "blocked.example",
		)

		var record map[string]any
		Expect(json.Unmarshal(logged.Bytes(), &record)).To(Succeed(), "the call wrote no single log record")
		Expect(record).To(HaveKeyWithValue("msg", security.Message))
		Expect(record).To(HaveKeyWithValue("event", string(security.EventOutputRejected)))
		Expect(record).To(HaveKeyWithValue("outcome", string(workflow.OutputFactoryMarker)))
		Expect(record).To(HaveKeyWithValue("host", "blocked.example"))
		Expect(record).To(HaveKeyWithValue("repository", "o/r"))

		points := metricPoints(collect(), telemetry.MetricSecurityEvents)
		Expect(points).To(HaveLen(1), "the call recorded no measurement")

		point, ok := pointWith(points,
			attribute.String(telemetry.AttrAgentRole, string(workflow.Implementer)),
			attribute.String(telemetry.AttrRepo, "o/r"),
			attribute.Int(telemetry.AttrIssue, 5),
			attribute.String(telemetry.AttrEvent, string(security.EventOutputRejected)),
			attribute.String(telemetry.AttrOutcome, string(workflow.OutputFactoryMarker)),
		)
		Expect(ok).To(BeTrue(), "the point carries the run's role, repository, issue, event and outcome")
		Expect(point.Value).To(Equal(int64(1)))
	})

	It("never carries a trace id as a metric label, even inside a span", func() {
		repo, err := workflow.ParseRepo("o/r")
		Expect(err).NotTo(HaveOccurred())

		tracer := sdktrace.NewTracerProvider().Tracer("test")
		ctx, span := tracer.Start(context.Background(), telemetry.RootSpanName("run"))
		DeferCleanup(span.End)

		newAlertInstruments(deps).securityEvent(
			ctx, slog.New(slog.DiscardHandler),
			security.Run{Role: string(workflow.Implementer), Repository: repo.String(), Issue: 5},
			security.EventOutputRejected, security.Outcome(workflow.OutputFactoryMarker), "",
		)

		points := metricPoints(collect(), telemetry.MetricSecurityEvents)
		Expect(points).To(HaveLen(1))

		point, ok := pointWith(points,
			attribute.Int(telemetry.AttrIssue, 5),
			attribute.String(telemetry.AttrOutcome, string(workflow.OutputFactoryMarker)),
		)
		Expect(ok).To(BeTrue(), "the point does not carry the issue it was raised on")
		Expect(point.Value).To(Equal(int64(1)))
		Expect(attributeValue(point.Attributes.ToSlice(), traceIDAttr)).To(BeEmpty(),
			"the point carries the trace id")
	})

	It("records a run's outcome and cost by agent role and repository", func() {
		repo, err := workflow.ParseRepo("o/r")
		Expect(err).NotTo(HaveOccurred())

		alerts := newAlertInstruments(deps)

		run := security.Run{Role: string(workflow.Implementer), Repository: repo.String(), Issue: 5}

		alerts.runOutcome(context.Background(), run, opencode.OutcomeFailed)

		cost := 0.25
		alerts.runCost(context.Background(), run, "m", &cost)

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

		run := security.Run{Role: string(workflow.Implementer), Repository: repo.String()}

		newAlertInstruments(deps).runCost(context.Background(), run, "m", nil)

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

		proxy := &fakeProxy{url: "http://127.0.0.1:9/api/v1", usage: openrouter.Usage{Cost: &cost}}
		// The key proxy raises its refusals through the run's tally, the way the real one does.
		h.deps.Proxy = func(_, _, _ string, refusals *security.Tally) (Forwarder, error) {
			for range 2 {
				refusals.Refuse(context.Background(), h.deps.Log, security.EventKeyProxyRefused,
					security.RefusedBadToken, "openrouter.ai")
			}

			return proxy, nil
		}

		egressProxy := &fakeEgress{url: proxy.url, address: "127.0.0.1:9"}
		h.deps.Egress = func(
			_ context.Context, _ sandbox.Network, _, _ string, refusals *security.Tally,
		) (EgressProxy, error) {
			refusals.Refuse(context.Background(), h.deps.Log, security.EventEgressRefused,
				security.RefusedNotRoutable, "10.1.2.3")

			return egressProxy, nil
		}

		Expect(h.runImpl(testAgent())).Error().NotTo(HaveOccurred())

		var rm metricdata.ResourceMetrics
		Expect(reader.Collect(context.Background(), &rm)).To(Succeed())

		points := metricPoints(rm, telemetry.MetricSecurityEvents)
		Expect(points).To(HaveLen(2))

		notRoutable, ok := pointWith(points,
			attribute.String(telemetry.AttrEvent, string(security.EventEgressRefused)),
			attribute.String(telemetry.AttrOutcome, string(security.RefusedNotRoutable)),
			attribute.Int(telemetry.AttrIssue, 5),
		)
		Expect(ok).To(BeTrue())
		Expect(notRoutable.Value).To(Equal(int64(1)))

		badToken, ok := pointWith(points,
			attribute.String(telemetry.AttrEvent, string(security.EventKeyProxyRefused)),
			attribute.String(telemetry.AttrOutcome, string(security.RefusedBadToken)),
			attribute.Int(telemetry.AttrIssue, 5),
		)
		Expect(ok).To(BeTrue())
		Expect(badToken.Value).To(Equal(int64(2)))

		// The one call that counted each refusal also logged it, exactly once.
		Expect(h.securityEvents()).To(HaveLen(3), "a refusal was logged twice or not at all")

		outcomes := metricPoints(rm, telemetry.MetricRunOutcomes)
		Expect(outcomes).To(HaveLen(1))

		costs := floatPoints(rm, telemetry.MetricRunCost)
		Expect(costs).To(HaveLen(1))
		Expect(costs[0].Value).To(Equal(0.5))

		for _, point := range outcomes {
			attrs := point.Attributes.ToSlice()
			Expect(attributeValue(attrs, telemetry.AttrIssue)).To(BeEmpty(), "a run outcome carries the issue")
			Expect(attributeValue(attrs, traceIDAttr)).To(BeEmpty(), "a run outcome carries the trace id")
		}

		for _, point := range costs {
			attrs := point.Attributes.ToSlice()
			Expect(attributeValue(attrs, telemetry.AttrIssue)).To(BeEmpty(), "a run cost carries the issue")
			Expect(attributeValue(attrs, traceIDAttr)).To(BeEmpty(), "a run cost carries the trace id")
		}
	})
})
