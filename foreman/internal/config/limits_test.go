package config_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

var _ = Describe("the review and CI limits", func() {
	agent := config.AgentConfig{
		Model: "prov/model", ContextCap: 200_000, Compaction: true, Steps: 200, DeadlineMinutes: 90,
	}

	// rawConfig renders a config with every agent role, overrides and any extra fields.
	rawConfig := func(overrides map[string]any, extra map[string]any) []byte {
		agents := map[string]config.AgentConfig{}
		for _, r := range workflow.KnownRoles() {
			agents[string(r)] = agent
		}

		fields := map[string]any{
			"roles": []workflow.Role{workflow.Implementer}, "agents": agents,
			"retro":                       config.Budget{Steps: 20, DeadlineMinutes: 15},
			"dead_run_margin_minutes":     10,
			"review_round_limit":          3,
			"ci_pending_deadline_minutes": 60,
		}
		maps.Copy(fields, overrides)

		maps.Copy(fields, extra)

		raw, err := json.Marshal(fields)
		Expect(err).NotTo(HaveOccurred())

		return raw
	}

	// load writes raw as a config file and loads it.
	load := func(raw []byte) (config.Config, error) {
		path := filepath.Join(GinkgoT().TempDir(), "c.json")
		Expect(os.WriteFile(path, raw, 0o600)).To(Succeed())

		return config.Load(path)
	}

	It("uses the shared retro budget unless an agent role has its own", func() {
		cfg, err := load(rawConfig(map[string]any{
			"retro_by_role": map[string]config.Budget{
				"reviewer": {Steps: 12, DeadlineMinutes: 7},
			},
		}, nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.RetroBudget(workflow.Reviewer)).To(Equal(config.Budget{Steps: 12, DeadlineMinutes: 7}))
		Expect(cfg.RetroBudget(workflow.Planner)).To(Equal(config.Budget{Steps: 20, DeadlineMinutes: 15}))
	})

	DescribeTable("rejects a retro override that cannot bound anything",
		func(overrides map[string]any, message string) {
			_, err := load(rawConfig(overrides, nil))
			Expect(err).To(MatchError(ContainSubstring(message)))
		},
		Entry("an unknown agent role", map[string]any{
			"retro_by_role": map[string]config.Budget{"janitor": {Steps: 20, DeadlineMinutes: 15}},
		}, "janitor"),
		Entry("no step cap", map[string]any{
			"retro_by_role": map[string]config.Budget{"planner": {DeadlineMinutes: 15}},
		}, "retro_by_role planner"),
		Entry("no deadline", map[string]any{
			"retro_by_role": map[string]config.Budget{"planner": {Steps: 20}},
		}, "retro_by_role planner"),
	)

	It("reads the review round limit and the CI pending deadline", func() {
		cfg, err := load(rawConfig(nil, nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.ReviewRoundLimit).To(Equal(3))
		Expect(cfg.CIPendingDeadline()).To(Equal(60 * time.Minute))
	})

	DescribeTable("rejects a limit that cannot bound anything",
		func(overrides map[string]any, message string) {
			_, err := load(rawConfig(overrides, nil))
			Expect(err).To(MatchError(ContainSubstring(message)))
		},
		Entry("a missing round limit", map[string]any{"review_round_limit": nil}, "review round limit"),
		Entry("no round limit", map[string]any{"review_round_limit": 0}, "review round limit"),
		Entry("a negative round limit", map[string]any{"review_round_limit": -1}, "review round limit"),
		Entry("a missing CI deadline", map[string]any{"ci_pending_deadline_minutes": nil}, "CI pending deadline"),
		Entry("no CI deadline", map[string]any{"ci_pending_deadline_minutes": 0}, "CI pending deadline"),
		Entry("a negative CI deadline", map[string]any{"ci_pending_deadline_minutes": -1}, "CI pending deadline"),
	)
})
