package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nateinaction/factory/foreman/internal/foreman"
	"github.com/nateinaction/factory/foreman/internal/githubapp"
)

// The token helper's two verbs.
const (
	tokenVerb        = "token"
	repositoriesVerb = "repositories"
)

// The token helper's flags.
const (
	flagApp      = "--app"
	flagRepo     = "--repo"
	flagReadOnly = "--read-only"
)

// The environment variables the token helper reads, as the Python helper did.
const (
	appDirEnv = "GITHUB_APP_DIR"
	apiURLEnv = "GITHUB_API_URL"
	// defaultAppDir holds one credential directory per agent role.
	defaultAppDir = "/etc/github-app"
	// helperTimeout bounds one helper command: a few requests, each with its own timeout.
	helperTimeout = 2 * time.Minute
	// requestTimeout bounds one API request.
	requestTimeout = 20 * time.Second
)

// helperArgs are the parsed arguments of `token` or `repositories`.
type helperArgs struct {
	app, repo string
	readOnly  bool
}

// parseHelperArgs reads the flags of the token helper's verbs. `token` takes --app, --repo and
// --read-only; `repositories` takes --app. A flag given twice, an unknown argument and a missing
// required flag are errors, so no repeat silently wins.
func parseHelperArgs(verb string, args []string) (helperArgs, error) {
	required := []string{flagApp}
	accepted := []string{flagApp}

	if verb == tokenVerb {
		required = append(required, flagRepo)
		accepted = append(accepted, flagRepo, flagReadOnly)
	}

	seen, unknown, err := scanFlags(accepted, args)
	if err != nil {
		return helperArgs{}, err
	}

	if len(unknown) > 0 {
		return helperArgs{}, fmt.Errorf("unrecognized arguments: %s", strings.Join(unknown, " "))
	}

	var missing []string

	for _, name := range required {
		if _, ok := seen[name]; !ok {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		return helperArgs{}, fmt.Errorf("the following arguments are required: %s", strings.Join(missing, ", "))
	}

	_, readOnly := seen[flagReadOnly]

	return helperArgs{app: seen[flagApp], repo: seen[flagRepo], readOnly: readOnly}, nil
}

// scanFlags finds the accepted flags in args: each flag's value (empty for the switch), and the
// arguments that are no accepted flag. A flag takes its value from `--flag=value` or the next argument.
func scanFlags(accepted, args []string) (map[string]string, []string, error) {
	var unknown []string

	seen := map[string]string{}

	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(args[i], "=")
		if !slices.Contains(accepted, name) {
			unknown = append(unknown, args[i])
			continue
		}

		if _, dup := seen[name]; dup {
			return nil, nil, fmt.Errorf("argument %s: given more than once", name)
		}

		if name != flagReadOnly && !hasValue {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return nil, nil, fmt.Errorf("argument %s: expected one argument", name)
			}

			i++
			value = args[i]
		}

		if name == flagReadOnly && hasValue {
			return nil, nil, fmt.Errorf("argument %s: ignored explicit argument '%s'", name, value)
		}

		seen[name] = value
	}

	return seen, unknown, nil
}

// helperCommand is `foreman token` or `foreman repositories`: the GitHub App token helper that
// foreman calls as a separate process. It prints on stdout only on success, one token or one
// OWNER/NAME per line, and any failure prints nothing there and exits non-zero. Cobra does not parse
// its flags: parseHelperArgs does, so a settings flag is no way to point it at another App.
func helperCommand(d *foreman.Deps, verb string) *cobra.Command {
	use, short := tokenVerb+" --app <agent-role> --repo <owner/name> [--read-only]",
		"print a token of an agent role's App for one repository"
	if verb == repositoriesVerb {
		use, short = repositoriesVerb+" --app <agent-role>", "list where an agent role's App is installed"
	}

	return &cobra.Command{
		Use:                use,
		Short:              short,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
				return cmd.Help()
			}

			parsed, err := parseHelperArgs(verb, args)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), helperTimeout)
			defer cancel()

			lines, err := helperLines(ctx, helperClient(d), verb, parsed)
			if err != nil {
				return &exitError{code: 1, err: err}
			}

			// An App installed nowhere lists nothing, and prints nothing.
			if len(lines) == 0 {
				return nil
			}

			if _, err := fmt.Fprintln(cmd.OutOrStdout(), strings.Join(lines, "\n")); err != nil {
				return &exitError{code: 1, err: fmt.Errorf("write result: %w", err)}
			}

			return nil
		},
	}
}

// helperLines is what the verb prints, computed whole before anything is written.
func helperLines(ctx context.Context, client githubapp.Client, verb string, a helperArgs) ([]string, error) {
	if verb == tokenVerb {
		token, err := client.Token(ctx, a.app, a.repo, a.readOnly)
		if err != nil {
			return nil, fmt.Errorf("token: %w", err)
		}

		return []string{token}, nil
	}

	repos, err := client.Repositories(ctx, a.app)
	if err != nil {
		return nil, fmt.Errorf("repositories: %w", err)
	}

	return repos, nil
}

// helperClient builds the helper's edge from the environment and the settings.
func helperClient(d *foreman.Deps) githubapp.Client {
	dir := os.Getenv(appDirEnv)
	if dir == "" {
		dir = defaultAppDir
	}

	api := os.Getenv(apiURLEnv)
	if api == "" {
		api = d.Settings.API
	}

	return githubapp.Client{Dir: dir, API: api, HTTP: &http.Client{Timeout: requestTimeout}}
}
