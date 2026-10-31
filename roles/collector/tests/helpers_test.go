package tests_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"

	"github.com/nateinaction/factory/roletest"
)

// commandTimeout bounds every short-lived command a spec runs.
const commandTimeout = time.Minute

// groupVars are the group_vars values the config is checked against, as render.yml writes them.
type groupVars struct {
	AllowedAttributes []string `json:"factory_collector_allowed_attributes"`
	QueueSize         int      `json:"factory_collector_queue_size"`
	TempoPort         int      `json:"factory_telemetry_tempo_otlp_port"`
	PrometheusPort    int      `json:"factory_telemetry_prometheus_port"`
	LokiPort          int      `json:"factory_telemetry_loki_port"`
	MemoryLimitMib    int      `json:"factory_collector_memory_limit_mib"`
	StateDir          string   `json:"factory_collector_state_dir"`
	DaguLogsDir       string   `json:"factory_dagu_logs_dir"`
}

// collector is one render of the role: the parsed config, the unit text and the group_vars.
type collector struct {
	dir    string
	config map[string]any
	unit   string
	vars   groupVars
}

// renderCollector renders the role for the backend, plus any extra vars, into a fresh directory.
func renderCollector(backend string, extra map[string]any) (collector, roletest.Rendered) {
	GinkgoHelper()
	rendered := roletest.Render("render.yml", roletest.Options{Vars: withBackend(backend, extra)})
	return load(rendered.Dir), rendered
}

func withBackend(backend string, extra map[string]any) map[string]any {
	vars := map[string]any{"factory_collector_backend_host": backend}
	for k, v := range extra {
		vars[k] = v
	}
	return vars
}

func load(dir string) collector {
	GinkgoHelper()
	c := collector{dir: dir, unit: contents(filepath.Join(dir, "otelcol.service"))}
	Expect(yaml.Unmarshal([]byte(contents(filepath.Join(dir, "config.yaml"))), &c.config)).To(Succeed())
	Expect(json.Unmarshal([]byte(contents(filepath.Join(dir, "vars.json"))), &c.vars)).To(Succeed())
	return c
}

func contents(path string) string {
	GinkgoHelper()
	b, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	return string(b)
}

// at walks a parsed config along the keys and fails the spec where a key is missing.
func at(node any, keys ...string) any {
	GinkgoHelper()
	for i, key := range keys {
		m, ok := node.(map[string]any)
		Expect(ok).To(BeTrue(), "%s is not a map", strings.Join(keys[:i], "."))
		node, ok = m[key]
		Expect(ok).To(BeTrue(), "no %s", strings.Join(keys[:i+1], "."))
	}
	return node
}

// strs reads a parsed list of strings.
func strs(node any) []string {
	GinkgoHelper()
	list, ok := node.([]any)
	Expect(ok).To(BeTrue(), "not a list: %v", node)
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		Expect(ok).To(BeTrue(), "not a string: %v", item)
		out = append(out, s)
	}
	return out
}

// before asserts that a comes before b in list, and that both are in it.
func before(list []string, a, b string) {
	GinkgoHelper()
	ia, ib := slices.Index(list, a), slices.Index(list, b)
	Expect(ia).To(BeNumerically(">=", 0), "%s is not in %v", a, list)
	Expect(ib).To(BeNumerically(">=", 0), "%s is not in %v", b, list)
	Expect(ia).To(BeNumerically("<", ib), "%s comes after %s in %v", a, b, list)
}

// run runs a short-lived command in dir under a deadline and fails the spec, with its output, if it fails.
func run(dir string, env []string, name string, args ...string) string {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "%s %v: %s", name, args, out)
	return string(out)
}
