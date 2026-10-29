package foreman

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Ceilings on every process foreman starts.
const (
	// GitTimeout bounds git and the small helpers.
	GitTimeout = 300 * time.Second
	// AgentTimeout bounds the Implementer's model run.
	AgentTimeout = 3600 * time.Second
	// RetroTimeout bounds the retro step.
	RetroTimeout = 900 * time.Second
	// ReviewTimeout bounds the Reviewer's model run.
	ReviewTimeout = 1800 * time.Second
	// MintTimeout bounds minting a token.
	MintTimeout = 30 * time.Second
)

// MaxAssignedRepos bounds how many assigned repositories one poll looks at.
const MaxAssignedRepos = 200

// Exit codes of `run reviewer`, read by `start reviewer`.
const (
	// ReviewClean means no high or medium findings.
	ReviewClean = 0
	// ReviewFindings means high or medium findings were posted.
	ReviewFindings = 10
	// ReviewExhausted means findings remain after MaxReviewRounds rounds.
	ReviewExhausted = 11
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
	return Settings{
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
func (s Settings) TokenPath(role Role) string {
	return filepath.Join(s.TokenDir, "gh-token-"+string(role))
}

// Config is what the Ansible role writes: a file rather than the environment, because sudo resets the environment.
type Config struct {
	// Roles are the agent roles whose Apps the host holds credentials for.
	Roles []Role `json:"roles"`
	Model string `json:"model"`
}

// LoadConfig reads and validates the config file.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
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
		if _, err := ParseRole(string(r)); err != nil {
			return Config{}, fmt.Errorf("config %s: %w", path, err)
		}
	}
	if c.Model == "" {
		return Config{}, fmt.Errorf("config %s: model is required", path)
	}
	return c, nil
}
