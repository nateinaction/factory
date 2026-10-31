package tests_test

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

// rule is one rendered Prometheus alert rule.
type rule struct {
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	Annotations map[string]string `yaml:"annotations"`
}

// event is one synthetic event: the series that makes an alert fire, the time to evaluate it, and the
// labels the alert must carry once it has. The series use the label names Foreman's metric attributes
// become in Prometheus (factory.event becomes factory_event), not the names a rule happens to select
// on, so a rule that selects another name fires nothing. The expected labels are written here, not
// copied from the rule: a rule that drops, renames or adds a label fails. A security alert aggregates
// away its event and outcome but keeps the issue; the reaped-run alert keeps the issue too; an
// aggregate alert keeps only the role and repo. No alert keeps a trace id.
type event struct {
	series []string
	eval   string
	labels map[string]string
}

var perIssue = map[string]string{
	"severity": "critical", "factory_agent_role": "implementer", "factory_repo": "o/r", "factory_issue": "5",
}

var perRole = map[string]string{
	"severity": "warning", "factory_agent_role": "implementer", "factory_repo": "o/r",
}

// events is one synthetic event per alert.
var events = map[string]event{
	"ForemanEgressNotRoutable": {
		series: []string{series("egress_refused", "refused_not_routable")},
		eval:   "5m", labels: perIssue,
	},
	"ForemanProxyBadToken": {
		series: []string{series("key_proxy_refused", "refused_bad_token")},
		eval:   "5m", labels: perIssue,
	},
	"ForemanOutputFactoryMarker": {
		series: []string{series("output_rejected", "factory_marker")},
		eval:   "5m", labels: perIssue,
	},
	"ForemanAgentRoleFailureRate": {
		series: []string{
			`factory_run_outcomes_total{factory_outcome="failed",factory_agent_role="implementer",factory_repo="o/r"}`,
			`factory_run_outcomes_total{factory_outcome="completed",factory_agent_role="implementer",factory_repo="o/r"}`,
		},
		eval: "10m", labels: perRole,
	},
	"ForemanDailyModelCost": {
		series: []string{`factory_run_cost_total{factory_agent_role="implementer",factory_repo="o/r",gen_ai_request_model="m"}`},
		eval:   "10m", labels: perRole,
	},
	"ForemanDeadRunReaped": {
		series: []string{`factory_runs_reaped_total{factory_agent_role="implementer",factory_repo="o/r",factory_issue="5"}`},
		eval:   "5m",
		labels: map[string]string{
			"severity": "warning", "factory_agent_role": "implementer", "factory_repo": "o/r", "factory_issue": "5",
		},
	},
}

func series(eventName, outcome string) string {
	return `factory_security_events_total{factory_event="` + eventName + `",factory_outcome="` + outcome +
		`",factory_agent_role="implementer",factory_repo="o/r",factory_issue="5"}`
}

// sampleLabels are what the synthetic series' labels put where an annotation templates a label.
var sampleLabels = map[string]string{"factory_agent_role": "implementer", "factory_repo": "o/r", "factory_issue": "5"}

var placeholder = regexp.MustCompile(`\{\{\s*\$labels\.(\w+)\s*\}\}`)

// expand fills a rule's label placeholders the way Prometheus does for the sample labels.
func expand(s string) string {
	return placeholder.ReplaceAllStringFunc(s, func(m string) string {
		return sampleLabels[placeholder.FindStringSubmatch(m)[1]]
	})
}

// promtoolFile is the promtool unit-test file for the rules: one test per alert.
func promtoolFile(rules []rule) map[string]any {
	tests := make([]any, 0, len(rules))
	for _, r := range rules {
		ev := events[r.Alert]
		input := make([]any, 0, len(ev.series))
		for _, s := range ev.series {
			input = append(input, map[string]any{"series": s, "values": "0+1x10"})
		}
		annotations := map[string]string{}
		for k, v := range r.Annotations {
			annotations[k] = expand(v)
		}
		tests = append(tests, map[string]any{
			"interval":     "1m",
			"input_series": input,
			"alert_rule_test": []any{map[string]any{
				"eval_time": ev.eval,
				"alertname": r.Alert,
				"exp_alerts": []any{map[string]any{
					"exp_labels":      ev.labels,
					"exp_annotations": annotations,
				}},
			}},
		})
	}
	return map[string]any{"rule_files": []string{"alerts.yaml"}, "evaluation_interval": "1m", "tests": tests}
}

// runPromtool runs promtool with args in dir and returns its combined output.
func runPromtool(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "promtool", args...)
	cmd.Dir = dir
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// What the telemetry role renders to: one Prometheus alert rule per signal that needs a human, each
// naming the agent role and repository and linking to the run's trace. Every threshold and the cost
// budget are group_vars. Each alert is fired from a synthetic event with promtool, so a rule that no
// longer matches its event fails here.
var _ = Describe("the telemetry role's alert rules", Ordered, func() {
	var (
		dir   string
		rules []rule
		v     map[string]any
	)

	named := func(alert string) rule {
		GinkgoHelper()
		for _, r := range rules {
			if r.Alert == alert {
				return r
			}
		}
		Fail("no alert " + alert)
		return rule{}
	}

	// str is a threshold as it appears in an expression.
	str := func(key string) string {
		GinkgoHelper()
		switch x := v[key].(type) {
		case string:
			return x
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64)
		default:
			Fail("threshold " + key + " is neither a string nor a number")
			return ""
		}
	}

	BeforeAll(func() {
		dir = roletest.Render("alerts.yml", roletest.Options{}).Dir
		var doc struct {
			Groups []struct {
				Rules []rule `yaml:"rules"`
			} `yaml:"groups"`
		}
		decodeYAML(readFile(dir, "alerts.yaml"), &doc)
		for _, g := range doc.Groups {
			rules = append(rules, g.Rules...)
		}
		Expect(json.Unmarshal([]byte(readFile(dir, "vars.json")), &v)).To(Succeed())
	})

	Describe("every signal has a rule, and it loads", func() {
		It("the rules load", func() {
			out, err := runPromtool(dir, "check", "rules", "alerts.yaml")
			Expect(err).NotTo(HaveOccurred(), out)
		})

		It("every signal that needs a human has an alert", func() {
			var got []string
			for _, r := range rules {
				got = append(got, r.Alert)
			}
			Expect(got).To(ConsistOf(slices.Sorted(maps.Keys(events))))
		})
	})

	Describe("every rule selects on a label the emitted series carry", func() {
		selectors := regexp.MustCompile(`\{([^{}]*)\}`)
		labelNames := regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*(?:=~|!~|!=|=)`)

		It("each alert selects only labels foreman emits", func() {
			emitted, err := emittedLabels(telemetrySource)
			Expect(err).NotTo(HaveOccurred())
			Expect(emitted).NotTo(BeEmpty())
			for _, r := range rules {
				for _, sel := range selectors.FindAllStringSubmatch(r.Expr, -1) {
					for _, l := range labelNames.FindAllStringSubmatch(sel[1], -1) {
						Expect(emitted).To(HaveKey(l[1]), r.Alert)
					}
				}
			}
		})

		It("each alert fires from its synthetic event", func() {
			test, err := json.MarshalIndent(promtoolFile(rules), "", "  ")
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(dir, "promtool.json"), test, 0o600)).To(Succeed())
			out, err := runPromtool(dir, "test", "rules", "promtool.json")
			Expect(err).NotTo(HaveOccurred(), out)
		})
	})

	Describe("every alert names the run and links to it", func() {
		It("each alert names the agent role and repository", func() {
			for _, r := range rules {
				all := strings.Join(slices.Collect(maps.Values(r.Annotations)), " ")
				Expect(all).To(ContainSubstring("{{ $labels.factory_agent_role }}"), r.Alert)
				Expect(all).To(ContainSubstring("{{ $labels.factory_repo }}"), r.Alert)
			}
		})

		It("each per-issue alert names the issue it was raised on", func() {
			for _, name := range []string{
				"ForemanEgressNotRoutable", "ForemanProxyBadToken", "ForemanOutputFactoryMarker", "ForemanDeadRunReaped",
			} {
				r := named(name)
				Expect(r.Annotations["summary"]).To(ContainSubstring("#{{ $labels.factory_issue }}"), name)
				Expect(r.Annotations["issue"]).To(HaveSuffix("/issues/{{ $labels.factory_issue }}"), name)
			}
		})

		It("each alert links to the role and repository's traces", func() {
			for _, r := range rules {
				trace := r.Annotations["trace"]
				Expect(trace).To(HavePrefix(str("factory_alerts_grafana_url")+"/explore?left="), r.Alert)
				Expect(trace).To(ContainSubstring("span.factory.agent_role"), r.Alert)
				Expect(trace).To(ContainSubstring("span.factory.repo"), r.Alert)
			}
		})

		It("no alert reads a trace id label, because no metric carries one", func() {
			for _, r := range rules {
				Expect(r.Expr).NotTo(ContainSubstring("factory_trace_id"), r.Alert)
				for _, a := range r.Annotations {
					Expect(a).NotTo(ContainSubstring("factory_trace_id"), r.Alert)
				}
			}
		})

		It("each aggregate alert links to the repository's failed issues", func() {
			for _, name := range []string{"ForemanAgentRoleFailureRate", "ForemanDailyModelCost"} {
				Expect(named(name).Annotations["issue"]).To(Equal(
					"https://github.com/{{ $labels.factory_repo }}/issues?q=label%3Afactory%3Afailed"), name)
			}
		})
	})

	Describe("thresholds and the budget are settings, not constants", func() {
		It("the security alerts use the configured security window", func() {
			for _, name := range []string{"ForemanEgressNotRoutable", "ForemanProxyBadToken", "ForemanOutputFactoryMarker"} {
				Expect(named(name).Expr).To(ContainSubstring("["+str("factory_alerts_security_window")+"]"), name)
			}
		})

		It("the failure-rate alert uses the configured threshold and window", func() {
			expr := named("ForemanAgentRoleFailureRate").Expr
			Expect(expr).To(ContainSubstring(str("factory_alerts_failure_rate_window")))
			Expect(expr).To(ContainSubstring("> " + str("factory_alerts_failure_rate_threshold")))
			Expect(expr).To(ContainSubstring(str("factory_alerts_failure_rate_min_runs")))
		})

		It("the cost alert uses the configured budget and window", func() {
			expr := named("ForemanDailyModelCost").Expr
			Expect(expr).To(ContainSubstring(str("factory_alerts_cost_window")))
			Expect(expr).To(ContainSubstring("> " + str("factory_alerts_daily_cost_budget_usd")))
		})

		It("the dead-run alert uses the configured window", func() {
			Expect(named("ForemanDeadRunReaped").Expr).To(ContainSubstring("[" + str("factory_alerts_dead_run_window") + "]"))
		})
	})

	It("prometheus loads the rules", func() {
		Expect(readFile("..", "templates", "prometheus.yaml.j2")).To(ContainSubstring("rule_files:"))
	})
})
