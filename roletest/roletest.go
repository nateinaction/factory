// Package roletest holds the two helpers every Ansible role's Ginkgo suite shares: rendering a
// role's test playbook, and building a fake binary out of the test binary itself.
package roletest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
)

// DefaultTimeout bounds a render that sets no Timeout of its own.
const DefaultTimeout = 2 * time.Minute

// Options are the inputs of a render.
type Options struct {
	// Vars are passed to the playbook as extra vars, which win over group_vars.
	Vars map[string]any
	// Env is added to the environment of ansible-playbook.
	Env map[string]string
	// Timeout bounds the render; zero means DefaultTimeout.
	Timeout time.Duration
}

// Rendered is the outcome of a successful render.
type Rendered struct {
	// Dir is the directory this render owns. The playbook receives it as the extra var render_dir.
	Dir string
	// Log is the playbook's combined output.
	Log string
}

// Render runs the playbook into a directory of its own and fails the spec, with the playbook's log,
// if the render fails.
func Render(playbook string, opts Options) Rendered {
	GinkgoHelper()
	rendered, err := TryRender(playbook, opts)
	if err != nil {
		Fail(err.Error())
	}
	return rendered
}

// TryRender runs ansible-playbook on the playbook with the repo's Ansible config, into a fresh
// directory under the current spec's temp directory. Call it from inside a spec.
// A failed run, or one that outlives the timeout, is an error that carries the playbook's log.
func TryRender(playbook string, opts Options) (Rendered, error) {
	root, err := repoRoot()
	if err != nil {
		return Rendered{}, err
	}
	dir, err := os.MkdirTemp(GinkgoT().TempDir(), "render-")
	if err != nil {
		return Rendered{}, fmt.Errorf("create render directory: %w", err)
	}

	vars := map[string]any{"render_dir": dir}
	for k, v := range opts.Vars {
		vars[k] = v
	}
	extra, err := json.Marshal(vars)
	if err != nil {
		return Rendered{}, fmt.Errorf("encode extra vars: %w", err)
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ansible-playbook", playbook, "-e", string(extra))
	cmd.Env = append(os.Environ(), "ANSIBLE_CONFIG="+filepath.Join(root, "ansible.cfg"))
	for k, v := range opts.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// Kill the whole run, not just ansible-playbook, once the deadline passes or its pipes stay open.
	cmd.WaitDelay = 5 * time.Second
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Rendered{}, fmt.Errorf("render of %s timed out after %s\n%s", playbook, timeout, out)
		}
		return Rendered{}, fmt.Errorf("render of %s failed: %w\n%s", playbook, runErr, out)
	}
	return Rendered{Dir: dir, Log: string(out)}, nil
}

// repoRoot finds the directory that holds ansible.cfg, walking up from the working directory.
// The walk is bounded by the depth of the path.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("find working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "ansible.cfg")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no ansible.cfg in any parent of the working directory")
		}
		dir = parent
	}
}

// FakeMain is the body of a fake binary: it gets its own name and arguments and returns an exit code.
type FakeMain func(name string, args []string) int

// RunFake is called first in a suite's TestMain. When the test binary was started under a name in
// fakes, it runs that fake and exits; otherwise it returns and the tests run.
func RunFake(fakes map[string]FakeMain) {
	name := filepath.Base(os.Args[0])
	if fake, ok := fakes[name]; ok {
		os.Exit(fake(name, os.Args[1:]))
	}
}

// BuildFake makes dir/name a link to the running test binary, so that running it re-executes the
// tests under the fake's name, which RunFake answers. Put dir on a PATH to stand in for the binary.
func BuildFake(dir, name string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find test binary: %w", err)
	}
	path := filepath.Join(dir, name)
	if err := os.Symlink(self, path); err != nil {
		return "", fmt.Errorf("link fake %s: %w", name, err)
	}
	return path, nil
}
