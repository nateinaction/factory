package tests_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

// The runs dashboard: model cost, run outcomes, claims and reaped runs by agent role, over the chosen
// time range, so a counter that moves only when a run finishes still shows something between runs.
var _ = Describe("the runs dashboard", Ordered, func() {
	var (
		dash   any
		panels []any
	)

	// exprs are the queries of the panel with the title.
	exprs := func(title string) []string {
		GinkgoHelper()
		for _, p := range panels {
			if text(at(p, "title")) == title {
				var out []string
				for _, t := range list(at(p, "targets")) {
					out = append(out, text(at(t, "expr")))
				}
				return out
			}
		}
		Fail("no panel titled " + title)
		return nil
	}

	panel := func(title string) any {
		GinkgoHelper()
		for _, p := range panels {
			if text(at(p, "title")) == title {
				return p
			}
		}
		Fail("no panel titled " + title)
		return nil
	}

	BeforeAll(func() {
		r := roletest.Render("runs.yml", roletest.Options{})
		dash = jsonDoc(r.Dir, "runs.json")
		panels = list(at(dash, "panels"))
	})

	It("the dashboard is valid JSON with its own uid", func() {
		Expect(at(dash, "uid")).To(Equal("factory-runs"))
		Expect(at(dash, "title")).To(Equal("Factory runs"))
	})

	// Each run is its own process, so its counters restart at zero and Prometheus holds one sample per run,
	// that run's whole count. increase() reads each restart as a reset and drops the first run's share;
	// summing the samples in each graph step counts every run exactly once.
	It("model cost is a running total over a trailing window, by role and by model", func() {
		Expect(exprs("Model cost by agent role")).To(Equal([]string{"sum by (factory_agent_role) (sum_over_time(factory_run_cost_total[$cost_window]))"}))
		Expect(exprs("Model cost by model")).To(Equal([]string{"sum by (gen_ai_request_model) (sum_over_time(factory_run_cost_total[$cost_window]))"}))
	})

	It("the cost lines step up at each run", func() {
		for _, p := range panels[:2] {
			Expect(at(p, "type")).To(Equal("timeseries"), text(at(p, "title")))
			custom := at(p, "fieldConfig", "defaults", "custom")
			Expect(at(custom, "drawStyle")).To(Equal("line"))
			Expect(at(custom, "lineInterpolation")).To(Equal("stepAfter"))
		}
	})

	// A running total's legend figure is its latest value. Summing the points adds the total up once per step.
	It("the cost legends show each line's latest value, not a sum of its points", func() {
		for _, p := range panels[:2] {
			Expect(at(p, "options", "legend", "calcs")).To(Equal([]any{"lastNotNull"}))
		}
	})

	It("the cost window is a dashboard variable, defaulting to 30 days", func() {
		var variable any
		for _, t := range list(at(dash, "templating", "list")) {
			if at(t, "name") == "cost_window" {
				variable = t
			}
		}
		Expect(variable).NotTo(BeNil(), "no cost_window variable")
		Expect(at(variable, "query")).To(Equal("30d"))
		Expect(at(variable, "current", "value")).To(Equal("30d"))
	})

	It("finished runs are state timelines of outcome and of role", func() {
		Expect(exprs("Runs by outcome")).To(Equal([]string{"sum by (factory_agent_role, factory_outcome) (sum_over_time(factory_run_outcomes_total[$__interval]))"}))
		Expect(exprs("Runs by agent role")).To(Equal([]string{"sum by (factory_agent_role) (sum_over_time(factory_run_outcomes_total[$__interval]))"}))
	})

	It("claims and reaped runs are state timelines by role", func() {
		Expect(exprs("Issues claimed by agent role")).To(Equal([]string{"sum by (factory_agent_role) (sum_over_time(factory_poll_claims_total[$__interval]))"}))
		Expect(exprs("Dead runs reaped by agent role")).To(Equal([]string{"sum by (factory_agent_role) (sum_over_time(factory_runs_reaped_total[$__interval]))"}))
	})

	It("the four count panels are state timelines", func() {
		var types []any
		for _, p := range panels {
			if !strings.HasPrefix(text(at(p, "title")), "Model cost") {
				types = append(types, at(p, "type"))
			}
		}
		Expect(types).To(Equal([]any{"state-timeline", "state-timeline", "state-timeline", "state-timeline"}))
	})

	It("a failed outcome is drawn red", func() {
		red := map[string]any{"id": "color", "value": map[string]any{"mode": "fixed", "fixedColor": "red"}}
		found := false
		for _, o := range list(at(panel("Runs by outcome"), "fieldConfig", "overrides")) {
			options := at(o, "matcher", "options")
			if strings.Contains(toString(options), "failed") {
				Expect(at(o, "properties")).To(ContainElement(red))
				found = true
			}
		}
		Expect(found).To(BeTrue(), "no override for failed")
	})

	It("no panel uses a window that empties between runs", func() {
		for _, p := range panels {
			for _, q := range exprs(text(at(p, "title"))) {
				for _, bad := range []string{"$__rate_interval", "$__range", "[5m]"} {
					Expect(q).NotTo(ContainSubstring(bad), text(at(p, "title")))
				}
			}
		}
	})

	It("every panel reads the provisioned Prometheus datasource", func() {
		for _, p := range panels {
			Expect(at(p, "datasource", "uid")).To(Equal("prometheus"))
			for _, t := range list(at(p, "targets")) {
				Expect(at(t, "datasource", "uid")).To(Equal("prometheus"))
			}
		}
	})

	It("every panel queries a range, so each run is a point at its own time", func() {
		for _, p := range panels {
			for _, t := range list(at(p, "targets")) {
				Expect(at(t, "range")).To(BeTrue())
				if instant, ok := t.(map[string]any)["instant"]; ok {
					Expect(instant).To(BeFalse())
				}
			}
		}
	})

	It("the dashboard is installed beside the host health one", func() {
		tasks := readFile("..", "tasks", "main.yml")
		Expect(tasks).To(ContainSubstring("dest: /etc/grafana/dashboards/runs.json"))
	})

})

// toString renders a matcher's options for a substring check; they are a string or a list of them.
func toString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []any:
		parts := make([]string, 0, len(s))
		for _, e := range s {
			parts = append(parts, toString(e))
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}
