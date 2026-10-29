// Package config is foreman's edge configuration: the host's paths and endpoints, and the JSON file the
// Ansible role writes. It is parsed once at the edge and passed inward as values.
package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// Settings are the paths and endpoints at the edge. SettingsViper fills them from the host layout.
type Settings struct {
	// API is GitHub's REST API root.
	API string `mapstructure:"api"`
	// TokenDir is where `start` leaves the repo-scoped token for the factory user, one file per run.
	TokenDir string `mapstructure:"token-dir"`
	// KeyFile holds the OpenRouter key.
	KeyFile string `mapstructure:"key-file"`
	// WorkRoot holds clones and scratch files.
	WorkRoot string `mapstructure:"work-root"`
	// Home is the real home directory, hidden from every sandboxed run.
	Home string `mapstructure:"home"`
	// DaguHome is dagu's home directory, hidden from every sandboxed run.
	DaguHome string `mapstructure:"dagu-home"`
	// Srt is the sandbox backend binary every model process runs under.
	Srt string `mapstructure:"srt"`
	// Skills is the factory's forked skills directory, read-only inside every sandbox.
	Skills string `mapstructure:"skills"`
	// Self is foreman's installed path, re-run as the factory user.
	Self string `mapstructure:"self"`
	// ConfigFile is the JSON config the Ansible role writes.
	ConfigFile string `mapstructure:"config"`
	// TokenHelper mints GitHub App tokens.
	TokenHelper string `mapstructure:"token-helper"`
	// Dagu is the dagu binary.
	Dagu string `mapstructure:"dagu"`
	// DAGPrefix is prepended to an agent role to name the dagu DAG a poll enqueues.
	DAGPrefix string `mapstructure:"dag-prefix"`
	// FactoryRepo is the Ansible role config's factory repository, where a public repository's ready
	// skill or factory group is filed. The edge copies it here so a run can file a promoted issue.
	FactoryRepo string `mapstructure:"-"`
}

// SettingsViper returns the settings source: the host layout as defaults, overridden by FACTORY_*
// environment variables (dashes become underscores) and by flags the caller binds by their names.
// Decode it with SettingsFrom. Flags win over the environment, which wins over the defaults.
func SettingsViper(home string) *viper.Viper {
	v := viper.New()
	v.SetEnvPrefix("FACTORY")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	v.AutomaticEnv()
	// The Ansible role config's environment name predates the FACTORY_* settings and keeps working.
	v.MustBindEnv("config", "FACTORY_AGENT_CONFIG")

	// The host layout. Config stays under /etc/factory-agent and ~/.config/factory-agent.
	v.SetDefault("api", "https://api.github.com")
	v.SetDefault("token-dir", "/run/factory")
	v.SetDefault("key-file", filepath.Join(home, ".config/factory-agent/openrouter-key"))
	v.SetDefault("work-root", filepath.Join(home, "work"))
	v.SetDefault("home", home)
	v.SetDefault("dagu-home", "/var/lib/dagu")
	v.SetDefault("srt", "/nix/var/nix/profiles/default/bin/srt")
	v.SetDefault("skills", filepath.Join(home, ".config/opencode/skills"))
	v.SetDefault("self", "/usr/local/bin/foreman")
	v.SetDefault("config", "/etc/factory-agent/config.json")
	v.SetDefault("token-helper", "/usr/local/bin/github-app-token")
	v.SetDefault("dagu", "/usr/local/bin/dagu")
	v.SetDefault("dag-prefix", "factory-")

	return v
}

// SettingsFrom decodes v into Settings. A value of the wrong type is an error, never a fallback.
func SettingsFrom(v *viper.Viper) (Settings, error) {
	var s Settings
	if err := v.Unmarshal(&s); err != nil {
		return Settings{}, fmt.Errorf("settings: %w", err)
	}

	return s, nil
}

// TokenPath is the token file of one agent role's run. The run id keeps concurrent runs of one agent
// role from sharing, overwriting or deleting each other's file. Callers pass a vetted run id (see
// the foreman package's tokenPath).
func (s Settings) TokenPath(role workflow.Role, runID string) string {
	return filepath.Join(s.TokenDir, "gh-token-"+string(role)+"-"+runID)
}

// Budget is a run's step cap and wall-clock deadline: one agent role's run, or a retro.
type Budget struct {
	// Steps is opencode's step cap and foreman's count: reaching it fails the run.
	Steps int `json:"steps" mapstructure:"steps"`
	// DeadlineMinutes is the wall-clock limit, in minutes.
	DeadlineMinutes int `json:"deadline_minutes" mapstructure:"deadline_minutes"`
}

// Deadline is the budget's wall-clock limit.
func (b Budget) Deadline() time.Duration {
	return time.Duration(b.DeadlineMinutes) * time.Minute
}

// AgentConfig is one agent role's model settings and limits, rendered from `factory_models` and
// `factory_agent_limits` by Ansible; the latter carries the run's budget and the retro's.
type AgentConfig struct {
	// Model is the OpenRouter model id, for the usage report.
	Model string `json:"model" mapstructure:"model"`
	// ContextCap is the input-token cap the model alias carries.
	ContextCap int `json:"context_cap" mapstructure:"context_cap"`
	// Compaction is whether opencode compacts instead of overflowing.
	Compaction bool `json:"compaction" mapstructure:"compaction"`
	// Egress is the agent role's network policy; an empty allowlist is open egress.
	Egress EgressConfig `json:"egress" mapstructure:"egress"`
	// Budget is the agent role's step cap and wall-clock deadline.
	Budget `mapstructure:",squash"`
	// Retro is the budget of the agent role's retro, separate from the run's.
	Retro Budget `json:"retro" mapstructure:"retro"`
}

// EgressConfig is one agent role's egress allowlist, rendered from `factory_egress` by Ansible. A
// repository cannot change it.
type EgressConfig struct {
	// AllowedDomains limits egress when non-empty. Empty means open egress, the default.
	AllowedDomains []string `json:"allowed_domains" mapstructure:"allowed_domains"`
}

// Config is what the Ansible role writes: a file rather than the environment, because sudo resets the environment.
type Config struct {
	// Roles are the agent roles whose Apps the host holds credentials for.
	Roles []workflow.Role `json:"roles" mapstructure:"roles"`
	// Agents holds each agent role's model settings, keyed by the role's name.
	Agents map[string]AgentConfig `json:"agents" mapstructure:"agents"`
	// DeadRunMarginMinutes is how far past an agent role's deadline its running label may age before
	// a poll calls the run dead.
	DeadRunMarginMinutes int `json:"dead_run_margin_minutes" mapstructure:"dead_run_margin_minutes"`
	// ReviewRoundLimit is how many review rounds, CI failures included, an issue gets before it fails.
	ReviewRoundLimit int `json:"review_round_limit" mapstructure:"review_round_limit"`
	// CIPendingDeadlineMinutes is how long an issue waits in factory:review with pending CI before it fails.
	CIPendingDeadlineMinutes int `json:"ci_pending_deadline_minutes" mapstructure:"ci_pending_deadline_minutes"`
	// UnhostedRoles are the agent roles no worker host takes, rendered by Ansible from the worker
	// hosts' role labels. Empty means every agent role has a host.
	UnhostedRoles []workflow.Role `json:"unhosted_roles" mapstructure:"unhosted_roles"`
	// RoleCapacity is how many runs of each agent role the fleet may run at once, rendered by Ansible
	// as the worker hosts' run cap times the hosts that take the agent role. An agent role it does not
	// name has no capacity, so the poll never claims for it. Its keys are agent roles, like
	// UnhostedRoles; Ansible writes their names, and Load rejects a name it does not know.
	RoleCapacity map[workflow.Role]int `json:"role_capacity" mapstructure:"role_capacity"`
	// FactoryRepo is the repository a public repository's skill or factory lesson is promoted to.
	// Empty means no repository is configured, so such a promotion fails closed.
	FactoryRepo string `json:"factory_repo" mapstructure:"factory_repo"`
}

// Unhosted reports whether no worker host takes the agent role. Its issues stay queued.
func (c Config) Unhosted(role workflow.Role) bool {
	return slices.Contains(c.UnhostedRoles, role)
}

// Capacity is how many runs of the agent role the fleet may run at once. An agent role the config
// does not name has no capacity, so the poll never claims for it.
func (c Config) Capacity(role workflow.Role) int {
	return c.RoleCapacity[role]
}

// RetroBudget is the retro budget of the agent role's config entry. Load has checked every known
// agent role has one.
func (c Config) RetroBudget(role workflow.Role) Budget {
	return c.Agents[string(role)].Retro
}

// CIPendingDeadline is how long pending CI may keep an issue queued before it fails.
func (c Config) CIPendingDeadline() time.Duration {
	return time.Duration(c.CIPendingDeadlineMinutes) * time.Minute
}

// DeadRunMargin is how long past an agent role's deadline a run keeps its claim before a poll calls
// it dead.
func (c Config) DeadRunMargin() time.Duration {
	return time.Duration(c.DeadRunMarginMinutes) * time.Minute
}

// Load reads and validates the Ansible role config through viper. An unknown key is an error, never ignored:
// a typo must not silently fall back to a default. Every agent role needs a model and a context cap.
func Load(path string) (Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("json")

	if err := v.ReadInConfig(); err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}

	var c Config
	if err := v.UnmarshalExact(&c); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}

	if err := validate(path, c); err != nil {
		return Config{}, err
	}

	return c, nil
}

// validate checks the roles and agent settings of a loaded config.
func validate(path string, c Config) error {
	if len(c.Roles) == 0 {
		return fmt.Errorf("config %s: roles is required", path)
	}

	if err := validateRoles(path, c.Roles); err != nil {
		return err
	}

	if err := validateAgents(path, c.Agents); err != nil {
		return err
	}

	if err := validateRequired(path, c.Agents); err != nil {
		return err
	}

	if err := validateLimits(path, c); err != nil {
		return err
	}

	if err := validateUnhosted(path, c.UnhostedRoles); err != nil {
		return err
	}

	if err := validateRoleCapacity(path, c.RoleCapacity); err != nil {
		return err
	}

	return validateFactoryRepo(path, c.FactoryRepo)
}

// validateRoleCapacity rejects a capacity for an agent role the workflow does not know, and a
// negative one. A missing agent role is allowed: it has no capacity, and its issues stay queued.
// Decoding a JSON key into a Role accepts any string, so the key is checked here, not by the decoder.
func validateRoleCapacity(path string, capacity map[workflow.Role]int) error {
	for _, role := range slices.Sorted(maps.Keys(capacity)) {
		if _, err := workflow.ParseRole(string(role)); err != nil {
			return fmt.Errorf("config %s: role capacity: %w", path, err)
		}

		if capacity[role] < 0 {
			return fmt.Errorf("config %s: role capacity for %s must not be negative", path, role)
		}
	}

	return nil
}

// validateLimits checks the dead-run margin, the review rounds and the CI deadline.
func validateLimits(path string, c Config) error {
	if err := validateDeadRun(path, c.DeadRunMarginMinutes); err != nil {
		return err
	}

	if err := validateReviewRoundLimit(path, c.ReviewRoundLimit); err != nil {
		return err
	}

	return validateCIPendingDeadline(path, c.CIPendingDeadlineMinutes)
}

// validateUnhosted rejects an unhosted agent role the workflow does not know.
func validateUnhosted(path string, roles []workflow.Role) error {
	for _, r := range roles {
		if _, err := workflow.ParseRole(string(r)); err != nil {
			return fmt.Errorf("config %s: unhosted roles: %w", path, err)
		}
	}

	return nil
}

// validateFactoryRepo checks the configured factory repository, when there is one, is owner/name.
func validateFactoryRepo(path, repo string) error {
	if repo == "" {
		return nil
	}

	if _, err := workflow.ParseRepo(repo); err != nil {
		return fmt.Errorf("config %s: factory repo: %w", path, err)
	}

	return nil
}

// validateRequired checks every known agent role has a model, a context cap, and a step cap and
// deadline for both its run and its retro.
func validateRequired(path string, agents map[string]AgentConfig) error {
	for _, r := range workflow.KnownRoles() {
		a, ok := agents[string(r)]
		if !ok || a.Model == "" || a.ContextCap <= 0 {
			return fmt.Errorf("config %s: agent role %s needs a model and a context cap", path, r)
		}

		if a.Steps <= 0 || a.DeadlineMinutes <= 0 {
			return fmt.Errorf("config %s: agent role %s needs a step cap and a deadline", path, r)
		}

		if a.Retro.Steps <= 0 || a.Retro.DeadlineMinutes <= 0 {
			return fmt.Errorf("config %s: agent role %s needs a retro step cap and deadline", path, r)
		}
	}

	return nil
}

// validateDeadRun checks the dead-run margin is a positive number of minutes.
func validateDeadRun(path string, minutes int) error {
	if minutes <= 0 {
		return fmt.Errorf("config %s: dead-run margin must be a positive number of minutes", path)
	}

	return nil
}

// validateReviewRoundLimit checks the review round limit is a positive number of rounds.
func validateReviewRoundLimit(path string, limit int) error {
	if limit <= 0 {
		return fmt.Errorf("config %s: review round limit must be a positive number of rounds", path)
	}

	return nil
}

// validateCIPendingDeadline checks the CI pending deadline is a positive number of minutes.
func validateCIPendingDeadline(path string, minutes int) error {
	if minutes <= 0 {
		return fmt.Errorf("config %s: CI pending deadline must be a positive number of minutes", path)
	}

	return nil
}

// validateRoles rejects an agent role the workflow does not know.
func validateRoles(path string, roles []workflow.Role) error {
	for _, r := range roles {
		if _, err := workflow.ParseRole(string(r)); err != nil {
			return fmt.Errorf("config %s: %w", path, err)
		}
	}

	return nil
}

// validateAgents rejects an agent role the workflow does not know.
func validateAgents(path string, agents map[string]AgentConfig) error {
	for _, name := range slices.Sorted(maps.Keys(agents)) {
		if _, err := workflow.ParseRole(name); err != nil {
			return fmt.Errorf("config %s: agents: %w", path, err)
		}
	}

	return nil
}
