package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/telemetry"
)

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
		return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
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

var _ = Describe("LogHandler", func() {
	record := func(ctx context.Context) map[string]any {
		var out bytes.Buffer

		slog.New(telemetry.LogHandler(slog.NewJSONHandler(&out, nil))).ErrorContext(ctx, "boom")

		var m map[string]any
		Expect(json.Unmarshal(out.Bytes(), &m)).To(Succeed())

		return m
	}

	It("adds the active span's trace and span ids", func() {
		tp := sdktrace.NewTracerProvider()
		ctx, span := tp.Tracer("t").Start(context.Background(), "s")

		m := record(ctx)
		Expect(m).To(HaveKeyWithValue("trace_id", span.SpanContext().TraceID().String()))
		Expect(m).To(HaveKeyWithValue("span_id", span.SpanContext().SpanID().String()))
	})

	It("adds nothing outside a span", func() {
		m := record(context.Background())
		Expect(m).NotTo(HaveKey("trace_id"))
		Expect(m).NotTo(HaveKey("span_id"))
	})
})
