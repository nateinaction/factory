package telemetry

import (
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// The one place foreman's instruments fall back to a no-op. Every builder here returns an instrument
// that records nothing rather than an error, because telemetry never fails or delays a run: a meter
// that refuses a name leaves the caller measuring nowhere.

// Int64Counter builds an int64 counter, falling back to a no-op counter.
func Int64Counter(m metric.Meter, name, description string) metric.Int64Counter {
	counter, err := m.Int64Counter(name, metric.WithDescription(description))
	if err == nil {
		return counter
	}

	// The name is a constant, so the no-op meter refuses it only if the SDK does not support it
	// either; its zero value still records nothing.
	counter, err = metricnoop.NewMeterProvider().Meter(scope).Int64Counter(name)
	if err == nil {
		return counter
	}

	return metricnoop.Int64Counter{}
}

// Int64Gauge builds an int64 gauge, falling back to a no-op gauge.
func Int64Gauge(m metric.Meter, name, description string) metric.Int64Gauge {
	gauge, err := m.Int64Gauge(name, metric.WithDescription(description))
	if err == nil {
		return gauge
	}

	gauge, err = metricnoop.NewMeterProvider().Meter(scope).Int64Gauge(name)
	if err == nil {
		return gauge
	}

	return metricnoop.Int64Gauge{}
}

// Float64Counter builds a float64 counter, falling back to a no-op counter.
func Float64Counter(m metric.Meter, name, description string) metric.Float64Counter {
	counter, err := m.Float64Counter(name, metric.WithDescription(description))
	if err == nil {
		return counter
	}

	counter, err = metricnoop.NewMeterProvider().Meter(scope).Float64Counter(name)
	if err == nil {
		return counter
	}

	return metricnoop.Float64Counter{}
}

// TracerOr returns t, or a tracer that records nothing when t is nil.
func TracerOr(t trace.Tracer) trace.Tracer {
	if t == nil {
		return tracenoop.NewTracerProvider().Tracer(scope)
	}

	return t
}

// MeterOr returns m, or a meter that records nothing when m is nil.
func MeterOr(m metric.Meter) metric.Meter {
	if m == nil {
		return metricnoop.NewMeterProvider().Meter(scope)
	}

	return m
}

// EnvNames are the standard OTEL_* variables foreman's process needs to export. A supervisor that
// starts a child across sudo must pass these explicitly, because sudo resets the environment.
var EnvNames = []string{ //nolint:gochecknoglobals // one read-only list of names, not state
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_SERVICE_NAME",
}

// ExportEnv is the export environment in lookup, keeping only the names EnvNames lists and only the
// values that are set. It is what `start` hands the factory half across sudo; an unset variable is
// absent rather than empty, so it cannot override a default.
func ExportEnv(lookup func(string) string) map[string]string {
	env := make(map[string]string, len(EnvNames))

	for _, name := range EnvNames {
		if value := lookup(name); value != "" {
			env[name] = value
		}
	}

	return env
}
