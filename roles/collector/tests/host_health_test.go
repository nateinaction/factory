package tests_test

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// What the collector renders for host health: it scrapes node_exporter on loopback, keeps only the
// health metric families, names the host on every metric, and lets the health attributes through
// redaction under the factory namespace. Run against the real group_vars.
var _ = Describe("the collector's host health pipeline", Ordered, func() {
	var c collector

	scrape := func() any {
		GinkgoHelper()
		return at(c.config, "receivers", "prometheus/host_health", "config", "scrape_configs", 0)
	}

	BeforeAll(func() {
		c, _ = renderCollector("telemetry.example", nil)
	})

	It("the collector scrapes host health on loopback", func() {
		targets := at(scrape(), "static_configs", 0, "targets")
		Expect(strs(targets)).To(Equal([]string{fmt.Sprintf("127.0.0.1:%d", c.vars.HostHealthPort)}))
		Expect(at(scrape(), "scrape_interval")).To(MatchRegexp(`s$`))
		Expect(strs(at(c.config, "service", "pipelines", "metrics", "receivers"))).To(ContainElement("prometheus/host_health"))
	})

	It("only the health metric families are scraped", func() {
		keep := at(scrape(), "metric_relabel_configs", 0)
		Expect(at(keep, "action")).To(Equal("keep"))
		// Prometheus anchors metric relabel regexes.
		pattern := regexp.MustCompile("^(?:" + at(keep, "regex").(string) + ")$")
		for _, name := range []string{
			"factory_host_temperature_celsius",
			"factory_host_throttled",
			"node_memory_MemTotal_bytes",
			"node_memory_MemAvailable_bytes",
			"node_filesystem_size_bytes",
			"node_filesystem_avail_bytes",
		} {
			Expect(pattern.MatchString(name)).To(BeTrue(), name)
		}
		Expect(pattern.MatchString("node_load1")).To(BeFalse())
	})

	It("every metric from a host names that host", func() {
		var resourceStatements []string
		for _, step := range list(at(c.config, "processors", "transform/host", "metric_statements")) {
			if at(step, "context") == "resource" {
				resourceStatements = append(resourceStatements, strs(at(step, "statements"))...)
			}
		}
		Expect(strings.Join(resourceStatements, "\n")).To(ContainSubstring("host.name"))
		before(strs(at(c.config, "service", "pipelines", "metrics", "processors")), "transform/host", "redaction")
	})

	It("the health metric attributes survive redaction under the factory namespace", func() {
		allowed := strs(at(c.config, "processors", "redaction", "allowed_keys"))
		for _, key := range []string{
			"host.name", "factory.worker_host", "factory.host_health.mountpoint",
			"factory.host_health.fstype", "factory.host_health.device",
			"factory.host_health.kind", "factory.host_health.state",
		} {
			Expect(slices.Contains(allowed, key)).To(BeTrue(), key)
		}
		// The bare Prometheus names are not admitted for every signal: transform/host renames them first.
		for _, key := range []string{"mountpoint", "fstype", "device", "kind", "state"} {
			Expect(slices.Contains(allowed, key)).To(BeFalse(), key)
		}
	})
})
