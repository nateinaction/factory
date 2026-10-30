package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/security"
	"github.com/nateinaction/factory/foreman/internal/telemetry"
)

// decodeRecords decodes every JSON line a logger wrote.
func decodeRecords(raw []byte) []map[string]any {
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	out := make([]map[string]any, 0, len(lines))

	for _, line := range lines {
		var m map[string]any
		Expect(json.Unmarshal(line, &m)).To(Succeed())
		out = append(out, m)
	}

	return out
}

// clearEnv blanks every OTEL_* variable the specs depend on, so the machine's own do not leak in.
func clearEnv() {
	for _, name := range []string{
		"OTEL_SDK_DISABLED", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_SERVICE_NAME",
	} {
		GinkgoT().Setenv(name, "")
	}
}

var _ = Describe("Start", func() {
	BeforeEach(clearEnv)

	// The SDK's error handler is process-wide and a late export can log after its spec ended, so the
	// sink must not be shared (or read) across specs.
	logger := func() *slog.Logger {
		return slog.New(slog.DiscardHandler)
	}

	It("exports one root span to the OTLP endpoint, then flushes", func() {
		var traces atomic.Int32

		collector := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/traces" {
				traces.Add(1)
			}
		}))
		DeferCleanup(collector.Close)
		GinkgoT().Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)

		tel := telemetry.Start(context.Background(), logger())

		_, span := tel.Tracer.Start(context.Background(), telemetry.RootSpanName("poll"))
		Expect(span.SpanContext().IsValid()).To(BeTrue())
		span.End()

		tel.Shutdown(context.Background())
		Expect(traces.Load()).To(Equal(int32(1)))
	})

	It("is a no-op without an endpoint", func() {
		tel := telemetry.Start(context.Background(), logger())

		_, span := tel.Tracer.Start(context.Background(), "x")
		Expect(span.SpanContext().IsValid()).To(BeFalse())
		span.End()

		start := time.Now()

		tel.Shutdown(context.Background())
		Expect(time.Since(start)).To(BeNumerically("<", time.Second))
	})

	It("stays disabled when OTEL_SDK_DISABLED is true", func() {
		GinkgoT().Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
		GinkgoT().Setenv("OTEL_SDK_DISABLED", "true")

		tel := telemetry.Start(context.Background(), logger())

		_, span := tel.Tracer.Start(context.Background(), "x")
		defer span.End()

		Expect(span.SpanContext().IsValid()).To(BeFalse())
	})

	It("flushes within its bound against a refused endpoint", func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		addr := ln.Addr().String()
		Expect(ln.Close()).To(Succeed())
		GinkgoT().Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+addr)

		tel := telemetry.Start(context.Background(), logger())

		_, span := tel.Tracer.Start(context.Background(), "x")
		span.End()

		start := time.Now()

		tel.Shutdown(context.Background())
		Expect(time.Since(start)).To(BeNumerically("<", telemetry.FlushBound+time.Second))
	})

	It("flushes within its bound against a collector that never answers", func() {
		release := make(chan struct{})
		slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
		DeferCleanup(slow.Close)
		DeferCleanup(func() { close(release) })
		GinkgoT().Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", slow.URL)

		tel := telemetry.Start(context.Background(), logger())

		_, span := tel.Tracer.Start(context.Background(), "x")
		span.End()

		start := time.Now()

		tel.Shutdown(context.Background())
		Expect(time.Since(start)).To(BeNumerically("<", telemetry.FlushBound+time.Second))
	})
})

var _ = Describe("trace context propagation", func() {
	// testTraceParent is a well-formed W3C trace context: version, trace id, parent id, sampled flag.
	const testTraceParent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	It("injects the active span as a W3C traceparent", func() {
		tp := sdktrace.NewTracerProvider()

		ctx, span := tp.Tracer("t").Start(context.Background(), "s")
		defer span.End()

		Expect(telemetry.TraceParent(ctx)).To(Equal(
			"00-" + span.SpanContext().TraceID().String() + "-" + span.SpanContext().SpanID().String() + "-01"))
	})

	It("injects nothing outside a real span", func() {
		Expect(telemetry.TraceParent(context.Background())).To(BeEmpty())
	})

	It("continues a valid trace context as its remote parent", func() {
		ctx, ok := telemetry.ContinueTrace(context.Background(), testTraceParent)
		Expect(ok).To(BeTrue())

		sc := trace.SpanContextFromContext(ctx)
		Expect(sc.TraceID().String()).To(Equal("4bf92f3577b34da6a3ce929d0e0e4736"))
		Expect(sc.SpanID().String()).To(Equal("00f067aa0ba902b7"))
		Expect(sc.IsRemote()).To(BeTrue())
	})

	It("starts a new trace from a missing context", func() {
		ctx, ok := telemetry.ContinueTrace(context.Background(), "")
		Expect(ok).To(BeTrue())
		Expect(trace.SpanContextFromContext(ctx).IsValid()).To(BeFalse())
	})

	DescribeTable("ignores a malformed context",
		func(value string) {
			ctx, ok := telemetry.ContinueTrace(context.Background(), value)
			Expect(ok).To(BeFalse())
			Expect(trace.SpanContextFromContext(ctx).IsValid()).To(BeFalse())
		},
		Entry("not a traceparent", "not-a-traceparent"),
		Entry("a non-hex version", "gg-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"),
		Entry("an all-zero trace id", "00-00000000000000000000000000000000-00f067aa0ba902b7-01"),
		Entry("a truncated span id", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa-01"),
		Entry("a bad flag", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-zz"),
	)
})

var _ = Describe("LogHandler", func() {
	logger := func(out *bytes.Buffer) *slog.Logger {
		return slog.New(telemetry.LogHandler(slog.NewJSONHandler(out, nil)))
	}

	record := func(ctx context.Context) map[string]any {
		var out bytes.Buffer

		logger(&out).ErrorContext(ctx, "boom")

		var m map[string]any
		Expect(json.Unmarshal(out.Bytes(), &m)).To(Succeed())

		return m
	}

	It("adds the active span's trace and span ids", func() {
		tp := sdktrace.NewTracerProvider()

		ctx, span := tp.Tracer("t").Start(context.Background(), "s")
		defer span.End()

		m := record(ctx)
		Expect(m).To(HaveKeyWithValue("trace_id", span.SpanContext().TraceID().String()))
		Expect(m).To(HaveKeyWithValue("span_id", span.SpanContext().SpanID().String()))
	})

	It("adds nothing outside a span", func() {
		m := record(context.Background())
		Expect(m).NotTo(HaveKey("trace_id"))
		Expect(m).NotTo(HaveKey("span_id"))
	})

	It("keeps the ids on a logger derived with attributes", func() {
		tp := sdktrace.NewTracerProvider()

		ctx, span := tp.Tracer("t").Start(context.Background(), "s")
		defer span.End()

		var out bytes.Buffer

		logger(&out).With("issue", 5).WarnContext(ctx, "boom")

		m := decodeRecords(out.Bytes())[0]
		Expect(m).To(HaveKeyWithValue("issue", BeNumerically("==", 5)))
		Expect(m).To(HaveKeyWithValue("trace_id", span.SpanContext().TraceID().String()))
		Expect(m).To(HaveKeyWithValue("span_id", span.SpanContext().SpanID().String()))
	})

	It("keeps the ids and the group on a grouped logger", func() {
		tp := sdktrace.NewTracerProvider()

		ctx, span := tp.Tracer("t").Start(context.Background(), "s")
		defer span.End()

		var out bytes.Buffer

		logger(&out).WithGroup("run").WarnContext(ctx, "boom")

		m := decodeRecords(out.Bytes())[0]
		Expect(m).To(HaveKey("run"))

		group, ok := m["run"].(map[string]any)
		Expect(ok).To(BeTrue(), "the group holds an object")
		Expect(group).To(HaveKeyWithValue("trace_id", span.SpanContext().TraceID().String()))
		Expect(group).To(HaveKeyWithValue("span_id", span.SpanContext().SpanID().String()))
	})
})

var _ = Describe("SpanLogger", func() {
	It("adds the active span's ids to an ordinary diagnostic and a security event", func() {
		tp := sdktrace.NewTracerProvider()

		ctx, span := tp.Tracer("t").Start(context.Background(), "s")
		defer span.End()

		var out bytes.Buffer

		log := telemetry.SpanLogger(slog.New(slog.NewJSONHandler(&out, nil)), ctx)
		log.Warn("ordinary diagnostic", "issue", 5)
		security.Emit(
			security.Scope(log, "implementer", "o/r", 5),
			security.EventOutputRejected, security.RefusedByPolicy, "example.test",
		)

		records := decodeRecords(out.Bytes())
		Expect(records).To(HaveLen(2))

		for _, m := range records {
			Expect(m).To(HaveKeyWithValue("trace_id", span.SpanContext().TraceID().String()))
			Expect(m).To(HaveKeyWithValue("span_id", span.SpanContext().SpanID().String()))
		}
	})

	It("leaves the logger alone outside a span", func() {
		var out bytes.Buffer

		log := telemetry.SpanLogger(slog.New(slog.NewJSONHandler(&out, nil)), context.Background())
		log.Warn("boom")

		m := decodeRecords(out.Bytes())[0]
		Expect(m).NotTo(HaveKey("trace_id"))
		Expect(m).NotTo(HaveKey("span_id"))
	})
})

var _ = Describe("ExportEnv", func() {
	It("keeps only the export variables that are set", func() {
		env := telemetry.ExportEnv(func(name string) string {
			return map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318",
				"OTEL_SERVICE_NAME":           "foreman",
				"FACTORY_WORK_ROOT":           "/somewhere",
			}[name]
		})

		Expect(env).To(Equal(map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318",
			"OTEL_SERVICE_NAME":           "foreman",
		}), "a setting other than telemetry's own must not cross sudo")
	})

	It("names every variable the sudoers rule keeps", func() {
		Expect(telemetry.EnvNames).To(ConsistOf("OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_SERVICE_NAME"))
	})
})
