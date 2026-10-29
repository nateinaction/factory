package foreman

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/nateinaction/factory/foreman/internal/github"
	"github.com/nateinaction/factory/foreman/internal/proc"
)

// devEnv is the part of `nix print-dev-env --json` foreman reads: the shell variables, of which PATH
// names the resolved toolchain's store paths.
type devEnv struct {
	Variables map[string]string `json:"variables"`
}

// toolchain resolves the repository's dev environment outside the sandbox, from the flake on the
// default branch. A change to flake.nix on the working branch has no effect until it merges.
// accept-flake-config is off, so a flake cannot grant itself settings, and the evaluation is told not
// to write a lock file: the default branch is used exactly as committed.
func (d Deps) toolchain(
	ctx context.Context, c github.Client, repo, token, branch string, f runFiles,
) (string, error) {
	if branch == "" {
		var err error
		if branch, err = c.DefaultBranch(ctx, repo); err != nil {
			return "", err
		}
	}

	if _, err := d.run(ctx, proc.Cmd{
		Argv: []string{
			gitProgram, "clone", "--depth", "1", "--branch", branch,
			"https://github.com/" + repo + ".git", f.toolchain,
		},
		Env: GitAuthEnv(token), Timeout: ToolchainTimeout,
	}); err != nil {
		return "", err
	}

	out, err := d.run(ctx, proc.Cmd{
		Argv: []string{
			"nix", "print-dev-env", "--json",
			"--option", "accept-flake-config", "false",
			"--no-write-lock-file",
			"--extra-experimental-features", "nix-command flakes",
			"--flake", "path:" + f.toolchain,
		},
		Dir: f.toolchain, Env: nixEnv(f), Timeout: ToolchainTimeout, Capture: true,
	})
	if err != nil {
		return "", fmt.Errorf("resolve the default branch's dev environment: %w", err)
	}

	var env devEnv
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return "", fmt.Errorf("parse the default branch's dev environment: %w", err)
	}

	if env.Variables["PATH"] == "" {
		return "", fmt.Errorf("the default branch's flake resolved no PATH for %s", repo)
	}

	return env.Variables["PATH"], nil
}

// nixEnv is the environment of the dev-environment resolution: the run's private HOME and XDG
// directories, with the host's nix profile on PATH. Nix never runs inside the sandbox.
func nixEnv(f runFiles) map[string]string {
	env := f.env("")
	env["PATH"] = nixProfilePath + ":" + defaultPath

	return env
}

// toolchainPaths is the PATH a run may read as the resolved dev environment.
func toolchainPaths(pathValue string) []string {
	out := make([]string, 0, len(filepath.SplitList(pathValue)))
	for _, p := range filepath.SplitList(pathValue) {
		if p != "" {
			out = append(out, p)
		}
	}

	return out
}
