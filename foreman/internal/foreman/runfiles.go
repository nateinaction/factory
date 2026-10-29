package foreman

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/nateinaction/factory/foreman/internal/sandbox"
	"github.com/nateinaction/factory/foreman/internal/telemetry"
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
	// dir holds the run's HOME, XDG directories, TMPDIR, state and the settings file. The run's
	// scratch is not here: it sits in the work root beside the output file (see runPaths).
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

// runDirName is the name of the agent role's run directory for an issue, under the work root.
func runDirName(role workflow.Role, number int) string {
	return fmt.Sprintf("run-%s-%d", role, number)
}

// toolchainDirName is the name of the dev environment clone of the agent role's run for an issue.
func toolchainDirName(role workflow.Role, number int) string {
	return fmt.Sprintf("toolchain-%s-%d", role, number)
}

// newSandboxFiles clears and creates one run's directories. A re-run starts from nothing.
func newSandboxFiles(root string, role workflow.Role, number int) (sandboxFiles, error) {
	dir, err := freshPath(root, runDirName(role, number))
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
	if f.toolchain, err = freshPath(root, toolchainDirName(role, number)); err != nil {
		return sandboxFiles{}, err
	}

	return f, nil
}

// remove deletes the run directory and the toolchain clone.
func (f sandboxFiles) remove() error {
	return errors.Join(os.RemoveAll(f.dir), os.RemoveAll(f.toolchain))
}

// db is the run's opencode state database, in the run directory so the retro can resume the run's
// session by id and the run's state is gone with that directory afterwards.
func (f sandboxFiles) db() string {
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

// resolveToolchain is the default branch's dev environment, or empty for an agent role that
// prepared none.
func (rc runCtx) resolveToolchain(ctx context.Context, files sandboxFiles, env runEnv, prep prepared) (string, error) {
	if prep.noToolchain {
		return "", nil
	}

	spans := rc.spans()

	ctx, span := spans.start(ctx, telemetry.SpanToolchain)
	defer span.End()

	path, err := rc.d.toolchain(
		ctx, rc.j.c, rc.spec.ref.Repo, env.sec.githubToken, prep.toolchainBranch, files, spans,
	)
	if err != nil {
		markFailureOf(span, err)
	}

	return path, err
}

// sandboxed puts the run's clone inside its own sandbox: the default branch's dev environment, the
// settings compiled from the agent role's spec, and the proxies the model may use. The agent role's
// prepared output files join the run's own.
func (rc runCtx) sandboxed(ctx context.Context, files sandboxFiles, env runEnv, prep prepared) (session, error) {
	toolchain, err := rc.resolveToolchain(ctx, files, env, prep)
	if err != nil {
		return session{}, err
	}

	spec := sandbox.Plan(rc.role, files.layout(rc.d, rc.paths.workdir, rc.paths.out, toolchainPaths(toolchain)))
	spec.ReadWrite = append(spec.ReadWrite, rc.writes(prep)...)
	spec.Network = env.network

	return rc.s.sandboxed(files, spec, toolchain, env.egressPort)
}

// writes is every output file the run writes outside its clone: the agent role's own, and its retro's
// when the agent role keeps one.
func (rc runCtx) writes(prep prepared) []string {
	return dedupe(append(slices.Clone(prep.extraWrite), rc.retroFile()))
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
