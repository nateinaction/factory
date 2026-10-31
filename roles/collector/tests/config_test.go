package tests_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

// What the collector role renders to: OTLP on loopback only, an attribute allowlist that redacts
// everything else, foreman's diagnostics shipped from dagu's run logs, span-derived metrics, a bounded
// local buffer, and a backend exporter only once one is named. Run against the real group_vars.

var pipelines = []string{"traces", "metrics", "logs"}

func hasLine(text, line string) bool {
	return slices.Contains(strings.Split(text, "\n"), line)
}

var _ = Describe("the collector role with no backend yet", Ordered, func() {
	var (
		c      collector
		second roletest.Rendered
	)

	BeforeAll(func() {
		var first roletest.Rendered
		c, first = renderCollector("", nil)
		// The same play again, into the same directory.
		second = roletest.Render("render.yml", roletest.Options{
			Vars: withBackend("", map[string]any{"render_dir": first.Dir}),
		})
	})

	It("the pinned collector accepts the rendered config", func() {
		run(".", nil, "otelcol-contrib", "validate", "--config="+filepath.Join(c.dir, "config.yaml"))
	})

	It("OTLP gRPC listens on loopback", func() {
		Expect(at(c.config, "receivers", "otlp", "protocols", "grpc", "endpoint")).To(Equal("127.0.0.1:4317"))
	})

	It("OTLP HTTP listens on loopback", func() {
		Expect(at(c.config, "receivers", "otlp", "protocols", "http", "endpoint")).To(Equal("127.0.0.1:4318"))
	})

	It("nothing in the config binds a wildcard address", func() {
		Expect(contents(filepath.Join(c.dir, "config.yaml"))).NotTo(MatchRegexp(`0\.0\.0\.0|\[::\]`))
	})

	It("the collector's own metrics endpoint is off", func() {
		Expect(at(c.config, "service", "telemetry", "metrics", "level")).To(Equal("none"))
	})

	It("redaction drops every key but the allowlist", func() {
		r := at(c.config, "processors", "redaction")
		Expect(at(r, "allow_all_keys")).To(BeFalse())
		Expect(strs(at(r, "allowed_keys"))).To(Equal(c.vars.AllowedAttributes))
	})

	It("redaction runs on traces and metrics before anything leaves", func() {
		for _, name := range pipelines {
			procs := strs(at(c.config, "service", "pipelines", name, "processors"))
			before(procs, "memory_limiter", "redaction")
			before(procs, "redaction", "batch")
		}
	})

	It("host health labels are renamed before redaction", func() {
		procs := strs(at(c.config, "service", "pipelines", "metrics", "processors"))
		before(procs, "transform/host", "redaction")
		statements := fmt.Sprint(at(c.config, "processors", "transform/host"))
		Expect(statements).To(ContainSubstring("factory.host_health.kind"))
		Expect(statements).To(ContainSubstring("factory.host_health.state"))
		Expect(statements).To(ContainSubstring("factory.host_health.mountpoint"))
	})

	It("the allowlist keeps the issue, state, failure kind and usage a run needs", func() {
		Expect(c.vars.AllowedAttributes).To(ContainElements(
			"factory.issue", "factory.state", "factory.failure_kind", "factory.run_id",
			"factory.peak_context", "factory.cost", "gen_ai.request.model",
			"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens"))
	})

	It("the allowlist keeps a poll's skip reason and a process's verb and exit code", func() {
		Expect(c.vars.AllowedAttributes).To(ContainElements(
			"factory.skip_reason", "factory.attempt", "factory.verb", "factory.exit_code"))
	})

	It("the allowlist keeps every field of a security event's record", func() {
		Expect(c.vars.AllowedAttributes).To(ContainElements(
			"event", "outcome", "host", "agent_role", "repository", "issue"))
	})

	It("the allowlist does not admit generic host health keys", func() {
		for _, generic := range []string{"kind", "state", "mountpoint", "fstype", "device"} {
			Expect(c.vars.AllowedAttributes).NotTo(ContainElement(generic))
		}
		Expect(c.vars.AllowedAttributes).To(ContainElements(
			"factory.host_health.kind", "factory.host_health.state",
			"factory.host_health.mountpoint", "factory.host_health.fstype",
			"factory.host_health.device"))
	})

	It("spanmetrics turns spans into metrics", func() {
		Expect(at(c.config, "connectors")).To(HaveKey("spanmetrics"))
		Expect(strs(at(c.config, "service", "pipelines", "traces", "exporters"))).To(ContainElement("spanmetrics"))
		Expect(strs(at(c.config, "service", "pipelines", "metrics", "receivers"))).To(ContainElement("spanmetrics"))
	})

	It("spanmetrics dimensions stay inside the allowlist", func() {
		spanmetrics := at(c.config, "connectors", "spanmetrics").(map[string]any)
		dimensions, _ := spanmetrics["dimensions"].([]any) // no dimensions is an empty list, which is inside any allowlist
		for _, d := range dimensions {
			Expect(c.vars.AllowedAttributes).To(ContainElement(at(d, "name")))
		}
	})

	// dagu 2.21 writes a step's standard error to <dag>/dag-run_*/run_*/<step>.*.err. Earlier runs left
	// <dag>/<run>/<attempt>/<step>.stderr.log, and both are still on disk.
	It("dagu's run diagnostics are read from the factory DAGs' stderr", func() {
		r := at(c.config, "receivers", "filelog/foreman")
		Expect(strs(at(r, "include"))).To(Equal([]string{
			c.vars.DaguLogsDir + "/factory-*/dag-run_*/run_*/*.err",
			c.vars.DaguLogsDir + "/factory-*/*/*/*.stderr.log",
		}))
		Expect(strs(at(c.config, "service", "extensions"))).To(ContainElement(at(r, "storage")))
		var types []string
		for _, op := range at(r, "operators").([]any) {
			types = append(types, at(op, "type").(string))
		}
		Expect(types).To(Equal([]string{"filter", "json_parser", "trace_parser", "move"}))
		Expect(strs(at(c.config, "service", "pipelines", "logs", "receivers"))).To(ContainElement("filelog/foreman"))
	})

	It("a file reached through a symlink is dropped before it is parsed", func() {
		r := at(c.config, "receivers", "filelog/foreman")
		Expect(at(r, "include_file_path_resolved")).To(BeTrue())
		op := at(r, "operators").([]any)[0]
		Expect(at(op, "type")).To(Equal("filter"))
		expr := at(op, "expr").(string)
		Expect(expr).To(ContainSubstring("path_resolved"))
		// The trailing slash keeps a sibling directory whose name starts the same way out of the prefix.
		Expect(expr).To(ContainSubstring(c.vars.DaguLogsDir + `/"`))
	})

	It("the JSON parser drops a line it cannot parse instead of shipping it raw", func() {
		for _, op := range at(c.config, "receivers", "filelog/foreman", "operators").([]any) {
			if at(op, "type") == "json_parser" {
				Expect(at(op, "on_error")).To(Equal("drop"))
				return
			}
		}
		Fail("no json_parser operator")
	})

	It("the filelog receiver resumes from disk after a restart", func() {
		storage := at(c.config, "extensions", "file_storage/offsets")
		Expect(at(storage, "directory")).To(Equal(c.vars.StateDir + "/offsets"))
		Expect(at(storage, "create_directory")).To(BeTrue())
	})

	It("no backend means nothing is exported", func() {
		for name := range at(c.config, "exporters").(map[string]any) {
			Expect(name).NotTo(HavePrefix("otlp"))
		}
		for _, name := range pipelines {
			Expect(strs(at(c.config, "service", "pipelines", name, "exporters"))).To(ContainElement("nop"), name)
		}
	})

	It("a second render reports no changes", func() {
		Expect(second.Log).To(ContainSubstring("changed=0"))
	})
})

var _ = Describe("the collector role with a backend named", Ordered, func() {
	var c collector

	BeforeAll(func() {
		c, _ = renderCollector("telemetry.example", nil)
	})

	It("the pinned collector accepts the backend config", func() {
		run(".", nil, "otelcol-contrib", "validate", "--config="+filepath.Join(c.dir, "config.yaml"))
	})

	It("a named backend receives traces, metrics and diagnostics on the backend's ports", func() {
		e := at(c.config, "exporters")
		Expect(at(e, "otlp_grpc/traces", "endpoint")).To(Equal(fmt.Sprintf("telemetry.example:%d", c.vars.TempoPort)))
		Expect(at(e, "otlp_http/metrics", "endpoint")).To(Equal(fmt.Sprintf("http://telemetry.example:%d/api/v1/otlp", c.vars.PrometheusPort)))
		Expect(at(e, "otlp_http/logs", "endpoint")).To(Equal(fmt.Sprintf("http://telemetry.example:%d/otlp", c.vars.LokiPort)))
		p := at(c.config, "service", "pipelines")
		Expect(strs(at(p, "traces", "exporters"))).To(ContainElement("otlp_grpc/traces"))
		Expect(strs(at(p, "metrics", "exporters"))).To(ContainElement("otlp_http/metrics"))
		Expect(strs(at(p, "logs", "exporters"))).To(ContainElement("otlp_http/logs"))
	})

	It("the unreachable-server buffer is bounded and survives a restart", func() {
		for _, name := range []string{"otlp_grpc/traces", "otlp_http/metrics", "otlp_http/logs"} {
			q := at(c.config, "exporters", name, "sending_queue")
			Expect(at(q, "enabled")).To(BeTrue(), name)
			Expect(at(q, "queue_size")).To(BeEquivalentTo(c.vars.QueueSize), name)
			Expect(strs(at(c.config, "service", "extensions"))).To(ContainElement(at(q, "storage")), name)
			Expect(at(q, "storage")).To(HavePrefix("file_storage"), name)
		}
	})

	It("the collector creates its queue directory, so a fresh host does not crash-loop", func() {
		storage := at(c.config, "extensions", "file_storage/queue")
		Expect(at(storage, "directory")).To(Equal(c.vars.StateDir + "/queue"))
		Expect(at(storage, "create_directory")).To(BeTrue())
	})

	It("the memory limit is set", func() {
		Expect(at(c.config, "processors", "memory_limiter", "limit_mib")).To(BeEquivalentTo(c.vars.MemoryLimitMib))
	})

	It("the unit runs as the unprivileged collector user", func() {
		Expect(hasLine(c.unit, "User=otelcol")).To(BeTrue(), c.unit)
	})

	It("the unit runs the contrib binary on the rendered config", func() {
		Expect(hasLine(c.unit, "ExecStart=/nix/var/nix/profiles/default/bin/otelcol-contrib --config=/etc/otelcol/config.yaml")).To(BeTrue(), c.unit)
	})

	It("the unit may read dagu's 0600 run logs and nothing more", func() {
		Expect(hasLine(c.unit, "AmbientCapabilities=CAP_DAC_READ_SEARCH")).To(BeTrue(), c.unit)
		Expect(hasLine(c.unit, "CapabilityBoundingSet=CAP_DAC_READ_SEARCH")).To(BeTrue(), c.unit)
	})

	It("the unit may write only the collector's state directory", func() {
		Expect(hasLine(c.unit, "ReadWritePaths=/var/lib/otelcol")).To(BeTrue(), c.unit)
	})
})

// A host's own name resolves to 127.0.1.1 on Debian, where the backend does not listen, so the server's
// collector must use the loopback the backend binds; the other hosts reach it by name over the tailnet.
var _ = Describe("where each host's collector sends to", func() {
	backendHost := func(host string) string {
		GinkgoHelper()
		// Ad-hoc ansible finds group_vars beside the working directory, so run from the repo root.
		out := run("../../..", []string{"ANSIBLE_CONFIG=ansible.cfg"},
			"ansible", "-i", "inventory/hosts.yml", host, "-c", "local",
			"-m", "debug", "-a", "var=factory_collector_backend_host")
		_, body, found := strings.Cut(out, "=>")
		Expect(found).To(BeTrue(), "no result in %s", out)
		var result map[string]any
		Expect(json.Unmarshal([]byte(body), &result)).To(Succeed())
		return result["factory_collector_backend_host"].(string)
	}

	It("the server's collector sends to the loopback its backend binds", func() {
		Expect(backendHost("factory-1")).To(Equal("127.0.0.1"))
	})

	It("a worker's collector sends to the server by name", func() {
		Expect(backendHost("factory-2")).To(Equal("factory-1"))
	})
})
