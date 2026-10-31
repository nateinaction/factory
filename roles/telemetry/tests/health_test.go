package tests_test

import (
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

// setsAttribute finds the datapoint attributes a collector transform statement sets.
var setsAttribute = regexp.MustCompile(`set\(datapoint\.attributes\["([^"]+)"\]`)

// bareLabel matches a Prometheus label the collector deletes, used as a filter.
func bareLabel(name, expr string) bool {
	return regexp.MustCompile(`(?:^|[^a-z_])` + name + `="`).MatchString(expr)
}

// The host health dashboard pairs run duration with temperature and throttling per host, and
// Prometheus promotes each host's name to a label. Run against the real group_vars.
var _ = Describe("the host health dashboard", Ordered, func() {
	var (
		dir       string
		dash      any
		panels    []any
		collector any
	)

	// expr is the query of the first panel whose title, lowercased, contains the word.
	expr := func(word string) string {
		GinkgoHelper()
		return text(at(panel(panels, word), "targets", 0, "expr"))
	}

	BeforeAll(func() {
		dir = roletest.Render("health.yml", roletest.Options{}).Dir
		dash = jsonDoc(dir, "host-health.json")
		panels = list(at(dash, "panels"))
		collector = yamlDoc(dir, "collector.yaml")
	})

	It("the dashboard shows run duration per worker host", func() {
		run := expr("run duration")
		Expect(run).To(ContainSubstring("traces_span_metrics_duration_milliseconds_bucket"))
		Expect(run).To(ContainSubstring("host_name"))
		Expect(run).To(ContainSubstring("span_name"))
	})

	It("the dashboard shows each host temperature beside the durations", func() {
		temp := expr("temperature")
		Expect(temp).To(ContainSubstring("factory_host_temperature_celsius"))
		Expect(temp).To(ContainSubstring("host_name"))
	})

	It("the dashboard shows each host throttling beside the durations", func() {
		throttle := expr("throttl")
		Expect(throttle).To(ContainSubstring("factory_host_throttled"))
		Expect(throttle).To(ContainSubstring("host_name"))
	})

	It("the dashboard shows memory and disk use per host", func() {
		memory, disk := expr("memory"), expr("disk")
		Expect(memory).To(ContainSubstring("node_memory_MemAvailable_bytes"))
		Expect(memory).To(ContainSubstring("host_name"))
		Expect(disk).To(ContainSubstring("node_filesystem_avail_bytes"))
		Expect(disk).To(ContainSubstring("host_name"))
	})

	It("every legend names its series with a Prometheus placeholder, which Grafana fills from the labels", func() {
		for _, p := range panels {
			legend := text(at(p, "targets", 0, "legendFormat"))
			Expect(legend).To(ContainSubstring("{{host_name}}"), text(at(p, "title")))
			Expect(legend).NotTo(ContainSubstring("__field"), text(at(p, "title")))
		}
	})

	It("throttling is a row per host and kind that colours the stretches it was active", func() {
		Expect(at(panel(panels, "throttl"), "type")).To(Equal("state-timeline"))
	})

	It("prometheus promotes each host's name to a metric label", func() {
		prom := yamlDoc(dir, "prometheus.yaml")
		Expect(list(at(prom, "otlp", "promote_resource_attributes"))).To(ContainElement("host.name"))
	})

	// The collector renames the bare Prometheus attributes into the factory namespace, and Prometheus
	// turns their dots into underscores. The names are taken from the rename itself, so a dashboard
	// query that drifts from the pipeline fails here.
	It("the dashboard's health filters name the labels the collector produces", func() {
		produced := map[string]bool{}
		for _, step := range list(at(collector, "processors", "transform/host", "metric_statements")) {
			if at(step, "context") != "datapoint" {
				continue
			}
			for _, statement := range list(at(step, "statements")) {
				for _, m := range setsAttribute.FindAllStringSubmatch(text(statement), -1) {
					produced[strings.ReplaceAll(m[1], ".", "_")] = true
				}
			}
		}
		throttle, disk := expr("throttl"), expr("disk")
		Expect(throttle).To(ContainSubstring(`factory_host_health_state="now"`))
		Expect(throttle).To(ContainSubstring("factory_host_health_kind"))
		Expect(disk).To(ContainSubstring(`factory_host_health_mountpoint="/"`))
		// The bare names are deleted by the collector, so filtering on them matches nothing.
		Expect(bareLabel("state", throttle)).To(BeFalse(), throttle)
		Expect(bareLabel("mountpoint", disk)).To(BeFalse(), disk)
		used := regexp.MustCompile(`factory_host_health_[a-z_]+`)
		for _, p := range panels {
			for _, label := range used.FindAllString(text(at(p, "targets", 0, "expr")), -1) {
				Expect(produced).To(HaveKey(label))
			}
		}
	})

	It("every dashboard panel can be split by host", func() {
		for _, p := range panels {
			Expect(text(at(p, "targets", 0, "expr"))).To(ContainSubstring("host_name"), text(at(p, "title")))
		}
	})

	It("every dashboard panel reads the provisioned Prometheus datasource", func() {
		for _, p := range panels {
			Expect(at(p, "datasource", "uid")).To(Equal("prometheus"), text(at(p, "title")))
		}
	})

	It("the run span name is a dashboard variable, defaulting to the group var", func() {
		var variable any
		for _, t := range list(at(dash, "templating", "list")) {
			if at(t, "name") == "run_span" {
				variable = t
			}
		}
		Expect(variable).NotTo(BeNil())
		Expect(at(variable, "query")).To(Equal(at(jsonDoc(dir, "vars.json"), "factory_telemetry_run_span")))
	})

	It("the provider loads dashboards from the factory directory", func() {
		provider := readFile(dir, "dashboards.yaml")
		Expect(provider).To(ContainSubstring("path: /etc/grafana/dashboards"))
		Expect(provider).To(ContainSubstring("folder: Factory"))
	})
})

// panel is the first panel whose title, lowercased, contains the word.
func panel(panels []any, word string) any {
	GinkgoHelper()
	for _, p := range panels {
		if strings.Contains(strings.ToLower(text(at(p, "title"))), word) {
			return p
		}
	}
	Fail("no panel titled with " + word)
	return nil
}
