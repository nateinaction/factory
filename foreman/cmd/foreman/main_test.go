package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/telemetry"
)

var _ = DescribeTable("detecting sudo",
	func(sudoUID string, realUID, effectiveUID int, expected bool) {
		Expect(underSudo(sudoUID, realUID, effectiveUID)).To(Equal(expected))
	},
	Entry("a plain run", "", 1000, 1000, false),
	// sudo -u factory sets the real and effective uid to the target, so only SUDO_UID reveals it.
	Entry("sudo -u factory", "1000", 2000, 2000, true),
	Entry("a setuid root binary", "", 1000, 0, true),
)

var _ = Describe("the root span", func() {
	var (
		h        *cliHarness
		recorder *tracetest.SpanRecorder
		tracer   trace.Tracer
	)

	BeforeEach(func() {
		h = newCLIHarness()
		recorder = tracetest.NewSpanRecorder()
		tracer = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)).Tracer("t")
	})

	It("is one span named for the verb, carrying the exit code, with the ids on diagnostics", func() {
		path := h.writeConfig(validConfig)
		root, err := newRootCommand(h.deps, config.SettingsViper(h.home))
		Expect(err).NotTo(HaveOccurred())

		root.SetOut(&h.out)
		root.SetErr(&h.errOut)

		code := executeTraced(context.Background(), tracer, h.logger, root, []string{"poll", "--config", path}, false)
		Expect(code).To(BeZero())

		spans := recorder.Ended()
		Expect(spans).To(HaveLen(1))
		Expect(spans[0].Name()).To(Equal("foreman.poll"))
		Expect(spans[0].Attributes()).To(ContainElement(attribute.Int(telemetry.AttrExitCode, 0)))
		Expect(spans[0].Status().Code).To(Equal(codes.Unset))
	})

	It("marks a failed run and logs the failure inside the span", func() {
		root, err := newRootCommand(h.deps, config.SettingsViper(h.home))
		Expect(err).NotTo(HaveOccurred())

		root.SetOut(&h.out)
		root.SetErr(&h.errOut)

		logger := slog.New(telemetry.LogHandler(slog.NewJSONHandler(&h.logs, nil)))
		code := executeTraced(context.Background(), tracer, logger, root, []string{"poll", "--config", "/nonexistent"}, false)
		Expect(code).To(Equal(1))

		spans := recorder.Ended()
		Expect(spans).To(HaveLen(1))
		Expect(spans[0].Status().Code).To(Equal(codes.Error))
		Expect(h.logs.String()).To(ContainSubstring(`"trace_id":"` + spans[0].SpanContext().TraceID().String() + `"`))
		Expect(h.logs.String()).To(ContainSubstring(`"span_id":"` + spans[0].SpanContext().SpanID().String() + `"`))
	})

	DescribeTable("names the verb",
		func(expected string, args ...string) {
			root, err := newRootCommand(h.deps, config.SettingsViper(h.home))
			Expect(err).NotTo(HaveOccurred())
			Expect(verbOf(root, args)).To(Equal(expected))
		},
		Entry("poll", "poll", "poll"),
		Entry("run with arguments", "run", "run", "implementer", "o/r", "5"),
		Entry("a flag first", "poll", "--config", "x", "poll"),
		Entry("an unknown verb", "unknown", "frobnicate"),
		Entry("no verb", "unknown"),
	)
})

var _ = Describe("the diagnostic logger", func() {
	It("writes one JSON object per line with level, message and attributes", func() {
		GinkgoT().Setenv(logLevelEnv, "info")

		var out bytes.Buffer

		newLogger(&out).Error("boom", "issue", 7)

		var record map[string]any
		Expect(json.Unmarshal(out.Bytes(), &record)).To(Succeed())
		Expect(bytes.Count(out.Bytes(), []byte("\n"))).To(Equal(1))
		Expect(record).To(HaveKeyWithValue("level", "ERROR"))
		Expect(record).To(HaveKeyWithValue("msg", "boom"))
		Expect(record).To(HaveKeyWithValue("issue", BeNumerically("==", 7)))
	})

	DescribeTable("leveling by FACTORY_LOG_LEVEL",
		func(name string, debugShown, infoShown, warnShown bool) {
			GinkgoT().Setenv(logLevelEnv, name)

			var out bytes.Buffer

			logger := newLogger(&out)
			shown := func(log func(string, ...any)) bool {
				out.Reset()
				log("x")

				return out.Len() > 0
			}
			Expect(shown(logger.Debug)).To(Equal(debugShown))
			Expect(shown(logger.Info)).To(Equal(infoShown))
			Expect(shown(logger.Warn)).To(Equal(warnShown))
		},
		Entry("debug", "debug", true, true, true),
		Entry("warn", "warn", false, false, true),
		Entry("unset", "", false, true, true),
		Entry("unknown", "loud", false, true, true),
	)
})
