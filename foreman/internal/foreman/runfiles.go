package foreman

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/nateinaction/factory/foreman/internal/opencode"
	"github.com/nateinaction/factory/foreman/internal/sandbox"
	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// defaultPath is the host's own tool path, appended to the default branch's dev environment so the
// model can still find opencode and the usual system tools.
const defaultPath = "/usr/local/bin:/usr/bin:/bin"

// nixProfilePath is where the hosts install nix and the sandbox runtime.
const nixProfilePath = "/nix/var/nix/profiles/default/bin"

// nixDaemonSocket is the Nix daemon socket, which no sandboxed run may reach.
const nixDaemonSocket = "/nix/var/nix/daemon-socket"

// sandboxFiles are one run's private sandbox directories. Everything here is deleted after the run and its
// retro, and nothing here is shared with another run.
type sandboxFiles struct {
	// dir holds HOME, XDG directories, scratch, state and the settings file.
	dir string
	// home is the run's HOME and the parent of its XDG directories.
	home string
	// tmp is the run's TMPDIR.
	tmp string
	// state is the run's opencode state directory.
	state string
	// settings is the srt settings file.
	settings string
	// toolchain is the default branch clone the dev environment is resolved from.
	toolchain string
}

// newSandboxFiles clears and creates one run's directories. A re-run starts from nothing.
func newSandboxFiles(root string, role workflow.Role, number int) (sandboxFiles, error) {
	dir, err := freshPath(root, fmt.Sprintf("run-%s-%d", role, number))
	if err != nil {
		return sandboxFiles{}, err
	}

	f := sandboxFiles{
		dir: dir, home: filepath.Join(dir, "home"), tmp: filepath.Join(dir, "tmp"),
		state:    filepath.Join(dir, "state"),
		settings: filepath.Join(dir, "srt-settings.json"),
	}

	for _, path := range []string{
		f.home, f.tmp, f.state,
		filepath.Join(f.home, ".cache"), filepath.Join(f.home, ".config"),
		filepath.Join(f.home, ".local", "share"), filepath.Join(f.home, ".local", "state"),
	} {
		if err := os.MkdirAll(path, privateDirMode); err != nil {
			return sandboxFiles{}, fmt.Errorf("run directory %s: %w", path, err)
		}
	}

	// The toolchain sits beside the run directory, not inside it: it is read-only inside the
	// sandbox, while everything under the run directory is writable.
	if f.toolchain, err = freshPath(root, fmt.Sprintf("toolchain-%s-%d", role, number)); err != nil {
		return sandboxFiles{}, err
	}

	return f, nil
}

// remove deletes the run directory and the toolchain clone.
func (f sandboxFiles) remove() error {
	return errors.Join(os.RemoveAll(f.dir), os.RemoveAll(f.toolchain))
}

// db is the run's opencode state database. The Reviewer's state never touches the disk.
func (f sandboxFiles) db(role workflow.Role) string {
	if role == workflow.Reviewer {
		return opencode.InMemoryDB
	}

	return filepath.Join(f.state, "opencode.db")
}

// env is the process environment of a sandboxed run. It is the whole environment, not an addition:
// nothing from foreman's own environment reaches the model. toolchainPath is the PATH the default
// branch's flake resolved; empty falls back to the host's own tools.
func (f sandboxFiles) env(toolchainPath string) map[string]string {
	path := defaultPath
	if toolchainPath != "" {
		path = toolchainPath + ":" + defaultPath
	}

	return map[string]string{
		"PATH":            path,
		"HOME":            f.home,
		"XDG_CACHE_HOME":  filepath.Join(f.home, ".cache"),
		"XDG_CONFIG_HOME": filepath.Join(f.home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(f.home, ".local", "share"),
		"XDG_STATE_HOME":  filepath.Join(f.home, ".local", "state"),
		"TMPDIR":          f.tmp,
		"LANG":            "C.UTF-8",
		"TERM":            "dumb",
	}
}

// layout is the sandbox spec's paths for a clone, one output file and the resolved toolchain.
func (f sandboxFiles) layout(d Deps, clone, output string, toolchain []string) sandbox.Layout {
	configDir := filepath.Dir(d.Settings.ConfigFile)
	if configDir == "." {
		configDir = ""
	}

	hidden := []string{
		d.Settings.Home, d.Settings.WorkRoot, d.Settings.DaguHome, d.Settings.TokenDir,
		configDir, d.Settings.KeyFile, nixDaemonSocket,
	}

	return sandbox.Layout{
		RunDir: f.dir, Clone: clone, Scratch: scratchOf(output), Output: output, Skills: d.Settings.Skills,
		Toolchain: toolchain, Hidden: dedupe(hidden),
	}
}

// scratchOf is the run's scratch directory: the directory its output file sits in.
func scratchOf(output string) string {
	if output == "" {
		return ""
	}

	return filepath.Dir(output)
}

// sandboxedSession compiles the agent role's spec and puts s inside it. extra names any further
// output files the agent role writes outside the clone, such as its output file and its retro.
func (d Deps) sandboxedSession(
	s session, files sandboxFiles, role workflow.Role, clone, output string, extra []string,
	network sandbox.Network, toolchain, proxyAddr string,
) (session, error) {
	spec := sandbox.Plan(role, files.layout(d, clone, output, toolchainPaths(toolchain)))
	spec.ReadWrite = append(spec.ReadWrite, extra...)
	spec.Network = network

	return s.sandboxed(files, spec, toolchain, proxyPort(proxyAddr))
}

// sandboxedWithToolchain resolves the default branch's dev environment and puts s inside the sandbox
// that uses it.
func (d Deps) sandboxedWithToolchain(
	ctx context.Context, s session, j job, sec secrets, repo workflow.Repo, branch string, files sandboxFiles,
	role workflow.Role, clone, output string, extra []string, network sandbox.Network, proxyAddr string,
) (session, error) {
	toolchain, err := d.toolchain(ctx, j.c, repo, sec.githubToken, branch, files)
	if err != nil {
		return session{}, err
	}

	return d.sandboxedSession(s, files, role, clone, output, extra, network, toolchain, proxyAddr)
}

// writeSettings compiles and writes the run's srt settings file.
func (f sandboxFiles) writeSettings(spec sandbox.Spec, proxyPort int) error {
	raw, err := sandbox.Settings(spec, proxyPort)
	if err != nil {
		return err
	}

	if err := os.WriteFile(f.settings, raw, privateFileMode); err != nil {
		return fmt.Errorf("write srt settings: %w", err)
	}

	return nil
}

// dedupe drops empty and repeated paths, keeping the first occurrence's order.
func dedupe(paths []string) []string {
	out := make([]string, 0, len(paths))

	for _, path := range paths {
		if path != "" && !slices.Contains(out, path) {
			out = append(out, path)
		}
	}

	return out
}
