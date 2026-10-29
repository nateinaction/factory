package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// testAgent is the agent role settings tests run with.
var testAgent = AgentConfig{Model: "prov/model", ContextCap: 200_000, Compaction: true}

// TestDefaultSettingsKeepTheConfigLocations pins: default settings keep the config locations.
func TestDefaultSettingsKeepTheConfigLocations(t *testing.T) {
	s := DefaultSettings("/home/factory", "")
	if s.ConfigFile != "/etc/factory-agent/config.json" || s.KeyFile != "/home/factory/.config/factory-agent/openrouter-key" ||
		s.WorkRoot != "/home/factory/work" || s.TokenPath(workflow.Reviewer) != "/run/factory/gh-token-reviewer" || s.API != "https://api.github.com" {
		t.Errorf("%+v", s)
	}
	if DefaultSettings("/h", "/x.json").ConfigFile != "/x.json" {
		t.Error("override ignored")
	}
}

// TestStartNamesTheTokenFileForItsAgentRole pins: each run's token file is named for its agent role.
func TestStartNamesTheTokenFileForItsAgentRole(t *testing.T) {
	s := Settings{TokenDir: "/run/factory"}
	if got := s.TokenPath(workflow.Reviewer); got != "/run/factory/gh-token-reviewer" {
		t.Errorf("got %q", got)
	}
	if s.TokenPath(workflow.Reviewer) == s.TokenPath(workflow.Implementer) {
		t.Error("two agent roles share a token file")
	}
}

// TestLoadConfigRequiresEveryAgentRole pins: each of the five agent roles has a model and a cap.
func TestLoadConfigRequiresEveryAgentRole(t *testing.T) {
	good := func() map[string]AgentConfig {
		m := map[string]AgentConfig{}
		for _, r := range workflow.KnownRoles() {
			m[string(r)] = testAgent
		}
		return m
	}
	load := func(agents map[string]AgentConfig) error {
		raw, err := json.Marshal(map[string]any{"roles": []workflow.Role{workflow.Implementer}, "agents": agents})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "c.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = Load(path)
		return err
	}
	if err := load(good()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string]AgentConfig){
		"missing role": func(m map[string]AgentConfig) { delete(m, "curator") },
		"no model":     func(m map[string]AgentConfig) { m["planner"] = AgentConfig{ContextCap: 1} },
		"no cap":       func(m map[string]AgentConfig) { m["planner"] = AgentConfig{Model: "m"} },
	} {
		m := good()
		mutate(m)
		if load(m) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
