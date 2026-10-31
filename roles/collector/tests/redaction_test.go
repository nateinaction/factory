package tests_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The rendered config is a host's collector: it receives OTLP on loopback and forwards to the
// telemetry backend. These specs render it with a second collector as its backend, so the second
// collector's debug exporter shows exactly what the first let through. Both are the real pinned
// otelcol-contrib from the dev shell. Every wait carries a deadline.

const (
	traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	spanID  = "00f067aa0ba902b7"

	// listenDeadline bounds the wait for a collector to accept connections.
	listenDeadline = 20 * time.Second
	// exportDeadline bounds the wait for the exports to reach the sink; the role's batch timeout is
	// 5s, so this is well over one batch interval.
	exportDeadline = 40 * time.Second
	// stopDeadline bounds the wait for a collector to exit after SIGTERM before it is killed.
	stopDeadline = 10 * time.Second
	postDeadline = 10 * time.Second
)

// securityEvents are what a run's stderr carries. Every field of each must reach the sink.
var securityEvents = []map[string]any{
	{
		"event": "output_rejected", "outcome": "factory_marker", "host": "blocked.example",
		"agent_role": "implementer", "repository": "o/r", "issue": 5,
	},
	{
		"event": "egress_refused", "outcome": "refused_not_routable", "host": "metadata.example",
		"agent_role": "implementer", "repository": "o/r", "issue": 5,
	},
}

func freePort() int {
	GinkgoHelper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	defer func() { Expect(l.Close()).To(Succeed()) }()
	return l.Addr().(*net.TCPAddr).Port
}

// waitForPort waits, up to listenDeadline, until something accepts a connection on the port.
func waitForPort(port int) {
	GinkgoHelper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	Eventually(func() error {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			return err
		}
		return conn.Close()
	}, listenDeadline, 100*time.Millisecond).Should(Succeed(), "collector did not listen on %s", addr)
}

// startCollector runs otelcol-contrib on the config, logging to logPath, and stops it (SIGTERM, then
// kill after stopDeadline) when the container ends.
func startCollector(config, logPath string) {
	GinkgoHelper()
	logFile, err := os.Create(logPath)
	Expect(err).NotTo(HaveOccurred())
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "otelcol-contrib", "--config", config)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = stopDeadline
	Expect(cmd.Start()).To(Succeed())
	DeferCleanup(func() {
		cancel()
		// A collector stopped by our signal exits non-zero; that is the stop working, not a failure.
		_ = cmd.Wait()
		Expect(logFile.Close()).To(Succeed())
	})
}

func post(url string, payload any) {
	GinkgoHelper()
	body, err := json.Marshal(payload)
	Expect(err).NotTo(HaveOccurred())
	client := http.Client{Timeout: postDeadline}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	defer func() { Expect(resp.Body.Close()).To(Succeed()) }()
	_, err = io.Copy(io.Discard, resp.Body)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(BeElementOf(http.StatusOK, http.StatusAccepted), url)
}

// attr is one OTLP/JSON attribute, typed from the Go value.
func attr(key string, value any) map[string]any {
	switch v := value.(type) {
	case int:
		return map[string]any{"key": key, "value": map[string]any{"intValue": strconv.Itoa(v)}}
	default:
		return map[string]any{"key": key, "value": map[string]any{"stringValue": fmt.Sprint(v)}}
	}
}

func attrs(pairs ...any) []any {
	out := make([]any, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, attr(pairs[i].(string), pairs[i+1]))
	}
	return out
}

// spanPayload is a run span with allowed attributes and one that must be dropped.
func spanPayload(now int64) map[string]any {
	return map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": attrs("service.name", "foreman")},
		"scopeSpans": []any{map[string]any{"spans": []any{map[string]any{
			"traceId": traceID, "spanId": spanID, "name": "foreman.run", "kind": 1,
			"startTimeUnixNano": strconv.FormatInt(now, 10),
			"endTimeUnixNano":   strconv.FormatInt(now+1_000_000, 10),
			"attributes": attrs(
				"factory.repo", "o/r",
				"factory.issue", 5,
				"factory.run_id", "run-old",
				// What a poll and a process report about themselves.
				"factory.skip_reason", "capacity_full",
				"factory.attempt", 2,
				"factory.verb", "start",
				"factory.exit_code", 1,
				// Never allowlisted: neither the key nor its value may leave.
				"not.allowed.attr", "secret-span-value",
			),
		}}}},
	}}}
}

func gaugePayload(name string, now int64, dataPoint map[string]any) map[string]any {
	dataPoint["timeUnixNano"] = strconv.FormatInt(now, 10)
	return map[string]any{"resourceMetrics": []any{map[string]any{
		"scopeMetrics": []any{map[string]any{"metrics": []any{map[string]any{
			"name":  name,
			"gauge": map[string]any{"dataPoints": []any{dataPoint}},
		}}}},
	}}}
}

func logPayload(now int64) map[string]any {
	return map[string]any{"resourceLogs": []any{map[string]any{
		"scopeLogs": []any{map[string]any{"logRecords": []any{map[string]any{
			"timeUnixNano": strconv.FormatInt(now, 10),
			"body":         map[string]any{"stringValue": "hello diagnostic"},
			"attributes":   attrs("factory.repo", "o/r", "not.allowed.log", "secret-log-value"),
			"traceId":      traceID,
			"spanId":       spanID,
		}}}},
	}}}
}

func sinkConfig(grpc, http int) string {
	return fmt.Sprintf(`receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 127.0.0.1:%d
      http:
        endpoint: 127.0.0.1:%d
        metrics_url_path: /api/v1/otlp/v1/metrics
        logs_url_path: /otlp/v1/logs
exporters:
  debug:
    verbosity: detailed
service:
  telemetry:
    metrics:
      level: none
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [debug]
    metrics:
      receivers: [otlp]
      exporters: [debug]
    logs:
      receivers: [otlp]
      exporters: [debug]
`, grpc, http)
}

func appendLine(path string, record map[string]any) {
	GinkgoHelper()
	line, err := json.Marshal(record)
	Expect(err).NotTo(HaveOccurred())
	appendRaw(path, string(line)+"\n")
}

func appendRaw(path, text string) {
	GinkgoHelper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	Expect(err).NotTo(HaveOccurred())
	_, err = f.WriteString(text)
	Expect(err).NotTo(HaveOccurred())
	Expect(f.Close()).To(Succeed())
}

func mkdirs(paths ...string) {
	GinkgoHelper()
	for _, p := range paths {
		Expect(os.MkdirAll(p, 0o755)).To(Succeed())
	}
}

func touch(path string) {
	GinkgoHelper()
	appendRaw(path, "")
}

// stageLogs lays out dagu's log directory and the files reached through symlinks, and returns the
// path of the run's stderr and of the three symlink targets.
func stageLogs(work, logs string) (runErr string, targets map[string]string) {
	GinkgoHelper()
	// The run's stderr file exists before the collector starts (a dagu run creates it as it runs), so
	// the receiver's start_at: end picks up what is appended afterwards.
	runDir := filepath.Join(logs, "factory-implementer", "run-old", "a1b2c3")
	mkdirs(runDir)
	runErr = filepath.Join(runDir, "implementer.stderr.log")
	touch(runErr)

	// A root-only file the dagu user must not be able to publish to Loki, reached the ways a symlink in
	// the log directory can: the link is the log file, or it is a whole run directory. The collector's
	// CAP_DAC_READ_SEARCH can read the target, so the receiver has to drop it.
	rootOnly := filepath.Join(work, "root-only")
	appendRaw(rootOnly, "")
	Expect(os.Symlink(rootOnly, filepath.Join(runDir, "leak.stderr.log"))).To(Succeed())

	outsideRun := filepath.Join(work, "outside-run")
	mkdirs(filepath.Join(outsideRun, "a1b2c3"), filepath.Join(logs, "factory-leak"))
	outside := filepath.Join(outsideRun, "a1b2c3", "sneak.stderr.log")
	touch(outside)
	Expect(os.Symlink(outsideRun, filepath.Join(logs, "factory-leak", "run-sneak"))).To(Succeed())

	// A directory whose name starts with the log directory's name is still outside it.
	siblingRun := filepath.Join(work, "dagu-logs-outside")
	mkdirs(filepath.Join(siblingRun, "a1b2c3"), filepath.Join(logs, "factory-sibling"))
	sibling := filepath.Join(siblingRun, "a1b2c3", "sneak.stderr.log")
	touch(sibling)
	Expect(os.Symlink(siblingRun, filepath.Join(logs, "factory-sibling", "run-sibling"))).To(Succeed())

	return runErr, map[string]string{
		"root-only secret":                 rootOnly,
		"symlinked directory secret":       outside,
		"similarly named directory secret": sibling,
	}
}

// exercise sends every kind of record through the front collector and returns what the sink logged.
func exercise(work string, frontHTTP int, runErr string, targets map[string]string) string {
	GinkgoHelper()
	now := time.Now().UnixNano()
	front := fmt.Sprintf("http://127.0.0.1:%d", frontHTTP)
	post(front+"/v1/traces", spanPayload(now))
	post(front+"/v1/metrics", gaugePayload("factory.poll.issues", now, map[string]any{
		"asInt": "3", "startTimeUnixNano": strconv.FormatInt(now, 10),
		"attributes": attrs("factory.repo", "o/r", "factory.state", "factory:review",
			"not.allowed.metric", "secret-metric-value"),
	}))
	// Host health reports generic labels, which the role renames before redaction.
	post(front+"/v1/metrics", gaugePayload("factory_host_throttled", now, map[string]any{
		"asInt": "1", "attributes": attrs("kind", "throttled", "state", "now"),
	}))
	post(front+"/v1/logs", logPayload(now))

	// A run's stderr as dagu writes it, appended after the receiver started (start_at: end).
	time.Sleep(time.Second) // a fixed settle time for the receiver's first poll, not a wait on a result
	for _, event := range securityEvents {
		record := map[string]any{
			"time": "2026-01-01T00:00:00.123456789Z", "level": "WARN", "msg": "security event",
			"trace_id": traceID, "span_id": spanID, "not.allowed.filelog": "secret-filelog-value",
		}
		for k, v := range event {
			record[k] = v
		}
		appendLine(runErr, record)
	}
	// A line that is not a JSON record, as a tool writing to fd 2 could leave behind: it must be
	// dropped rather than shipped as raw text to Loki.
	appendRaw(runErr, "private raw tool output\n")

	// The symlink targets, written after the receiver started. A record that came back through a
	// symlink must never reach Loki, even though the collector can read the target.
	for message, path := range targets {
		appendLine(path, map[string]any{
			"time": "2026-01-01T00:00:00.223456789Z", "level": "INFO", "msg": message,
		})
	}

	sinkLog := filepath.Join(work, "sink.log")
	wanted := []string{
		"factory.repo", "factory.issue", "factory.state",
		"factory.host_health.kind", "factory.host_health.state",
		"factory.skip_reason", "factory.attempt", "factory.verb", "factory.exit_code",
		"hello diagnostic", "security event", "egress_refused",
	}
	Eventually(func() []string {
		out := contents(sinkLog)
		var missing []string
		for _, marker := range wanted {
			if !strings.Contains(out, marker) {
				missing = append(missing, marker)
			}
		}
		return missing
	}, exportDeadline, 500*time.Millisecond).Should(BeEmpty(), func() string {
		return "front collector:\n" + contents(filepath.Join(work, "front.log")) +
			"sink collector:\n" + contents(sinkLog)
	})
	return contents(sinkLog)
}

var _ = Describe("the real collector's redaction", Ordered, func() {
	var out string

	BeforeAll(func() {
		// Real path, because the collector compares a file's resolved path with the log directory: a
		// temporary tree reached through a symlink (/tmp on macOS is /private/tmp) would differ.
		work, err := filepath.EvalSymlinks(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		logs := filepath.Join(work, "dagu-logs")
		mkdirs(logs)
		frontGRPC, frontHTTP, sinkGRPC, sinkHTTP := freePort(), freePort(), freePort(), freePort()

		c, _ := renderCollector("127.0.0.1", map[string]any{
			"factory_collector_grpc_port":       frontGRPC,
			"factory_collector_http_port":       frontHTTP,
			"factory_telemetry_tempo_otlp_port": sinkGRPC,
			"factory_telemetry_prometheus_port": sinkHTTP,
			"factory_telemetry_loki_port":       sinkHTTP,
			"factory_collector_state_dir":       filepath.Join(work, "state"),
			"factory_dagu_logs_dir":             logs,
		})
		runErr, targets := stageLogs(work, logs)

		// The sink accepts what the role's exporters send: OTLP/gRPC for traces, and the Prometheus and
		// Loki OTLP HTTP paths for metrics and diagnostics.
		sink := filepath.Join(work, "sink.yaml")
		Expect(os.WriteFile(sink, []byte(sinkConfig(sinkGRPC, sinkHTTP)), 0o600)).To(Succeed())
		startCollector(sink, filepath.Join(work, "sink.log"))
		startCollector(filepath.Join(c.dir, "config.yaml"), filepath.Join(work, "front.log"))
		waitForPort(sinkHTTP)
		waitForPort(frontHTTP)

		out = exercise(work, frontHTTP, runErr, targets)
	})

	It("a non-allowlisted attribute never leaves the collector", func() {
		for _, marker := range []string{
			"not.allowed.attr", "secret-span-value",
			"not.allowed.metric", "secret-metric-value",
			"not.allowed.log", "secret-log-value",
			"not.allowed.filelog", "secret-filelog-value",
			"-> kind:", "-> state:",
		} {
			Expect(out).NotTo(ContainSubstring(marker), "the collector let a non-allowlisted attribute through")
		}
	})

	It("a line the collector cannot parse as JSON is not shipped", func() {
		Expect(out).NotTo(ContainSubstring("private raw tool output"))
	})

	It("a file reached through a symlink is never shipped", func() {
		for _, secret := range []string{
			"root-only secret", "symlinked directory secret", "similarly named directory secret",
		} {
			Expect(out).NotTo(ContainSubstring(secret))
		}
	})

	// A poll's skip reason and a process's verb, exit code and attempt are what #126 and #122 read, so
	// each must survive the allowlist on the span that carries it.
	It("a run's skip reason, attempt, verb and exit code survive the allowlist", func() {
		for _, marker := range []string{
			"factory.skip_reason: Str(capacity_full)", "factory.attempt: Int(2)",
			"factory.verb: Str(start)", "factory.exit_code: Int(1)",
		} {
			Expect(out).To(ContainSubstring(marker))
		}
	})

	// Every security event ships with the whole record. They share one body, so one query returns all
	// of them; the record itself names the event, outcome, host and run it belongs to.
	It("every security event ships with its whole record", func() {
		records := strings.Split(out, "Body: Str(security event)")[1:]
		for _, event := range securityEvents {
			var shipped string
			for _, record := range records {
				if strings.Contains(record, fmt.Sprintf("event: Str(%s)", event["event"])) {
					shipped = record
				}
			}
			Expect(shipped).NotTo(BeEmpty(), "security event %v did not reach the sink", event["event"])
			for _, field := range []string{
				fmt.Sprintf("outcome: Str(%s)", event["outcome"]),
				fmt.Sprintf("host: Str(%s)", event["host"]),
				fmt.Sprintf("agent_role: Str(%s)", event["agent_role"]),
				fmt.Sprintf("repository: Str(%s)", event["repository"]),
				fmt.Sprintf("issue: Double(%d)", event["issue"]),
			} {
				Expect(shipped).To(ContainSubstring(field), "the collector dropped a security event field")
			}
		}
	})

	// The filelog record's trace id survives as the record's trace context and as an attribute Loki's
	// derived field matches, so a trace can be followed to its diagnostics.
	It("the diagnostics keep their trace id", func() {
		start := strings.Index(out, "Body: Str(security event)")
		Expect(start).To(BeNumerically(">=", 0))
		Expect(out[start:min(start+600, len(out))]).To(ContainSubstring(traceID))
		Expect(out).To(ContainSubstring("trace_id: Str(" + traceID))
	})
})
