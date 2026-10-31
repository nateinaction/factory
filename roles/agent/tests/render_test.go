package tests_test

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var agentRoles = []string{"planner", "researcher", "implementer", "reviewer", "curator"}

const otlpEndpoint = "http://127.0.0.1:4318"

// envOf reads a DAG's env list, a list of single-entry maps, into one map.
func envOf(dag tree) map[string]string {
	GinkgoHelper()
	out := map[string]string{}
	for _, entry := range listAt(dag, "env") {
		m, ok := entry.(tree)
		Expect(ok).To(BeTrue(), "env entry %v is not a map", entry)
		for k, v := range m {
			s, ok := v.(string)
			Expect(ok).To(BeTrue(), "env %s is not a string", k)
			out[k] = s
		}
	}
	return out
}

// paramNames lists the names a DAG declares under params.
func paramNames(dag tree) []string {
	GinkgoHelper()
	var names []string
	for _, p := range listAt(dag, "params") {
		m, ok := p.(tree)
		Expect(ok).To(BeTrue())
		names = append(names, m["name"].(string))
	}
	return names
}

func firstRun(dag tree) string {
	GinkgoHelper()
	step, ok := listAt(dag, "steps")[0].(tree)
	Expect(ok).To(BeTrue())
	return step["run"].(string)
}

// roleKeys is the set of agent roles named in a worker's labels, after the runner label.
func roleKeys(labels string) []string {
	GinkgoHelper()
	parts := strings.Split(labels, ",")
	Expect(parts).NotTo(BeEmpty())
	var keys []string
	for _, l := range parts[1:] {
		keys = append(keys, strings.SplitN(l, "=", 2)[0])
	}
	sort.Strings(keys)
	return keys
}

func capacities(t tree, keys ...string) map[string]int {
	GinkgoHelper()
	out := map[string]int{}
	for k, v := range treeAt(t, keys...) {
		n, ok := v.(int)
		Expect(ok).To(BeTrue(), "capacity of %s is %v", k, v)
		out[k] = n
	}
	return out
}

func sortedStrings(l []any) []string {
	GinkgoHelper()
	s := strs(l)
	sort.Strings(s)
	return s
}

// sudoersLines are the lines of a sudoers file that are not comments.
func sudoersLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			out = append(out, t)
		}
	}
	return out
}

var _ = Describe("the agent role's templates", Ordered, func() {
	var out string

	BeforeAll(func() { out = render(nil) })

	for _, role := range agentRoles {
		Describe(role, func() {
			var dag tree
			BeforeAll(func() { dag = parse(filepath.Join(out, "factory-"+role+".yaml")) })

			It(role+" DAG calls foreman start with its role", func() {
				Expect(firstRun(dag)).To(Equal("/usr/local/bin/foreman start " + role + ` "${params.REPO}" "${params.ISSUE}" "${params.TRACEPARENT}"`))
			})
			It(role+" DAG declares REPO", func() { Expect(paramNames(dag)).To(ContainElement("REPO")) })
			It(role+" DAG declares ISSUE", func() { Expect(paramNames(dag)).To(ContainElement("ISSUE")) })
			It(role+" DAG declares TRACEPARENT", func() { Expect(paramNames(dag)).To(ContainElement("TRACEPARENT")) })
			It(role+" DAG has its own queue", func() { Expect(dag["queue"]).To(Equal("factory-" + role)) })
			It(role+" DAG selects a host that takes the role", func() {
				Expect(treeAt(dag, "worker_selector")).To(HaveKeyWithValue(role, "true"))
			})
			It(role+" queue rendered for dagu", func() {
				var names []string
				for _, q := range listAt(parse(filepath.Join(out, "dagu-config.yaml")), "queues", "config") {
					names = append(names, q.(tree)["name"].(string))
				}
				Expect(names).To(ContainElement("factory-" + role))
			})
		})
	}

	It("the poll DAG runs foreman poll", func() {
		Expect(firstRun(parse(filepath.Join(out, "factory-poll.yaml")))).To(Equal("/usr/local/bin/foreman poll"))
	})
	It("the poll DAG runs one poll at a time", func() {
		Expect(parse(filepath.Join(out, "factory-poll.yaml"))["max_active_runs"]).To(Equal(1))
	})

	It("an unset host role list takes every agent role", func() {
		labels := parse(filepath.Join(out, "scheduling.yml"))["worker_labels"].(string)
		Expect(roleKeys(labels)).To(ConsistOf(agentRoles))
	})
	It("no agent role is unhosted when every worker takes all", func() {
		Expect(listAt(parse(filepath.Join(out, "scheduling.yml")), "unhosted_roles")).To(BeEmpty())
	})
	It("the per-host run cap defaults to one", func() {
		sched := parse(filepath.Join(out, "scheduling.yml"))
		Expect(sched["run_cap"]).To(Equal(1))
		Expect(sched["queue_capacity"]).To(Equal(1))
	})
	It("a worker runs at most the run cap at once", func() {
		Expect(readText(filepath.Join(out, "dagu-worker.service"))).To(ContainSubstring("--worker.max-active-runs 1"))
	})
	It("a worker labelled every agent role", func() {
		Expect(readText(filepath.Join(out, "dagu-worker.service"))).To(ContainSubstring(
			"--worker.labels runner=agent,planner=true,researcher=true,implementer=true,reviewer=true,curator=true"))
	})
	It("dagu exports foreman's OTLP endpoint to its steps", func() {
		lines := strings.Split(readText(filepath.Join(out, "dagu-worker.service")), "\n")
		Expect(lines).To(ContainElement("Environment=OTEL_EXPORTER_OTLP_ENDPOINT=" + otlpEndpoint))
		Expect(lines).To(ContainElement("Environment=OTEL_SERVICE_NAME=foreman"))
	})

	// Dagu hands a step only its own allowlisted environment, not the service unit's, so each DAG names the
	// export itself or foreman's spans and metrics never leave the process.
	for _, dag := range []string{"factory-poll", "factory-planner", "factory-researcher", "factory-implementer", "factory-reviewer", "factory-curator"} {
		It(dag+" exports foreman's OTLP endpoint to its steps", func() {
			env := envOf(parse(filepath.Join(out, dag+".yaml")))
			Expect(env).To(HaveKeyWithValue("OTEL_EXPORTER_OTLP_ENDPOINT", otlpEndpoint))
			Expect(env).To(HaveKeyWithValue("OTEL_SERVICE_NAME", "foreman"))
		})
	}

	allRoles := map[string]int{"planner": 1, "researcher": 1, "implementer": 1, "reviewer": 1, "curator": 1}
	It("foreman's config names the unhosted roles", func() {
		Expect(parse(filepath.Join(out, "config.json"))).To(HaveKey("unhosted_roles"))
	})
	It("each agent role's run capacity is the run cap times its hosts", func() {
		Expect(capacities(parse(filepath.Join(out, "scheduling.yml")), "role_capacity")).To(Equal(allRoles))
	})
	It("foreman's config gives each agent role its run capacity", func() {
		Expect(capacities(parse(filepath.Join(out, "config.json")), "role_capacity")).To(Equal(allRoles))
	})
	It("the server's config points status at Tempo", func() {
		Expect(parse(filepath.Join(out, "config.json"))["telemetry_url"]).To(Equal("http://127.0.0.1:3200"))
	})
	It("a worker's config reads no remote backend", func() {
		Expect(parse(filepath.Join(out, "config-worker.json"))["telemetry_url"]).To(Equal(""))
	})
	It("no old name in any rendered file", func() {
		for _, f := range files(out) {
			Expect(readText(f)).NotTo(ContainSubstring("finances-agent"), f)
		}
	})

	Describe("sudoers", func() {
		var path string
		BeforeAll(func() { path = filepath.Join(out, "dagu-agent.sudoers") })

		It("sudoers permits foreman run *", func() {
			Expect(readText(path)).To(ContainSubstring("/usr/local/bin/foreman run *"))
		})
		It("sudoers has exactly one permitted command", func() {
			var commands []string
			for _, l := range sudoersLines(readText(path)) {
				if _, cmd, ok := strings.Cut(l, "NOPASSWD:"); ok {
					commands = append(commands, strings.TrimSpace(cmd))
				}
			}
			Expect(commands).To(Equal([]string{"/usr/local/bin/foreman run *"}))
		})
		// The agent never inherits the factory's configuration through sudo: no FACTORY_* variable is kept.
		It("sudoers keeps no FACTORY_ variable", func() {
			Expect(strings.Join(sudoersLines(readText(path)), "\n")).NotTo(ContainSubstring("FACTORY_"))
		})
		// The check above is not vacuous: env_keep is present, and names the run id, the trace context and the
		// telemetry export variables only.
		It("sudoers keeps only the run id, trace context and OTEL export variables", func() {
			keep := regexp.MustCompile(`env_keep\s*\+?=\s*(.*)`)
			var kept []string
			for _, l := range sudoersLines(readText(path)) {
				if m := keep.FindStringSubmatch(l); m != nil {
					kept = append(kept, strings.Fields(strings.ReplaceAll(m[1], `"`, ""))...)
				}
			}
			slices.Sort(kept)
			Expect(slices.Compact(kept)).To(Equal([]string{"DAG_RUN_ID", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_SERVICE_NAME", "TRACEPARENT"}))
		})
		It("sudoers passes visudo", func() {
			visudo, err := exec.LookPath("visudo")
			if err != nil {
				Skip("visudo is not installed")
			}
			out, err := exec.Command(visudo, "-cf", path).CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), string(out))
		})
	})
})

// A host that lists its roles takes only those: the labels select only them, and every other role is
// reported as unhosted so the poll warns and leaves its issues queued.
var _ = Describe("the agent role's templates on a host that lists only the reviewer role", Ordered, func() {
	var out string
	unhosted := []string{"planner", "researcher", "implementer", "curator"}
	hosted := map[string]int{"planner": 0, "researcher": 0, "implementer": 0, "reviewer": 2, "curator": 0}

	BeforeAll(func() {
		out = render(map[string]any{"factory_worker_agent_roles": []string{"reviewer"}, "factory_agent_run_cap": 2})
	})

	It("a host takes only the roles it lists", func() {
		labels := parse(filepath.Join(out, "scheduling.yml"))["worker_labels"].(string)
		Expect(roleKeys(labels)).To(Equal([]string{"reviewer"}))
	})
	It("an agent role no host takes is named", func() {
		Expect(sortedStrings(listAt(parse(filepath.Join(out, "scheduling.yml")), "unhosted_roles"))).To(ConsistOf(unhosted))
	})
	It("foreman's config names the roles no host takes", func() {
		Expect(sortedStrings(listAt(parse(filepath.Join(out, "config.json")), "unhosted_roles"))).To(ConsistOf(unhosted))
	})
	It("a raised run cap lets a worker run two at once", func() {
		Expect(readText(filepath.Join(out, "dagu-worker.service"))).To(ContainSubstring("--worker.max-active-runs 2"))
	})
	It("a raised run cap raises the queue capacity", func() {
		sched := parse(filepath.Join(out, "scheduling.yml"))
		Expect(sched["run_cap"]).To(Equal(2))
		Expect(sched["queue_capacity"]).To(Equal(2))
	})
	It("a raised run cap raises only the hosted role's capacity", func() {
		Expect(capacities(parse(filepath.Join(out, "scheduling.yml")), "role_capacity")).To(Equal(hosted))
	})
	It("foreman's config gives the hosted role its raised capacity", func() {
		Expect(capacities(parse(filepath.Join(out, "config.json")), "role_capacity")).To(Equal(hosted))
	})
})
