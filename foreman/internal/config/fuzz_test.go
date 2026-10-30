package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// validSeed is a config Load accepts, so the fuzzer mutates from a file that passes validation.
func validSeed(f *testing.F) []byte {
	f.Helper()

	agents := map[string]config.AgentConfig{}
	for _, role := range workflow.KnownRoles() {
		agents[string(role)] = config.AgentConfig{
			Model: "m", ContextCap: 1, Steps: 1, DeadlineMinutes: 1,
			Retro: config.Budget{Steps: 1, DeadlineMinutes: 1},
		}
	}

	raw, err := json.Marshal(map[string]any{
		"agents": agents, "dead_run_margin_minutes": 1, "review_round_limit": 1,
		"ci_pending_deadline_minutes": 1,
	})
	if err != nil {
		f.Fatalf("marshal the seed config: %v", err)
	}

	return raw
}

// FuzzLoad checks that Load answers any file content with a config or an error, never a panic, and
// that a config it returns has every known agent role with a model, a cap and a budget.
func FuzzLoad(f *testing.F) {
	f.Add(validSeed(f))

	for _, seed := range []string{"", "{}", "null", "[]", `{"unknown":1}`, "\x00"} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, content []byte) {
		file := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(file, content, 0o600); err != nil {
			t.Fatalf("write fuzz input: %v", err)
		}

		cfg, err := config.Load(file)
		if err != nil {
			return
		}

		for _, role := range workflow.KnownRoles() {
			a := cfg.Agents[string(role)]
			if a.Model == "" || a.ContextCap <= 0 || a.Steps <= 0 || a.DeadlineMinutes <= 0 {
				t.Fatalf("Load accepted agent role %s without a model, cap and budget: %+v", role, a)
			}
		}
	})
}
