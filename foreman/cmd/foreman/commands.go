package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// roleArgCount is how many positional arguments `start` and `run` take: agent role, repository,
// issue.
const roleArgCount = 3

// The two verbs that run one agent role.
const (
	startVerb = "start"
	runVerb   = "run"
)

// logsArgCount is how many positional arguments `logs` takes: repository, issue.
const logsArgCount = 2

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
			return errors.New("a command is required: poll, start, run, status, logs or history")
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

	root.AddCommand(
		pollCommand(d), roleCommand(d, startVerb), roleCommand(d, runVerb), statusCommand(d), logsCommand(d),
		historyCommand(d), drainCheckCommand(d),
	)

	return root, nil
}

// sudoRunArgs is the exact command line sudo may start: the verb `run` and its three positional
// arguments, the agent role, repository and issue.
const sudoRunArgs = roleArgCount + 1

// sudoRun reports whether args is exactly the one shape sudo permits: the verb `run` followed by
// exactly three positional arguments, with no flag anywhere. Nothing else is a run, so no flag can
// point the factory half at another sandbox binary, key file, skills directory, config or foreman
// itself, and no other verb or an extra positional can run as the factory user.
//
// Refusing every flag is enough only because the settings a flag names also come from FACTORY_*
// environment variables, and sudo's env_reset drops them: the sudoers file keeps only DAG_RUN_ID and
// TRACEPARENT, which name a run and link its trace, so a caller cannot pass an override in the
// environment instead.
func sudoRun(args []string) bool {
	if len(args) != sudoRunArgs || args[0] != runVerb {
		return false
	}

	for _, arg := range args[1:] {
		if arg == "" || strings.HasPrefix(arg, "-") {
			return false
		}
	}

	return true
}

// executeCommand runs one command line. When sudo started foreman, only `run` with exactly its three
// positional arguments is permitted. It is checked against the raw arguments, before cobra parses or
// acts on anything, so even --help is refused.
func executeCommand(ctx context.Context, root *cobra.Command, args []string, underSudo bool) (*cobra.Command, error) {
	if underSudo && !sudoRun(args) {
		return root, fmt.Errorf(
			"sudo permits only `foreman run` followed by exactly the agent role, repository and issue: "+
				"refusing %q", strings.Join(args, " "),
		)
	}

	root.SetArgs(args)

	cmd, err := root.ExecuteContextC(ctx)
	if err != nil {
		return cmd, fmt.Errorf("execute command: %w", err)
	}

	return cmd, nil
}

// addSettingsFlags defines the persistent flags that override Settings. Each flag's name is its
// viper key, so the environment names follow the setting names: --work-root, FACTORY_WORK_ROOT.
func addSettingsFlags(fs *pflag.FlagSet) {
	fs.String("api", "", "GitHub REST API root")
	fs.String("token-dir", "", "directory holding each agent role's token file")
	fs.String("key-file", "", "file holding the OpenRouter key")
	fs.String("work-root", "", "directory holding clones and scratch files")
	fs.String("home", "", "the real home directory, hidden from sandboxed runs")
	fs.String("dagu-home", "", "dagu's home directory, hidden from sandboxed runs")
	fs.String("srt", "", "the sandbox backend binary every model process runs under")
	fs.String("skills", "", "the factory's forked skills directory")
	fs.String("self", "", "foreman's installed path, re-run as the factory user")
	fs.String("config", "", "the Ansible-written role config (FACTORY_AGENT_CONFIG)")
	fs.String("token-helper", "", "the GitHub App token helper")
	fs.String("dagu", "", "the dagu binary")
	fs.String("dag-prefix", "", "prefix of an agent role's dagu DAG")
	fs.String("telemetry-url", "", "the telemetry backend's trace API, which status reads usage from")
}

// pollCommand is `foreman poll`: claim up to each agent role's free run capacity and queue the runs.
func pollCommand(d *foreman.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "poll",
		Short: "claim what each agent role's free capacity allows and queue the runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(d.Settings.ConfigFile)
			if err != nil {
				return &exitError{code: 1, err: err}
			}

			code, err := foreman.Poll(cmd.Context(), *d, cfg)

			return result(code, err)
		},
	}
}

// roleCommand is `foreman start` or `foreman run`: one agent role on one issue.
func roleCommand(d *foreman.Deps, verb string) *cobra.Command {
	args := cobra.ExactArgs(roleArgCount)
	if verb == startVerb {
		// The DAG appends its TRACEPARENT parameter after the issue; a hand-run start omits it.
		args = cobra.RangeArgs(roleArgCount, roleArgCount+1)
	}

	return &cobra.Command{
		Use:   verb + " <agent-role> <owner/repo> <issue>",
		Short: roleShort(verb),
		Args:  args,
		RunE: func(cmd *cobra.Command, args []string) error {
			role, err := workflow.ParseRole(args[0])
			if err != nil {
				return err
			}

			ref, err := workflow.ParseIssueRef(args[1], args[2])
			if err != nil {
				return err
			}

			cfg, err := config.Load(d.Settings.ConfigFile)
			if err != nil {
				return &exitError{code: 1, err: err}
			}

			var code int

			if verb == startVerb {
				code, err = foreman.Start(cmd.Context(), *d, cfg, role, ref)
			} else {
				d.Settings.FactoryRepo = cfg.FactoryRepo
				code, err = foreman.Run(
					cmd.Context(), *d, role, ref,
					cfg.Agents[string(role)], cfg.RetroBudget(role), cfg.ReviewRoundLimit,
				)
			}

			return result(code, err)
		},
	}
}

// roleShort is the one-line description of start or run.
func roleShort(verb string) string {
	if verb == startVerb {
		return "on the worker: give factory a token, run, record the outcome"
	}

	return "as factory: clone, run the model, push, post"
}

// statusCommand is `foreman status`: the read-only report a person runs.
func statusCommand(d *foreman.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "show active runs, queues, roles with no host and recent usage",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(d.Settings.ConfigFile)
			if err != nil {
				return &exitError{code: 1, err: err}
			}

			code, err := foreman.Status(cmd.Context(), *d, cfg)

			return result(code, err)
		},
	}
}

// logsCommand is `foreman logs`: the latest dagu run log for one issue, optionally followed.
func logsCommand(d *foreman.Deps) *cobra.Command {
	var follow bool

	cmd := &cobra.Command{
		Use:   "logs <owner/repo> <issue>",
		Short: "print the issue's latest dagu run log",
		Args:  cobra.ExactArgs(logsArgCount),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := workflow.ParseIssueRef(args[0], args[1])
			if err != nil {
				return err
			}

			code, err := foreman.Logs(cmd.Context(), *d, ref, follow)

			return result(code, err)
		},
	}
	cmd.Flags().BoolVar(&follow, "follow", false, "keep printing new output until the run ends")

	return cmd
}

// historyCommand is `foreman history`: each issue's lifecycle in one assigned repository, read from
// GitHub.
func historyCommand(d *foreman.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "history <owner/repo>",
		Short: "show each issue's time in every state, human gate wait, rework rounds and outcome, from GitHub",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := workflow.ParseRepo(args[0])
			if err != nil {
				return err
			}

			cfg, err := config.Load(d.Settings.ConfigFile)
			if err != nil {
				return &exitError{code: 1, err: err}
			}

			code, err := foreman.History(cmd.Context(), *d, cfg, repo)

			return result(code, err)
		},
	}
}

// drainCheckCommand is `foreman drain-check`: exit 0 only when the latest run of every named DAG
// has finished, for the deploy's cutover guard.
func drainCheckCommand(d *foreman.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "drain-check <dag>...",
		Short: "exit 0 only if the latest run of every named DAG has finished",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			code, err := foreman.DrainCheck(cmd.Context(), *d, args)

			return result(code, err)
		},
	}
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
