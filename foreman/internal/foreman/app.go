package foreman

import (
	"context"

	"github.com/nateinaction/factory/foreman/internal/config"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// ExitUsage is the exit code of a malformed invocation, including an unknown agent role.
const ExitUsage = 2

// usage is printed for a malformed invocation.
const usage = `usage:
  foreman poll                                          claim one issue and queue its run
  foreman start <agent-role> <owner/repo> <issue>       on the worker: give factory a token, run, record the outcome
  foreman run <agent-role> <owner/repo> <issue>         as factory: clone, run the model, push, post`

// Main runs one foreman invocation (args exclude the program name) and returns the process exit code.
func Main(ctx context.Context, args []string, d Deps) int {
	verb, rest := "", []string(nil)
	if len(args) > 0 {
		verb, rest = args[0], args[1:]
	}
	switch {
	case verb == "poll" && len(rest) == 0:
		return poll(ctx, d)
	case (verb == "start" || verb == "run") && len(rest) == 3:
		return roleVerb(ctx, d, verb, rest)
	}
	return usageError(d, "bad invocation")
}

// usageError prints the usage and returns ExitUsage.
func usageError(d Deps, why string) int {
	d.logf("%s\n%s", why, usage)
	return ExitUsage
}

// fail reports err and returns exit code 1. Messages never carry tokens: errors name the call, not its credentials.
func fail(d Deps, err error) int {
	d.logf("%v", err)
	return 1
}

// poll loads the config and polls every repository one of its Apps is installed on.
func poll(ctx context.Context, d Deps) int {
	cfg, err := config.Load(d.Settings.ConfigFile)
	if err != nil {
		return fail(d, err)
	}
	code, err := Poll(ctx, d, cfg.Roles)
	if err != nil {
		return fail(d, err)
	}
	return code
}

// roleVerb parses and checks the arguments of `start` and `run`; an unknown agent role fails closed.
func roleVerb(ctx context.Context, d Deps, verb string, rest []string) int {
	role, err := workflow.ParseRole(rest[0])
	if err != nil {
		return usageError(d, err.Error())
	}
	if err := workflow.ValidateRepo(rest[1]); err != nil {
		return usageError(d, err.Error())
	}
	number, err := workflow.ParseIssueNumber(rest[2])
	if err != nil {
		return usageError(d, err.Error())
	}
	cfg, err := config.Load(d.Settings.ConfigFile)
	if err != nil {
		return fail(d, err)
	}
	var code int
	if verb == "start" {
		code, err = Start(ctx, d, role, rest[1], number)
	} else {
		code, err = Run(ctx, d, role, rest[1], cfg.Agents[string(role)], number)
	}
	if err != nil {
		d.logf("%v", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}
