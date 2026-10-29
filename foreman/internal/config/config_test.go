package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("SettingsViper", func() {
	// settings decodes the settings source for home.
	settings := func(home string) (config.Settings, error) {
		return config.SettingsFrom(config.SettingsViper(home))
	}

	It("keeps the host's config locations as defaults", func() {
		GinkgoT().Setenv("FACTORY_AGENT_CONFIG", "")
		GinkgoT().Setenv("FACTORY_KEY_FILE", "")
		GinkgoT().Setenv("FACTORY_WORK_ROOT", "")
		GinkgoT().Setenv("FACTORY_TOKEN_DIR", "")
		GinkgoT().Setenv("FACTORY_API", "")

		s, err := settings("/home/factory")
		Expect(err).NotTo(HaveOccurred())
		Expect(s.ConfigFile).To(Equal("/etc/factory-agent/config.json"))
		Expect(s.KeyFile).To(Equal("/home/factory/.config/factory-agent/openrouter-key"))
		Expect(s.WorkRoot).To(Equal("/home/factory/work"))
		Expect(s.TokenPath(workflow.Reviewer)).To(Equal("/run/factory/gh-token-reviewer"))
		Expect(s.API).To(Equal("https://api.github.com"))
		Expect(s.Self).To(Equal("/usr/local/bin/foreman"))
		Expect(s.TokenHelper).To(Equal("/usr/local/bin/github-app-token"))
		Expect(s.Dagu).To(Equal("/usr/local/bin/dagu"))
		Expect(s.DAGPrefix).To(Equal("factory-"))
	})

	It("lets FACTORY_* environment variables override the defaults", func() {
		GinkgoT().Setenv("FACTORY_WORK_ROOT", "/srv/work")
		GinkgoT().Setenv("FACTORY_TOKEN_DIR", "/srv/tokens")
		GinkgoT().Setenv("FACTORY_DAG_PREFIX", "other-")

		s, err := settings("/home/factory")
		Expect(err).NotTo(HaveOccurred())
		Expect(s.WorkRoot).To(Equal("/srv/work"))
		Expect(s.TokenDir).To(Equal("/srv/tokens"))
		Expect(s.DAGPrefix).To(Equal("other-"))
	})

	It("still honors FACTORY_AGENT_CONFIG", func() {
		GinkgoT().Setenv("FACTORY_AGENT_CONFIG", "/x.json")

		s, err := settings("/h")
		Expect(err).NotTo(HaveOccurred())
		Expect(s.ConfigFile).To(Equal("/x.json"))
	})
})

var _ = Describe("Settings.TokenPath", func() {
	It("names each run's token file for its agent role", func() {
		s := config.Settings{TokenDir: "/run/factory"}
		Expect(s.TokenPath(workflow.Reviewer)).To(Equal("/run/factory/gh-token-reviewer"))
		Expect(s.TokenPath(workflow.Reviewer)).NotTo(Equal(s.TokenPath(workflow.Implementer)))
	})
})

var _ = Describe("Load", func() {
	agent := config.AgentConfig{
		Model: "prov/model", ContextCap: 200_000, Compaction: true, Steps: 200, DeadlineMinutes: 90,
	}
	retro := config.Budget{Steps: 20, DeadlineMinutes: 15}

	// every gives each of the five agent roles a model, a cap, a step cap and a deadline.
	every := func() map[string]config.AgentConfig {
		m := map[string]config.AgentConfig{}
		for _, r := range workflow.KnownRoles() {
			m[string(r)] = agent
		}

		return m
	}

	// writeConfig writes raw JSON as a config file and returns its path.
	writeConfig := func(raw []byte) string {
		GinkgoHelper()

		path := filepath.Join(GinkgoT().TempDir(), "c.json")
		Expect(os.WriteFile(path, raw, 0o600)).To(Succeed())

		return path
	}

	// load writes a config with the given agents and loads it.
	load := func(agents map[string]config.AgentConfig) error {
		raw, err := json.Marshal(map[string]any{
			"roles": []workflow.Role{workflow.Implementer}, "agents": agents, "retro": retro,
			"dead_run_margin_minutes": 10, "review_round_limit": 3, "ci_pending_deadline_minutes": 60,
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = config.Load(writeConfig(raw))

		return err
	}

	It("accepts every agent role with a model and a cap", func() {
		Expect(load(every())).To(Succeed())
	})

	DescribeTable("requires a model, a cap, a step cap and a deadline for every agent role",
		func(mutate func(map[string]config.AgentConfig)) {
			m := every()
			mutate(m)
			Expect(load(m)).NotTo(Succeed())
		},
		Entry("missing role", func(m map[string]config.AgentConfig) { delete(m, "curator") }),
		Entry("no model", func(m map[string]config.AgentConfig) { m["planner"] = config.AgentConfig{ContextCap: 1} }),
		Entry("no cap", func(m map[string]config.AgentConfig) { m["planner"] = config.AgentConfig{Model: "m"} }),
		Entry("no steps", func(m map[string]config.AgentConfig) {
			a := m["planner"]
			a.Steps = 0
			m["planner"] = a
		}),
		Entry("no deadline", func(m map[string]config.AgentConfig) {
			a := m["planner"]
			a.DeadlineMinutes = 0
			m["planner"] = a
		}),
	)

	It("requires a retro step cap and deadline", func() {
		raw, err := json.Marshal(map[string]any{
			"roles": []workflow.Role{workflow.Implementer}, "agents": every(),
			"retro": config.Budget{Steps: 0, DeadlineMinutes: 0}, "dead_run_margin_minutes": 10,
			"review_round_limit": 3, "ci_pending_deadline_minutes": 60,
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = config.Load(writeConfig(raw))
		Expect(err).To(MatchError(ContainSubstring("retro")))
	})

	It("requires a positive dead-run margin", func() {
		raw, err := json.Marshal(map[string]any{
			"roles": []workflow.Role{workflow.Implementer}, "agents": every(),
			"retro": retro, "dead_run_margin_minutes": 0,
			"review_round_limit": 3, "ci_pending_deadline_minutes": 60,
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = config.Load(writeConfig(raw))
		Expect(err).To(MatchError(ContainSubstring("dead-run margin")))
	})

	It("reads the factory repo, and rejects one that is not owner/name", func() {
		load := func(repo string) error {
			raw, err := json.Marshal(map[string]any{
				"roles": []workflow.Role{workflow.Implementer}, "agents": every(), "retro": retro,
				"dead_run_margin_minutes": 10, "review_round_limit": 3, "ci_pending_deadline_minutes": 60,
				"factory_repo": repo,
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = config.Load(writeConfig(raw))

			return err
		}

		Expect(load("nateinaction/factory")).To(Succeed())
		Expect(load("")).To(Succeed(), "an unset factory repo is allowed, and promotes nothing out")
		Expect(load("../nope")).To(MatchError(ContainSubstring("factory repo")))
	})

	It("reads the dead-run margin as a duration", func() {
		raw, err := json.Marshal(map[string]any{
			"roles": []workflow.Role{workflow.Implementer}, "agents": every(),
			"retro": retro, "dead_run_margin_minutes": 10,
			"review_round_limit": 3, "ci_pending_deadline_minutes": 60,
		})
		Expect(err).NotTo(HaveOccurred())

		cfg, err := config.Load(writeConfig(raw))
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.DeadRunMargin()).To(Equal(10 * time.Minute))
	})

	DescribeTable("rejects a malformed config",
		func(raw string) {
			_, err := config.Load(writeConfig([]byte(raw)))
			Expect(err).To(HaveOccurred())
		},
		Entry("empty", ``),
		Entry("not JSON", `[`),
		Entry("no fields", `{}`),
	)

	It("rejects an unknown top-level key", func() {
		_, err := config.Load(writeConfig([]byte(`{"roles":["implementer"],"agents":{},"extra":true}`)))
		Expect(err).To(HaveOccurred())
	})

	It("rejects an unknown agent key", func() {
		_, err := config.Load(writeConfig([]byte(
			`{"roles":["implementer"],"agents":{"janitor":{"model":"m","context_cap":1}}}`,
		)))
		Expect(err).To(HaveOccurred())
	})

	It("rejects an agent setting it does not know", func() {
		_, err := config.Load(writeConfig([]byte(
			`{"roles":["implementer"],"agents":{"implementer":{"model":"m","context_cap":1,"extra":true}}}`,
		)))
		Expect(err).To(HaveOccurred())
	})
})
