package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("DefaultSettings", func() {
	It("keeps the host's config locations", func() {
		s := config.DefaultSettings("/home/factory", "")
		Expect(s.ConfigFile).To(Equal("/etc/factory-agent/config.json"))
		Expect(s.KeyFile).To(Equal("/home/factory/.config/factory-agent/openrouter-key"))
		Expect(s.WorkRoot).To(Equal("/home/factory/work"))
		Expect(s.TokenPath(workflow.Reviewer)).To(Equal("/run/factory/gh-token-reviewer"))
		Expect(s.API).To(Equal("https://api.github.com"))
	})

	It("takes a config file override", func() {
		Expect(config.DefaultSettings("/h", "/x.json").ConfigFile).To(Equal("/x.json"))
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
	agent := config.AgentConfig{Model: "prov/model", ContextCap: 200_000, Compaction: true}

	// every gives each of the five agent roles a model and a cap.
	every := func() map[string]config.AgentConfig {
		m := map[string]config.AgentConfig{}
		for _, r := range workflow.KnownRoles() {
			m[string(r)] = agent
		}

		return m
	}

	// load writes a config with the given agents and loads it.
	load := func(agents map[string]config.AgentConfig) error {
		raw, err := json.Marshal(map[string]any{"roles": []workflow.Role{workflow.Implementer}, "agents": agents})
		Expect(err).NotTo(HaveOccurred())

		path := filepath.Join(GinkgoT().TempDir(), "c.json")
		Expect(os.WriteFile(path, raw, 0o600)).To(Succeed())
		_, err = config.Load(path)

		return err
	}

	It("accepts every agent role with a model and a cap", func() {
		Expect(load(every())).To(Succeed())
	})

	DescribeTable("requires a model and a cap for every agent role",
		func(mutate func(map[string]config.AgentConfig)) {
			m := every()
			mutate(m)
			Expect(load(m)).NotTo(Succeed())
		},
		Entry("missing role", func(m map[string]config.AgentConfig) { delete(m, "curator") }),
		Entry("no model", func(m map[string]config.AgentConfig) { m["planner"] = config.AgentConfig{ContextCap: 1} }),
		Entry("no cap", func(m map[string]config.AgentConfig) { m["planner"] = config.AgentConfig{Model: "m"} }),
	)
})
