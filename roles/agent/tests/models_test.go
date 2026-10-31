package tests_test

import (
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.yaml.in/yaml/v3"
)

// The expected values come from the spec (issue #33), not from group_vars, so a wrong group_vars
// value fails here.

// skillsFor is the spec's "Skills and permissions" table.
var skillsFor = map[string][]string{
	"planner":     {"grilling", "domain-modeling", "wayfinder", "to-spec", "to-tickets", "writing-for-agents", "retro"},
	"researcher":  {"research", "retro"},
	"implementer": {"implement", "tdd", "code-review", "diagnosing-bugs", "domain-modeling", "pr", "retro", "writing-for-agents"},
	"reviewer":    {"code-review", "retro"},
	"curator":     {"curate"},
}

// limitsFor is the spec's "Step caps and deadlines" table: steps, then minutes.
var limitsFor = map[string][2]int{
	"planner":     {40, 20},
	"researcher":  {80, 45},
	"implementer": {200, 90},
	"reviewer":    {200, 30},
	"curator":     {60, 30},
}

var outputFile = map[string]string{
	"planner":     "planner-output.json",
	"researcher":  "researcher-output.json",
	"implementer": "implementer-output.json",
	"reviewer":    "reviewer-output.json",
	"curator":     "curator-output.json",
}

const (
	retroSteps   = 20
	retroMinutes = 15
	// The dead-run margin, in minutes, past an agent role's deadline (the spec's "Other limits").
	deadRunMargin = 10
	// The review round limit, CI failures included, and the CI pending deadline in minutes.
	reviewRoundLimit         = 3
	ciPendingDeadlineMinutes = 60
)

var readOnlyGH = []string{
	"gh api --method GET", "gh api -X GET", "gh auth status", "gh issue list", "gh issue view",
	"gh label list", "gh pr checks", "gh pr diff", "gh pr list", "gh pr status", "gh pr view",
	"gh release list", "gh release view", "gh repo list", "gh repo view", "gh run list", "gh run view",
	"gh workflow list", "gh workflow view",
}

// writeCommands must resolve to deny for every role: gh writes, and the api forms that mutate.
var writeCommands = []string{
	"gh pr comment 5 --body fixed",
	"gh pr review 5 --approve",
	"gh pr merge 5 --squash",
	"gh pr create --title x --body y",
	"gh issue comment 5 --body hi",
	"gh api -f body=hi repos/o/r/issues/5/comments",
	"gh api repos/o/r/issues/5 -f body=hi",
	"gh api --field body=hi repos/o/r/issues/5/comments",
	"gh api --raw-field body=hi repos/o/r/issues/5/comments",
	"gh api --input payload.json repos/o/r/issues/5/comments",
	"gh api --method POST repos/o/r/issues/5/comments",
	"gh api -X POST repos/o/r/issues/5/comments",
	"git push origin HEAD",
}

var readCommands = []string{
	"git status", "git log --oneline", "gh pr view 5", "gh issue view 5", "gh pr list", "gh pr diff 5", "go test ./...",
}

// rule is one pattern and the action it gives, in the order the config lists them.
type rule struct{ pattern, action string }

// rules reads a permission value: a bare action is the catch-all, a mapping is an ordered rule list.
func rules(n *yaml.Node) []rule {
	GinkgoHelper()
	if n.Kind == yaml.ScalarNode {
		return []rule{{"*", n.Value}}
	}
	Expect(n.Kind).To(Equal(yaml.MappingNode))
	var out []rule
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, rule{n.Content[i].Value, n.Content[i+1].Value})
	}
	return out
}

// allowed is the patterns a rule allows, ignoring its catch-all.
func allowed(rs []rule) []string {
	var out []string
	for _, r := range rs {
		if r.action == "allow" && r.pattern != "*" {
			out = append(out, r.pattern)
		}
	}
	return out
}

func catchAll(rs []rule) string {
	for _, r := range rs {
		if r.pattern == "*" {
			return r.action
		}
	}
	return ""
}

// wildcard matches a command the way opencode does: * is any run of characters, ? is one.
func wildcard(pattern, command string) bool {
	re := "^" + strings.NewReplacer(`\*`, `.*`, `\?`, `.`).Replace(regexp.QuoteMeta(pattern)) + "$"
	return regexp.MustCompile("(?s)" + re).MatchString(command)
}

// resolve gives the action opencode applies to a command: the last matching rule wins.
func resolve(rs []rule, command string) string {
	action := ""
	for _, r := range rs {
		if wildcard(r.pattern, command) {
			action = r.action
		}
	}
	return action
}

func endsWithAny(patterns []string, suffix string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool { return strings.HasSuffix(p, suffix) })
}

// actions lists every action in a permission node, nested rules included.
func actions(n *yaml.Node) []string {
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}
	var out []string
	for i := 1; i < len(n.Content); i += 2 {
		out = append(out, actions(n.Content[i])...)
	}
	return out
}

var _ = Describe("opencode.json and config.json agree with factory_models", Ordered, func() {
	var (
		oc      tree
		cfg     tree
		ocNode  *yaml.Node
		permFor func(role string) *yaml.Node
	)

	BeforeAll(func() {
		out := render(nil)
		oc = parse(filepath.Join(out, "opencode.json"))
		cfg = parse(filepath.Join(out, "config.json"))
		ocNode = parseNode(filepath.Join(out, "opencode.json"))
		permFor = func(role string) *yaml.Node { return nodeAt(ocNode, "agent", role, "permission") }
	})

	It("names the same agent roles in opencode and foreman", func() {
		Expect(treeAt(oc, "agent")).To(HaveLen(len(agentRoles)))
		for _, role := range agentRoles {
			Expect(oc["agent"]).To(HaveKey(role))
			Expect(cfg["agents"]).To(HaveKey(role))
		}
		Expect(treeAt(cfg, "agents")).To(HaveLen(len(agentRoles)), "an agent role is missing")
	})

	It("gives each role its own capped model alias", func() {
		aliases := treeAt(oc, "provider", "openrouter", "models")
		for _, role := range agentRoles {
			alias := strings.TrimPrefix(at(oc, "agent", role, "model").(string), "openrouter/")
			model := treeAt(aliases, alias)
			Expect(model["id"]).To(Equal(at(cfg, "agents", role, "model")), role+": model differs")
			Expect(at(model, "limit", "input")).To(Equal(200000), role+": cap")
			Expect(at(cfg, "agents", role, "context_cap")).To(Equal(200000), role+": cap")
		}
	})

	It("keeps the reviewer and implementer in different model families", func() {
		family := func(role string) string {
			return strings.SplitN(at(cfg, "agents", role, "model").(string), "/", 2)[0]
		}
		Expect(family("reviewer")).NotTo(Equal(family("implementer")), "reviewer and implementer share a model family")
	})

	It("turns compaction on for the implementer and researcher only", func() {
		var on []string
		for _, role := range agentRoles {
			if at(cfg, "agents", role, "compaction") == true {
				on = append(on, role)
			}
		}
		sort.Strings(on)
		Expect(on).To(Equal([]string{"implementer", "researcher"}), "compaction on for %v", on)
	})

	It("sets no global model, which an agent role could fall back to", func() {
		Expect(oc).NotTo(HaveKey("model"))
	})

	for _, role := range agentRoles {
		Describe(role, func() {
			It("denies by default, allows the read tools and denies the ask-only tools", func() {
				perm := treeAt(oc, "agent", role, "permission")
				Expect(perm["*"]).To(Equal("deny"), role+": does not deny by default")
				for _, tool := range []string{"read", "glob", "grep", "list", "webfetch", "websearch"} {
					Expect(perm[tool]).To(Equal("allow"), role+": "+tool+" is not allowed")
				}
				for _, tool := range []string{"question", "task", "doom_loop"} {
					Expect(perm[tool]).To(Equal("deny"), role+": "+tool+" is not denied")
				}
				// foreman's per-run inline config is the only place that names the run's own scratch
				// directory, so no work-root wildcard may appear here.
				Expect(at(oc, "permission", "external_directory")).To(Equal(tree{"*": "deny"}),
					role+": external directories are not denied by default")
			})

			It("loads exactly its skills", func() {
				skills := rules(nodeAt(permFor(role), "skill"))
				Expect(catchAll(skills)).To(Equal("deny"), role+": skills are not denied by default")
				Expect(allowed(skills)).To(ConsistOf(skillsFor[role]), role+": skills differ")
			})

			It("edits exactly what the spec allows", func() {
				edit := nodeAt(permFor(role), "edit")
				if role == "implementer" || role == "reviewer" {
					Expect(edit.Value).To(Equal("allow"), role+": the clone is not editable")
					return
				}
				rs := rules(edit)
				Expect(catchAll(rs)).To(Equal("deny"), role+": edit does not deny by default")
				patterns := allowed(rs)
				if role == "researcher" {
					Expect(patterns).To(ContainElement("docs/research/**"), "researcher: docs/research is not editable")
				}
				if role == "planner" || role == "researcher" || role == "curator" {
					Expect(endsWithAny(patterns, outputFile[role])).To(BeTrue(), role+": no output file")
				}
				var output []string
				for _, p := range patterns {
					if strings.HasSuffix(p, ".json") {
						output = append(output, p)
					}
				}
				Expect(sort.StringsAreSorted(output)).To(BeTrue(), role+": extra output")
				for _, p := range output {
					Expect(p).To(ContainSubstring(outputFile[role]), role+": extra output")
				}
			})

			It("runs shell commands the spec allows and writes nothing through gh or git", func() {
				bash := rules(nodeAt(permFor(role), "bash"))
				if role == "implementer" || role == "reviewer" {
					Expect(catchAll(bash)).To(Equal("allow"), role+": bash is not allowed")
				} else {
					Expect(catchAll(bash)).To(Equal("deny"), role+": bash is not read-only")
					if role == "planner" || role == "curator" {
						Expect(allowed(bash)).NotTo(BeEmpty(), role+": no read-only commands")
					}
				}
				for _, r := range bash {
					if strings.HasPrefix(r.pattern, "gh ") && r.action == "allow" {
						key := strings.TrimRight(strings.TrimSuffix(r.pattern, "*"), " ")
						Expect(readOnlyGH).To(ContainElement(key), role+": gh write allowed: "+r.pattern)
					}
				}
				for _, command := range writeCommands {
					Expect(resolve(bash, command)).To(Equal("deny"), role+": write allowed: "+command)
				}
				for _, command := range readCommands {
					effect := resolve(bash, command)
					if role == "implementer" || role == "reviewer" {
						Expect(effect).NotTo(Equal("deny"), role+": a read is denied: "+command)
					}
					readOnlyGh := strings.HasPrefix(command, "gh ") &&
						(strings.Contains(command, " view") || strings.Contains(command, " list"))
					if (role == "planner" || role == "curator") && readOnlyGh {
						Expect(effect).To(Equal("allow"), role+": a read-only gh command is denied: "+command)
					}
				}
			})

			It("carries the step cap in opencode and both limits in foreman", func() {
				steps, minutes := limitsFor[role][0], limitsFor[role][1]
				Expect(at(oc, "agent", role, "steps")).To(Equal(steps), role+": opencode step cap")
				Expect(at(cfg, "agents", role, "steps")).To(Equal(steps), role+": foreman step cap")
				Expect(at(cfg, "agents", role, "deadline_minutes")).To(Equal(minutes), role+": foreman deadline")
			})

			It("asks for nothing, since an unattended run can never answer a prompt", func() {
				Expect(actions(permFor(role))).NotTo(ContainElement("ask"), role+": a permission asks")
			})

			It("has its retro budget and a list of allowed egress domains", func() {
				Expect(at(cfg, "agents", role, "retro")).To(Equal(tree{"steps": retroSteps, "deadline_minutes": retroMinutes}),
					role+": retro budget")
				_, ok := at(cfg, "agents", role, "egress", "allowed_domains").([]any)
				Expect(ok).To(BeTrue(), role+": egress is not a list")
			})
		})
	}

	It("keeps a retro budget only in the agent roles", func() {
		Expect(cfg).NotTo(HaveKey("retro"))
		Expect(cfg).NotTo(HaveKey("retro_by_role"))
	})
	It("sets the dead-run margin, review round limit and CI pending deadline", func() {
		Expect(cfg["dead_run_margin_minutes"]).To(Equal(deadRunMargin), "dead-run margin")
		Expect(cfg["review_round_limit"]).To(Equal(reviewRoundLimit), "review round limit")
		Expect(cfg["ci_pending_deadline_minutes"]).To(Equal(ciPendingDeadlineMinutes), "CI pending deadline")
	})
})
