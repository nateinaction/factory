package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/foreman"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// exitError carries a non-zero exit code through cobra's error return.
type exitError struct {
	// code is the process exit status.
	code int
	// err is the failure to report, or nil when the code itself is the result (a review verdict).
	err error
}

// Error reports the failure, or the exit code when there is none.
func (e *exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit %d", e.code)
	}

	return e.err.Error()
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *exitError) Unwrap() error {
	return e.err
}

// roleArgCount is how many positional arguments `start` and `run` take: role, repository, issue.
const roleArgCount = 3

// newRootCommand builds the CLI: one command per verb, and the settings flags on the root.
func newRootCommand(d *foreman.Deps, settings *viper.Viper) (*cobra.Command, error) {
	root := &cobra.Command{
		Use:           "foreman",
		Short:         "Poll GitHub for issues and run agent roles on them",
		Long:          "foreman claims issues GitHub marks ready for an agent role and runs that role on them.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("a command is required: poll, start or run")
		},
		PersistentPreRunE: func(*cobra.Command, []string) error {
			s, err := config.SettingsFrom(settings)
			if err != nil {
				return &exitError{code: 1, err: err}
			}

			d.Settings = s

			return nil
		},
	}
	// The generated completion verb would widen the invocation contract the dagu DAGs rely on.
	root.CompletionOptions.DisableDefaultCmd = true

	addSettingsFlags(root.PersistentFlags())

	if err := settings.BindPFlags(root.PersistentFlags()); err != nil {
		return nil, fmt.Errorf("bind settings flags: %w", err)
	}

	root.AddCommand(pollCommand(d), roleCommand(d, "start"), roleCommand(d, "run"))

	return root, nil
}

// addSettingsFlags defines the persistent flags that override Settings. Each flag's name is its
// viper key, so the environment names follow the setting names: --work-root, FACTORY_WORK_ROOT.
func addSettingsFlags(fs *pflag.FlagSet) {
	fs.String("api", "", "GitHub REST API root")
	fs.String("token-dir", "", "directory holding each agent role's token file")
	fs.String("key-file", "", "file holding the OpenRouter key")
	fs.String("work-root", "", "directory holding clones and scratch files")
	fs.String("self", "", "foreman's installed path, re-run as the factory user")
	fs.String("config", "", "the Ansible-written role config (FACTORY_AGENT_CONFIG)")
	fs.String("token-helper", "", "the GitHub App token helper")
	fs.String("dagu", "", "the dagu binary")
	fs.String("dag-prefix", "", "prefix of an agent role's dagu DAG")
}

// pollCommand is `foreman poll`: claim one issue and queue its run.
func pollCommand(d *foreman.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "poll",
		Short: "claim one issue and queue its run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(d.Settings.ConfigFile)
			if err != nil {
				return &exitError{code: 1, err: err}
			}

			code, err := foreman.Poll(cmd.Context(), *d, cfg.Roles)

			return result(code, err)
		},
	}
}

// roleCommand is `foreman start` or `foreman run`: one agent role on one issue.
func roleCommand(d *foreman.Deps, verb string) *cobra.Command {
	return &cobra.Command{
		Use:   verb + " <agent-role> <owner/repo> <issue>",
		Short: roleShort(verb),
		Args:  cobra.ExactArgs(roleArgCount),
		RunE: func(cmd *cobra.Command, args []string) error {
			role, err := workflow.ParseRole(args[0])
			if err != nil {
				return err
			}

			if err := workflow.ValidateRepo(args[1]); err != nil {
				return err
			}

			number, err := workflow.ParseIssueNumber(args[2])
			if err != nil {
				return err
			}

			cfg, err := config.Load(d.Settings.ConfigFile)
			if err != nil {
				return &exitError{code: 1, err: err}
			}

			var code int

			if verb == "start" {
				code, err = foreman.Start(cmd.Context(), *d, role, args[1], number)
			} else {
				code, err = foreman.Run(cmd.Context(), *d, role, args[1], cfg.Agents[string(role)], number)
			}

			return result(code, err)
		},
	}
}

// roleShort is the one-line description of start or run.
func roleShort(verb string) string {
	if verb == "start" {
		return "on the worker: give factory a token, run, record the outcome"
	}

	return "as factory: clone, run the model, push, post"
}

// result carries a command's exit code through cobra. A failure without a code exits 1.
func result(code int, err error) error {
	if code == 0 && err == nil {
		return nil
	}

	if code == 0 {
		code = 1
	}

	return &exitError{code: code, err: err}
}
