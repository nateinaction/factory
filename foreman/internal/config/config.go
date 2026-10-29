// Package config is foreman's edge configuration: the host's paths and endpoints, and the JSON file the
// Ansible role writes. It is parsed once at the edge and passed inward as values.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// Settings are the paths and endpoints at the edge. DefaultSettings fills them from the host layout.
type Settings struct {
	// API is GitHub's REST API root.
	API string
	// TokenDir is where `start` leaves the repo-scoped token for the factory user, one file per agent role.
	TokenDir string
	// KeyFile holds the OpenRouter key.
	KeyFile string
	// WorkRoot holds clones and scratch files.
	WorkRoot string
	// Self is foreman's installed path, re-run as the factory user.
	Self string
	// ConfigFile is the role-written JSON config.
	ConfigFile string
	// TokenHelper mints GitHub App tokens.
	TokenHelper string
	// Dagu is the dagu binary.
	Dagu string
	// DAGPrefix is prepended to an agent role to name the dagu DAG a poll enqueues.
	DAGPrefix string
}

// DefaultSettings is the host layout. Config stays under /etc/factory-agent and ~/.config/factory-agent.
func DefaultSettings(home, configOverride string) Settings {
	config := "/etc/factory-agent/config.json"
	if configOverride != "" {
		config = configOverride
	}

	// gosec reads the Token* and Key* field names as credentials; every value here is a path or URL.
	return Settings{ //nolint:gosec // see above
		API:         "https://api.github.com",
		TokenDir:    "/run/factory",
		KeyFile:     filepath.Join(home, ".config/factory-agent/openrouter-key"),
		WorkRoot:    filepath.Join(home, "work"),
		Self:        "/usr/local/bin/foreman",
		ConfigFile:  config,
		TokenHelper: "/usr/local/bin/github-app-token",
		Dagu:        "/usr/local/bin/dagu",
		DAGPrefix:   "factory-",
	}
}

// TokenPath is the token file of one agent role's run.
func (s Settings) TokenPath(role workflow.Role) string {
	return filepath.Join(s.TokenDir, "gh-token-"+string(role))
}

// AgentConfig is one agent role's model settings, rendered from `factory_models` by Ansible.
type AgentConfig struct {
	// Model is the OpenRouter model id, for the usage report.
	Model string `json:"model"`
	// ContextCap is the input-token cap the model alias carries.
	ContextCap int `json:"context_cap"`
	// Compaction is whether opencode compacts instead of overflowing.
	Compaction bool `json:"compaction"`
}

// Config is what the Ansible role writes: a file rather than the environment, because sudo resets the environment.
type Config struct {
	// Roles are the agent roles whose Apps the host holds credentials for.
	Roles []workflow.Role `json:"roles"`
	// Agents holds each agent role's model settings, keyed by the role's name.
	Agents map[string]AgentConfig `json:"agents"`
}

// Load reads and validates the config file: every agent role needs a model and a context cap.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own setting, read at the edge
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}

	if len(c.Roles) == 0 {
		return Config{}, fmt.Errorf("config %s: roles is required", path)
	}

	for _, r := range c.Roles {
		if _, err := workflow.ParseRole(string(r)); err != nil {
			return Config{}, fmt.Errorf("config %s: %w", path, err)
		}
	}

	for _, r := range workflow.KnownRoles() {
		a, ok := c.Agents[string(r)]
		if !ok || a.Model == "" || a.ContextCap <= 0 {
			return Config{}, fmt.Errorf("config %s: agent role %s needs a model and a context cap", path, r)
		}
	}

	return c, nil
}
