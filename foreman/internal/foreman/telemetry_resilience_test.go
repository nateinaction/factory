package foreman

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/telemetry"
)

var _ = Describe("a run against an unresponsive collector", func() {
	BeforeEach(func() {
		for _, name := range []string{
			"OTEL_SDK_DISABLED", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
			"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL",
		} {
			GinkgoT().Setenv(name, "")
		}
	})

	It("finishes within its bound and flushes afterwards", func() {
		h := newHarness()

		// The collector accepts a connection and never answers, so the exporter's request hangs.
		release := make(chan struct{})
		reached := make(chan struct{}, 1)
		collector := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			select {
			case reached <- struct{}{}:
			default:
			}

			<-release
		}))
		DeferCleanup(collector.Close)
		DeferCleanup(func() { close(release) })
		GinkgoT().Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)

		tel := telemetry.Start(context.Background(), slog.New(slog.DiscardHandler))
		h.deps.Tracer = tel.Tracer
		h.deps.Meter = tel.Meter

		// A full batch starts an export that never completes. It is in flight while the run goes.
		for range 512 {
			_, span := tel.Tracer.Start(context.Background(), "fill")
			span.End()
		}

		Eventually(reached, 5*time.Second).Should(Receive(), "the collector never received an export")

		start := time.Now()

		code, err := h.runImpl(testAgent())

		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(BeZero())
		Expect(time.Since(start)).To(BeNumerically("<", telemetry.FlushBound),
			"telemetry delayed a run while its collector never answered")

		start = time.Now()

		tel.Shutdown(context.Background())
		Expect(time.Since(start)).To(BeNumerically("<", telemetry.FlushBound+time.Second))
	})
})
