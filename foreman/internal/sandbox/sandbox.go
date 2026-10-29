// Package sandbox owns one OS-agnostic sandbox specification per agent role and compiles it to srt,
// the pinned backend. It is pure: it builds settings and command lines and never starts a process.
// The spec covers what the run may read and write, what it may reach on the network, and which paths
// stay hidden; foreman's shell does the I/O.
package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// Layout is every path one run's sandbox is built from, resolved by foreman outside the sandbox.
type Layout struct {
	// RunDir holds the run's HOME, XDG directories, TMPDIR, state and settings file. Deleted after
	// the run.
	RunDir string
	// Clone is the repository clone the model works in.
	Clone string
	// Scratch is the run's own scratch directory outside the clone, which holds its outputs. Empty
	// when the run has none.
	Scratch string
	// Output is the one result file the run may write. Empty when the agent role writes none.
	Output string
	// Skills is the factory's forked skills, read-only.
	Skills string
	// Toolchain is the dev environment resolved from the default branch's flake, read-only.
	Toolchain []string
	// Hidden is everything the run must not read: the real home, other runs, dagu's directories
	// and foreman's secrets.
	Hidden []string
}

// Spec is one agent role's sandbox: the OS-agnostic answer before any backend sees it.
type Spec struct {
	// ReadWrite is the paths the run may modify.
	ReadWrite []string
	// ReadOnly is the paths worth naming as readable but never writable.
	ReadOnly []string
	// Hidden is the paths the run must not read at all.
	Hidden []string
	// DenyWrite is paths inside ReadWrite that stay read-only, such as a clone's .git.
	DenyWrite []string
	// Network is the egress the run is allowed.
	Network Network
}

// Plan is one agent role's sandbox. The Implementer and the Reviewer may write their clone; the
// Planner, Researcher and Curator only read theirs. Every agent role writes its run directory and
// its one output file. A clone's .git is never writable inside the sandbox: foreman owns every
// commit.
func Plan(role workflow.Role, layout Layout) Spec {
	writableClone := role == workflow.Implementer || role == workflow.Reviewer

	readWrite := []string{layout.RunDir}
	if layout.Scratch != "" {
		readWrite = append(readWrite, layout.Scratch)
	}

	if writableClone {
		readWrite = append(readWrite, layout.Clone)
	}

	// The output sits in the run's scratch directory, which is already writable; name it only when
	// there is no scratch directory to cover it.
	if layout.Output != "" && layout.Scratch == "" {
		readWrite = append(readWrite, layout.Output)
	}

	readOnly := []string{}
	if layout.Skills != "" {
		readOnly = append(readOnly, layout.Skills)
	}

	readOnly = append(readOnly, layout.Toolchain...)
	denyWrite := []string{}

	// An agent role without a clone leaves the path empty; an empty path would otherwise become a
	// stray root-level entry.
	if layout.Clone != "" {
		if writableClone {
			denyWrite = append(denyWrite, filepath.Join(layout.Clone, ".git"))
		} else {
			readOnly = append(readOnly, layout.Clone)
		}
	}

	return Spec{
		ReadWrite: readWrite, ReadOnly: readOnly, Hidden: slices.Clone(layout.Hidden), DenyWrite: denyWrite,
	}
}

// hostRead is the operating system every run may still read. The sandbox hides the host by
// denying `/` and binding back only these paths (plus the run's own), so the list is the host's
// share, never the factory's: it holds the OS runtime, its device and process files, and /nix/store.
// Both Linux and macOS paths are listed; srt skips an entry that does not exist.
var hostRead = []string{ //nolint:gochecknoglobals // a read-only list
	// The toolchain foreman resolves from the default branch.
	"/nix/store",
	// The OS runtime and its shared libraries.
	"/usr", "/bin", "/sbin", "/lib", "/lib64",
	// /etc is the TLS certificate, DNS and NSS configuration a toolchain needs; the factory's own
	// /etc/factory-agent stays denied by name. On macOS /etc is a symlink to /private/etc.
	"/etc",
	// srt 0.0.81 leaves these visible even under denyRead: ["/"]; see docs/sandbox.md.
	"/dev", "/proc", "/sys",
	// macOS's runtime frameworks and its real /etc and /var. /private/var stays because macOS's
	// /var is a symlink to it and tools consult /var/run and the dynamic loader's /var/db; it holds
	// no factory data, and the factory's own paths under it stay in the deny list.
	"/System", "/private/etc", "/private/var",
}

// settings is the srt settings file's shape. Only the fields foreman sets are here.
type settings struct {
	Network    networkSettings    `json:"network"`
	Filesystem filesystemSettings `json:"filesystem"`
}

// networkSettings always names foreman's egress proxy: srt itself cannot express open egress, so
// the proxy enforces the allowlist and logs every domain the run contacts.
type networkSettings struct {
	AllowedDomains []string `json:"allowedDomains"`
	DeniedDomains  []string `json:"deniedDomains"`
	HTTPProxyPort  int      `json:"httpProxyPort"`
}

// filesystemSettings is srt's allow-write, deny-read model.
type filesystemSettings struct {
	AllowRead  []string `json:"allowRead"`
	AllowWrite []string `json:"allowWrite"`
	DenyRead   []string `json:"denyRead"`
	DenyWrite  []string `json:"denyWrite"`
}

// Settings compiles a spec to the JSON file srt reads. proxyPort is the run's egress proxy on
// loopback. A missing port is an error, never a run without an egress boundary.
func Settings(spec Spec, proxyPort int) ([]byte, error) {
	if proxyPort <= 0 {
		return nil, errors.New("srt settings: an egress proxy port is required")
	}

	// The host is hidden by default: srt's read model is deny-then-allow, and denying `/` denies
	// every directory in it. allowRead then binds back the OS paths in hostRead and the paths of
	// this run alone. The named hidden paths stay denied even though they sit under an allowed
	// region, which is the defence srt provides for a more specific deny.
	denyRead := append([]string{"/"}, spec.Hidden...)
	allowRead := append(append(slices.Clone(hostRead), spec.ReadOnly...), spec.ReadWrite...)

	raw, err := json.Marshal(settings{
		Network: networkSettings{
			AllowedDomains: []string{}, DeniedDomains: []string{}, HTTPProxyPort: proxyPort,
		},
		Filesystem: filesystemSettings{
			AllowRead:  nonNil(nonEmpty(allowRead)),
			AllowWrite: nonNil(nonEmpty(spec.ReadWrite)),
			DenyRead:   nonNil(nonEmpty(denyRead)),
			DenyWrite:  nonNil(nonEmpty(spec.DenyWrite)),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode srt settings: %w", err)
	}

	return raw, nil
}

// nonNil makes an empty path list an empty JSON array. srt refuses a null list rather than reading it
// as an empty one, and a refused settings file fails the run.
func nonNil(paths []string) []string {
	if paths == nil {
		return []string{}
	}

	return paths
}

// nonEmpty drops empty path entries. A spec built for a run without a skills directory or a clone
// would otherwise name "" as an allowed path, which srt refuses as an invalid config.
func nonEmpty(paths []string) []string {
	kept := make([]string, 0, len(paths))

	for _, path := range paths {
		if path != "" {
			kept = append(kept, path)
		}
	}

	return kept
}

// srtPrefixArgs is how many arguments precede the wrapped command: the binary, --settings, the
// settings path and --.
const srtPrefixArgs = 4

// Argv wraps command in srt with the run's settings. A missing backend, settings file or command
// yields nil: the caller fails the run rather than starting anything unsandboxed.
func Argv(srtBinary, settingsPath string, command []string) []string {
	if srtBinary == "" || settingsPath == "" || len(command) == 0 {
		return nil
	}

	argv := make([]string, 0, srtPrefixArgs+len(command))
	argv = append(argv, srtBinary, "--settings", settingsPath, "--")

	return append(argv, command...)
}

// supportedOS is the set srt serves: Seatbelt on macOS and bubblewrap on Linux.
var supportedOS = []string{"linux", "darwin"} //nolint:gochecknoglobals // a read-only set

// Supported admits a worker host the adapter can serve and refuses every other OS. It runs before
// any work, so an unsupported host fails closed rather than running unsandboxed.
func Supported(goos string) error {
	if slices.Contains(supportedOS, goos) {
		return nil
	}

	return fmt.Errorf("sandbox backend cannot serve %s: only Linux and macOS worker hosts can enroll", goos)
}

// Network is the run's egress policy. An empty allowlist is open egress, the default; a non-empty
// one permits exactly those hosts and their matching wildcard subdomains, and nothing else.
type Network struct {
	allowed []string
}

// ParseNetwork validates an allowlist from the agent role's configuration. An invalid entry fails
// the run: a typo must never widen egress.
func ParseNetwork(allowed []string) (Network, error) {
	seen := make([]string, 0, len(allowed))

	for _, entry := range allowed {
		if !validDomain(entry) {
			return Network{}, fmt.Errorf("invalid egress domain %q", entry)
		}

		normalized := strings.ToLower(entry)
		if !slices.Contains(seen, normalized) {
			seen = append(seen, normalized)
		}
	}

	return Network{allowed: seen}, nil
}

// validDomain accepts an exact host with at least one dot or a *.example.com wildcard whose base
// has at least two labels. Schemes, paths, ports and a bare * are refused, as srt refuses them.
func validDomain(entry string) bool {
	if entry == "" || strings.ContainsAny(entry, ":/ ") {
		return false
	}

	host := entry
	if strings.HasPrefix(entry, "*.") {
		host = entry[2:]
	} else if strings.Contains(entry, "*") {
		return false
	}

	labels := strings.Split(host, ".")

	return len(labels) >= 2 && !slices.Contains(labels, "")
}

// Open reports whether the run may reach any host.
func (n Network) Open() bool {
	return len(n.allowed) == 0
}

// Allowed is the validated allowlist, empty when egress is open.
func (n Network) Allowed() []string {
	return slices.Clone(n.allowed)
}

// Permits reports whether host is reachable under the policy.
func (n Network) Permits(host string) bool {
	if n.Open() {
		return true
	}

	host = strings.ToLower(host)

	return slices.ContainsFunc(n.allowed, func(pattern string) bool {
		if strings.HasPrefix(pattern, "*.") {
			return strings.HasSuffix(host, pattern[1:])
		}

		return host == pattern
	})
}
