# Sandboxes for agent role runs across host operating systems

Research for [Sandboxes for agent role runs across host operating systems](https://github.com/nateinaction/factory/issues/26),
under the map [Wayfinder: agent roles for the factory](https://github.com/nateinaction/factory/issues/4).
Researched 2026-10-27.

## Question

What can confine an agent role run on each kind of host the factory may run on?
Today that is Raspberry Pi OS (Debian). Later it may be macOS, Fedora Atomic and NixOS.
Is there an existing OS-agnostic policy format that compiles to each OS's mechanism?

[Which skills, permissions and step cap does each agent role get?](https://github.com/nateinaction/factory/issues/19)
decided that a sandbox does most of the tool restriction, and that its spec must be OS-agnostic.

## Answer in brief

- **Landlock is not available on today's hosts.** The Raspberry Pi kernel is built without it.
  Anything that relies on Landlock alone (nono, landrun, Codex's legacy mode) fails there.
- **Bubblewrap works on today's hosts.** Unprivileged user namespaces are on, and Debian 13 ships bubblewrap 0.12.0.
  Seatbelt (`sandbox-exec`) is the macOS counterpart. It is deprecated but still shipped, and every agent harness below uses it.
- **Two portable layers already compile one policy to both:**
  Anthropic's `sandbox-runtime` (`srt`, JSON settings) and Codex CLI's `codex sandbox` (TOML permissions profiles).
  Both use bubblewrap + seccomp on Linux and Seatbelt on macOS, both filter egress through a host-side proxy,
  and both ship arm64 builds. nono has a third format, but its Linux backend is Landlock.
- **The OpenRouter key can stay outside.** `srt` can mask a secret inside the sandbox and substitute it on egress.
  A plain host-side forwarding proxy that adds the key also works; Codex ships one for OpenAI (`codex-responses-api-proxy`).
- **Resolve the Nix toolchain outside the sandbox.** Inside, the run only needs `/nix/store` read-only.
  Letting the sandbox reach the Nix daemon socket would also give it the daemon's network access.
- **opencode v1.19.3 has no sandbox of its own and needs only a few writable paths.** Nobody has reported a test of it inside `srt` or bubblewrap. That is unconfirmed.

## Facts measured on today's worker

Read-only probe of factory-2 on 2026-10-27.

| Item | Value |
| --- | --- |
| Kernel | `6.18.50+rpt-rpi-v8`, `aarch64` |
| OS | Debian GNU/Linux 13 (trixie) |
| Active LSMs (`/sys/kernel/security/lsm`) | `capability` only |
| `/boot/config-*` | `# CONFIG_SECURITY_LANDLOCK is not set`, `CONFIG_LSM=""`, `CONFIG_SECCOMP=y`, `CONFIG_USER_NS=y` |
| Unprivileged user namespaces | `unshare -Urn true` succeeds; `user.max_user_namespaces` = 29274 |
| cgroup v2 controllers | `cpuset cpu io pids` (no `memory`; the boot command line has `cgroup_disable=memory`) |
| Candidates in apt | `bubblewrap 0.12.0-1~deb13u1`, `podman 5.4.2`, `systemd-container 257.13` (none installed) |
| Nix | Determinate Nix 3.23.3 (Nix 2.35.3), multi-user |

The Raspberry Pi defconfigs agree: `bcm2711_defconfig` and `bcm2712_defconfig` on `rpi-6.12.y`,
and `bcm2711_defconfig` on `rpi-6.18.y`, set `CONFIG_SECURITY_APPARMOR=y` and `CONFIG_LSM=""` and leave Landlock out
([raspberrypi/linux](https://github.com/raspberrypi/linux/tree/rpi-6.18.y/arch/arm64/configs)).

Other kernels for comparison:

- Fedora: `CONFIG_SECURITY_LANDLOCK=y` and `CONFIG_LSM="lockdown,yama,integrity,selinux,bpf,landlock,ipe"`
  ([kernel-ark common](https://gitlab.com/cki-project/kernel-ark/-/raw/os-build/redhat/configs/common/generic/CONFIG_SECURITY_LANDLOCK),
  [kernel-ark fedora](https://gitlab.com/cki-project/kernel-ark/-/raw/os-build/redhat/configs/fedora/generic/CONFIG_LSM)).
  Fedora Atomic uses the same kernel package (unconfirmed, but it is the Fedora kernel).
- NixOS: `SECURITY_LANDLOCK = yes` in nixpkgs `pkgs/os-specific/linux/kernel/common-config.nix`.
  Whether it is in the active LSM list at boot is unconfirmed.

## Linux mechanisms

### Bubblewrap

- A tool for building sandboxes, not a sandbox. "bubblewrap is not a complete, ready-made sandbox with a specific security policy."
  The policy is the argument list. There is no config file ([README](https://github.com/containers/bubblewrap/blob/main/README.md)).
- Needs unprivileged user namespaces, which factory-2 has. The setuid mode has been removed (README).
- Filesystem: `--ro-bind`, `--bind`, `--tmpfs`, masking a path with `/dev/null`. A read-only view of `/` with writable binds on top is the common pattern.
- Network: `--unshare-net` gives a namespace with only loopback. No host allowlist. Egress has to go through a proxy reached over a bound Unix socket.
- Process: `--unshare-pid`, `--die-with-parent`, `--new-session`, and a seccomp filter passed by file descriptor.
- arm64: packaged for Debian 13 arm64 (above). Fedora Atomic ships it (Flatpak depends on it; unconfirmed for the exact image). nixpkgs has it.
- Inside another user namespace (a nested sandbox, rootless podman) it may fail. srt documents both ways this breaks (see below).

### Landlock

- Unprivileged self-restriction. "Empowers any process, including unprivileged ones, to securely restrict themselves" ([kernel docs](https://docs.kernel.org/userspace-api/landlock.html)).
- Filesystem rights since ABI 1 (Linux 5.13). TCP bind/connect by **port** since ABI 4 (Linux 6.7). Abstract Unix socket and signal scoping since ABI 6 (Linux 6.12). UDP rights later ([landlock(7)](https://man7.org/linux/man-pages/man7/landlock.7.html)).
- No hostname or IP rules. Network rules are ports only, so a host allowlist still needs a proxy.
- **Not usable on the Pi hosts today**: the kernel is built without it (measured above). Usable on Fedora, and probably on NixOS.

### systemd-nspawn

- "The full functionality is currently only available when invoked with privileges."
  Unprivileged use supports only disk images (`--image=`) or trees owned by the foreign UID range,
  and needs `systemd-mountfsd` and `systemd-nsresourced` ([systemd-nspawn(1)](https://www.freedesktop.org/software/systemd/man/latest/systemd-nspawn.html)).
- Network: `--private-network` (loopback only) or `--network-veth`. No host allowlist.
- It boots or runs a whole OS tree. That is heavy for a per-run sandbox, and it is Linux only.
- Related: systemd unit sandboxing (`ProtectSystem=`, `ReadWritePaths=`, `PrivateNetwork=`, `IPAddressAllow=`).
  `IPAddressAllow=`/`IPAddressDeny=` filter by IP, not hostname, and need eBPF cgroup support
  ([systemd.resource-control(5)](https://www.freedesktop.org/software/systemd/man/latest/systemd.resource-control.html)).
  `MemoryMax=` needs the memory controller, which the Pi boot line disables.

### Podman

- Rootless OCI containers. Debian 13 arm64 offers 5.4.2 (measured). Fedora Atomic includes it (unconfirmed for the exact image). nixpkgs has it.
- Filesystem: volumes, `:ro`, read-only rootfs. Network: `--network=none` or a custom network. No hostname allowlist built in; a proxy is still needed.
- Process: PID namespace, `--pids-limit`, seccomp profile. `--memory` needs the memory controller, which the Pi lacks.
- Policy is a run command or a Containerfile/compose file. It is OS-agnostic only by running a Linux VM on macOS (`podman machine`).
- Toolchain: the image has to carry Nix, or the host's `/nix` must be mounted in.
- Rootless podman cannot start inside `srt` on Linux without loosening it (srt README, "allowNestedUserNamespaces").

## macOS mechanisms

### Seatbelt (`sandbox-exec`)

- The man page says "The sandbox-exec command is DEPRECATED" and points app developers to App Sandbox (`man sandbox-exec`, macOS 27.2).
  It is still shipped. Codex hard-codes `/usr/bin/sandbox-exec` (`codex-rs/sandboxing/src/seatbelt.rs`), and `srt` and Gemini CLI use it too.
- The policy is an SBPL profile string. Filesystem read/write by path or subpath, network outbound by host:port (in practice "only localhost:proxy-port"), Mach lookups, process exec.
- Native on Apple silicon. The profile language is undocumented by Apple (unconfirmed beyond the man page's "DEPRECATED").
- Known holes the harnesses call out: Go TLS needs `com.apple.trustd.agent`, and Apple Events let a child launch apps outside the sandbox (srt README, "Security Limitations").

### Containers or VMs

- Apple `container`: Linux containers as "lightweight virtual machines". Needs Apple silicon and macOS 26 ([README](https://github.com/apple/container/blob/main/README.md)).
- `podman machine` runs a Linux VM, then podman inside it (unconfirmed detail of the backend).
- Either way, the agent runs on Linux inside the VM, so the Linux policy applies there. The host's `/nix` is a macOS store and cannot be shared into a Linux VM.

## Portable layers used by agent harnesses

### Anthropic `sandbox-runtime` (`srt`), v0.0.81

Source: [README at v0.0.81](https://github.com/anthropic-experimental/sandbox-runtime/blob/v0.0.81/README.md).

- "Beta Research Preview", developed for Claude Code. "APIs and configuration formats may evolve."
- **Backends**: `sandbox-exec` on macOS; bubblewrap plus a seccomp helper on Linux; a dedicated user plus WFP filters on Windows (alpha).
- **One JSON settings file** for every OS: `filesystem.{denyRead, allowRead, allowWrite, denyWrite}`, `network.{allowedDomains, deniedDomains, allowUnixSockets, allowLocalBinding}`, `credentials`, and more.
  A settings file that is empty or invalid is an error, not a fallback (fail closed).
- **Filesystem**: reads allowed by default, deny-then-allow. Writes denied by default, allow-only. Some files are always write-protected ("Mandatory Deny Paths").
- **Network**: denied by default, allowlist by domain with optional `:port`. On Linux the namespace has no network; HTTP and SOCKS5 proxies on the host are reached over bound Unix sockets via `socat`. On macOS the profile allows only the proxy's localhost port.
  Allowed names that resolve to loopback, link-local or the host's own addresses are refused unless the IP literal is allowlisted.
  `network.parentProxy` chains to another proxy.
- **Processes**: nested PID namespace; seccomp blocks new Unix sockets and new user or mount namespaces.
  So a nested bubblewrap, rootless podman or `unshare` inside fails unless `allowNestedUserNamespaces` is set.
  `allowUnixSockets` is ignored on Linux ("seccomp can't filter by path").
- **Secrets**: `credentials.envVars` and `credentials.files`. `mode: "deny"` withholds the value. `mode: "mask"` puts a sentinel inside and the proxy substitutes the real value on egress to `injectHosts`.
  Masking needs `network.tlsTerminate` (the proxy terminates TLS and the sandbox trusts its CA via `NODE_EXTRA_CA_CERTS` and similar).
  Files are masked on Linux only; macOS denies them instead.
- **arm64**: prebuilt static `apply-seccomp` for x64 and arm64. Linux needs `bubblewrap` (0.4.0+, 0.5.0+ for some masks), `socat` and `ripgrep`.
- **Install**: an npm package. Install it outside every writable path, or a sandboxed command can rewrite it.
- **Root callers** need `CAP_SETFCAP`; the README prefers a non-root caller. The factory already runs agents as the unprivileged `factory` user.
- **Known gap**: on Linux, traffic is steered by `HTTP_PROXY` and friends. A program that ignores them cannot reach the network at all, rather than leaking.

### Codex CLI sandbox, rust-v0.165.0

Source: [openai/codex at rust-v0.165.0](https://github.com/openai/codex/tree/rust-v0.165.0/codex-rs).

- **Backends**: bubblewrap plus seccomp on Linux (system `bwrap` if on PATH, else a bundled one); Seatbelt on macOS; a Windows sandbox. Landlock is "legacy" and is rejected for filesystem-restricted policies (`codex-rs/linux-sandbox/README.md`).
- **Runs any command**: `codex sandbox -P <profile> -- <cmd>`. The host OS picks the backend (`codex-rs/cli/src/main.rs`, `codex-rs/cli/src/lib.rs`).
- **One TOML format**: named `[permissions.<name>]` profiles in `config.toml`, with filesystem entries (`write`, read-only, `none`, globs) and a `network` table (`codex-rs/network-proxy/README.md`).
- **Filesystem**: `--ro-bind / /`, writable roots bound on top, `.git` and similar re-protected read-only, unreadable globs masked.
- **Network**: `--unshare-net`. In proxy mode a TCP→Unix socket→TCP bridge reaches only the managed HTTP and SOCKS5 proxies, which enforce a host allow/deny list. Seccomp blocks network syscalls when network is off, and always blocks `io_uring`.
- **Processes**: `--unshare-user`, `--unshare-pid`, fresh `/proc`, `PR_SET_NO_NEW_PRIVS`.
- **Secrets**: `codex-responses-api-proxy` holds the API key in a separate process run by a privileged user, reads the key from stdin, and forwards only `POST /v1/responses` to OpenAI (`codex-rs/responses-api-proxy/README.md`).
  It speaks the OpenAI Responses API only, so it does not fit OpenRouter as is.
- **arm64**: release assets include `codex-aarch64-unknown-linux-musl`, `codex-aarch64-apple-darwin` and `bwrap-aarch64-unknown-linux-musl`.
- **Fit**: the profile format belongs to Codex's config stack. Using it means installing the Codex binary only to wrap opencode. Whether `codex sandbox` is stable as a general wrapper is unconfirmed.

### nono, v0.80.0

Source: [nolabs-ai/nono at v0.80.0](https://github.com/nolabs-ai/nono/tree/v0.80.0).

- Composable JSON profiles, a profile registry, credential injection by proxy ("Keep API keys outside the agent's sandbox with proxy-based injection"), and an L7 network filter (README).
- Names OpenCode among supported agents (README). A commenter in [opencode issue 2242](https://github.com/anomalyco/opencode/issues/2242) reports using it with opencode.
- **Linux backend is Landlock**: `crates/nono/src/sandbox/linux.rs` errors with "Landlock not available. Requires Linux kernel 5.13+ with Landlock enabled."
  So it **cannot run on today's Pi hosts**. macOS uses Seatbelt (`crates/nono/src/sandbox/macos.rs`).
- arm64 builds for Linux (`.deb`, `.rpm`, tarball) and macOS. Installable with `nix profile add github:nolabs-ai/nono`.

### Gemini CLI

Source: [docs/cli/sandbox.md](https://github.com/google-gemini/gemini-cli/blob/main/docs/cli/sandbox.md).

- macOS: six built-in Seatbelt profiles (`permissive-open` … `strict-proxied`). `-proxied` routes network through a proxy.
- Linux: Docker or Podman, gVisor, or LXC. No native bubblewrap or Landlock option.
- No single policy format across OSes: a profile name on macOS, container flags elsewhere.

### opencode itself

- `SECURITY.md` at v1.19.3: "OpenCode does **not** sandbox the agent." The permission system "is not designed to provide security isolation."
- Two sandbox pull requests were closed unmerged (comment in [opencode issue 2242](https://github.com/anomalyco/opencode/issues/2242); unconfirmed beyond that comment).
- In opencode's code, "sandbox" means a git worktree, not an OS sandbox (`packages/opencode/src/project/project.ts`).

## Getting the toolchain in with Nix

- `nix develop --profile <path>` records a flake's dev environment into a profile, and `nix develop <path>` reuses it.
  `nix print-dev-env --json` prints its variables and shell functions ([nix develop](https://nix.dev/manual/nix/stable/command-ref/new-cli/nix3-develop), [nix print-dev-env](https://nix.dev/manual/nix/stable/command-ref/new-cli/nix3-print-dev-env)).
  The manual describes this as building a derivation that records the environment, which implies the inputs get realised. The manual does not state that directly.
- **Run it outside the sandbox.** Inside, give the run `/nix/store` read-only plus the recorded environment.
  Reasons:
  - Evaluating and building talks to the Nix daemon socket. The daemon fetches and builds outside any agent sandbox, so a reachable socket is an egress path the agent's proxy does not see (inference from how the daemon works; not tested).
  - `srt` on Linux blocks new Unix sockets with seccomp and ignores per-path socket allowlists, so `nix develop` would fail inside it anyway (inference from the srt README).
- Per OS:
  - Raspberry Pi OS: Determinate Nix is installed today (measured).
  - macOS: Determinate's installer supports Apple silicon ([nix-installer README](https://github.com/DeterminateSystems/nix-installer/blob/main/README.md)). The store is a separate APFS volume; Seatbelt can allow reading it (unconfirmed by test).
  - Fedora Atomic: the installer "supports SELinux and OSTree-based distributions" (same README). Silverblue/Atomic is not named.
  - NixOS: native.
- **Mismatch to watch**: inside a Linux VM on macOS (Apple `container`, `podman machine`), the host's macOS store is useless. The VM needs its own Nix.

## opencode v1.19.3 inside a sandbox

Read from the source at tag `v1.19.3`. Nothing here was run in a sandbox; every "works" below is unconfirmed.

- **Binary**: a Bun-compiled single file, `opencode-linux-arm64` today. Bun-specific needs inside bubblewrap (`/dev/shm`, `/proc`) are untested.
- **Writable paths**: `$XDG_DATA_HOME/opencode`, `$XDG_CACHE_HOME/opencode` (with `bin/`), `$XDG_CONFIG_HOME/opencode`, `$XDG_STATE_HOME/opencode` and `$TMPDIR/opencode` are all created at startup (`packages/core/src/global.ts`). Plus the repo clone.
  Pointing the XDG variables at a per-run directory keeps sessions out of the shared home. The run still needs the skills directory readable.
- **`opencode run` needs no listening port**: the non-interactive mode uses an in-process server (`packages/opencode/src/cli/cmd/run.ts`).
  (opencode v2 crashes when `~/.config/opencode` is read-only, per [opencode issue 56359](https://github.com/anomalyco/opencode/issues/56359). That is v2, not the pinned v1.19.3.)
- **Startup network calls to remove or allow**:
  - Model catalogue from `https://models.opencode.ai`. Off with `OPENCODE_DISABLE_MODELS_FETCH`, or point `OPENCODE_MODELS_PATH` at a file (`packages/core/src/models-dev.ts`).
  - `ripgrep` is downloaded from GitHub only if `rg` is not on PATH (`packages/core/src/ripgrep/binary.ts`).
  - LSP servers download unless `OPENCODE_DISABLE_LSP_DOWNLOAD` is set.
  - Autoupdate is already off in the factory's config.
- **Proxy support**: opencode "respects standard proxy environment variables" and `NODE_EXTRA_CA_CERTS` (`packages/web/src/content/docs/network.mdx`). That is what `srt` and Codex rely on.
- **Model endpoint**: any provider's `options.baseURL` can be overridden (`packages/web/src/content/docs/providers.mdx`). So opencode can talk to a local key-adding proxy instead of `openrouter.ai`.

## Keeping the OpenRouter key outside

Today `finances-agent` passes `OPENROUTER_API_KEY` in opencode's environment (`roles/agent/files/finances-agent`), so the model's tools can read it.
Three ways to keep it out, none tested:

1. **srt mask**: list the variable under `credentials.envVars` with `mode: "mask"` and `injectHosts: ["openrouter.ai"]`. Needs `tlsTerminate`, so opencode must trust srt's CA (`NODE_EXTRA_CA_CERTS`). macOS and Linux.
2. **Host-side forwarding proxy**: a small process outside the sandbox holds the key, accepts plain HTTP from the sandbox, adds `Authorization`, and forwards only to OpenRouter's API.
   opencode's `baseURL` points at it. Codex's `codex-responses-api-proxy` is the same pattern for OpenAI.
   Under `srt`, the sandbox reaches it as an allowlisted loopback IP literal through srt's proxy (srt refuses loopback otherwise).
3. **Upstream proxy chain**: `srt`'s `network.parentProxy` forwards allowed traffic to a proxy that injects the key. Needs TLS interception at that hop.

Each still lets the agent spend the key's budget through the allowed endpoint. Only the key itself stays out.

## Comparison

"Pi" means Raspberry Pi OS 13 arm64 as measured. ✓ = documented in a primary source. ~ = partial or with a caveat. ✗ = not available. ? = unconfirmed.

| Mechanism | Pi OS | macOS | Fedora Atomic | NixOS | Filesystem | Network egress | Process | Secret outside | arm64 | Toolchain via Nix |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| bubblewrap (raw) | ✓ (userns on, pkg 0.12.0) | ✗ | ✓ ? | ✓ | ro `/`, writable binds, masks | none or loopback only; proxy via bound socket | PID/user ns, seccomp by fd | ~ by omission; proxy needed | ✓ | ✓ ro `/nix/store` |
| Landlock (raw) | ✗ (not built) | ✗ | ✓ | ✓ ? | path rights | TCP/UDP **ports** only | signal and abstract-socket scoping | ~ by omission | ✓ | ✓ |
| systemd-nspawn | ~ (root or images) | ✗ | ~ | ~ | OS tree or image | private net or veth | full container | ~ | ✓ | ~ needs `/nix` in tree |
| systemd unit sandboxing | ~ (system manager) | ✗ | ✓ | ✓ | `ProtectSystem`, `ReadWritePaths` | IP allow/deny, no hostnames | `PrivateTmp`, `NoNewPrivileges`; no memory limit on Pi | ~ | ✓ | ✓ |
| podman rootless | ✓ (pkg 5.4.2) | ~ via VM | ✓ ? | ✓ | volumes, ro rootfs | none or custom net; proxy needed | container, `--pids-limit` | ~ | ✓ | ~ Nix in image or `/nix` mount |
| Seatbelt (raw) | ✗ | ✓ deprecated | ✗ | ✗ | path read/write rules | host:port rules; proxy port in practice | exec and Mach rules | ~ | ✓ | ✓ read `/nix` |
| Apple `container` | ✗ | ✓ macOS 26, Apple silicon | ✗ | ✗ | VM disk + mounts | VM network | VM | ~ | ✓ | ~ own Nix in VM |
| `srt` v0.0.81 | ✓ ? (needs bwrap, socat, rg) | ✓ | ✓ ? | ✓ ? | JSON allow/deny read and write | domain allowlist via host proxy | nested PID ns, seccomp, no nested userns | ✓ mask or deny | ✓ x64 and arm64 seccomp | ✓ outside; daemon socket blocked inside |
| `codex sandbox` 0.165.0 | ✓ ? (bundled bwrap) | ✓ | ✓ ? | ✓ ? | TOML profiles, globs, protected `.git` | host allowlist via managed proxy | user/PID ns, seccomp, no `io_uring` | ~ separate OpenAI-only proxy | ✓ | ✓ outside ? |
| nono 0.80.0 | ✗ (Landlock) | ✓ | ✓ | ✓ ? | JSON profiles | proxy, L7 filter | ? | ✓ proxy injection | ✓ | ? |
| Gemini CLI sandbox | ~ containers only | ✓ fixed profiles | ~ containers | ~ containers | per profile | `-open` or `-proxied` | container or Seatbelt | ✗ documented | ? | ✗ documented |

Is there an OS-agnostic policy format that compiles to each OS? Yes, two in active use:
`srt`'s JSON settings and Codex's TOML permissions profiles. Both compile to bubblewrap + seccomp and Seatbelt.
nono's JSON profiles also do, but only to Landlock on Linux. No vendor-neutral standard exists; each format belongs to its tool.
OCI (podman) is portable only by running Linux everywhere.

## Implications for the grilling ticket

Options for [What sandbox does an agent role run in, and how does it get a repository's toolchain?](https://github.com/nateinaction/factory/issues/27), without a decision:

- **Policy format**
  - Adopt `srt`'s JSON settings as the factory's sandbox spec. Works on Pi today. It is a research preview, so pin the version.
  - Adopt Codex's TOML permissions profiles and install the Codex binary only as a wrapper.
  - Write a small factory-owned spec and compile it to `srt` settings now, other backends later.
  - Wait for Landlock on the Pi (a custom kernel or a future Pi kernel) and use nono. Not possible today.
- **What runs inside**
  - The whole `opencode run` inside the sandbox (simplest boundary; needs the five XDG paths writable).
  - Only opencode's tool calls inside, opencode outside. opencode has no hook for this at v1.19.3, so it means patching or a plugin.
- **The OpenRouter key**
  - `srt` credential masking with TLS termination.
  - A factory-owned forwarding proxy and a `baseURL` override in the opencode config.
  - Either way, the key leaves opencode's environment in `finances-agent`.
- **Toolchain**
  - Resolve the dev environment with `nix develop --profile` or `nix print-dev-env` before entering the sandbox, then mount `/nix/store` read-only.
  - Or allow the Nix daemon socket inside and accept the extra egress path.
- **Other hosts**
  - macOS hosts use Seatbelt through the same spec. Fedora Atomic and NixOS use bubblewrap through it, with Landlock as an optional extra.
  - Container or VM hosts (podman, Apple `container`) only if per-host Linux everywhere is wanted.
- **Limits the sandbox will not give on Pi**
  - No memory cap (no memory controller). PIDs can be capped. The step cap and a wall-clock timeout remain the run's main bounds.
- **Test before deciding**
  - Install bubblewrap, socat and ripgrep on factory-2, run `opencode run` under `srt`, and confirm Bun, the XDG paths and the proxy work on arm64.

## Sources

- factory-2 probe (2026-10-27): `uname`, `/etc/os-release`, `/sys/kernel/security/lsm`, `/boot/config-6.18.50+rpt-rpi-v8`, `unshare -Urn`, `apt-cache policy`, `/sys/fs/cgroup/cgroup.controllers`, `nix --version`.
- [raspberrypi/linux arm64 defconfigs](https://github.com/raspberrypi/linux/tree/rpi-6.18.y/arch/arm64/configs)
- [kernel-ark (Fedora kernel) configs](https://gitlab.com/cki-project/kernel-ark/-/tree/os-build/redhat/configs)
- [nixpkgs kernel common-config.nix](https://github.com/NixOS/nixpkgs/blob/master/pkgs/os-specific/linux/kernel/common-config.nix)
- [Landlock userspace API](https://docs.kernel.org/userspace-api/landlock.html), [landlock(7)](https://man7.org/linux/man-pages/man7/landlock.7.html)
- [bubblewrap README](https://github.com/containers/bubblewrap/blob/main/README.md)
- [systemd-nspawn(1)](https://www.freedesktop.org/software/systemd/man/latest/systemd-nspawn.html), [systemd.resource-control(5)](https://www.freedesktop.org/software/systemd/man/latest/systemd.resource-control.html)
- `sandbox-exec(1)` man page, macOS 27.2
- [apple/container README](https://github.com/apple/container/blob/main/README.md)
- [anthropic-experimental/sandbox-runtime v0.0.81](https://github.com/anthropic-experimental/sandbox-runtime/tree/v0.0.81)
- [openai/codex rust-v0.165.0](https://github.com/openai/codex/tree/rust-v0.165.0)
- [nolabs-ai/nono v0.80.0](https://github.com/nolabs-ai/nono/tree/v0.80.0)
- [Gemini CLI sandbox docs](https://github.com/google-gemini/gemini-cli/blob/main/docs/cli/sandbox.md)
- [anomalyco/opencode v1.19.3](https://github.com/anomalyco/opencode/tree/v1.19.3): `SECURITY.md`, `packages/core/src/global.ts`, `packages/core/src/models-dev.ts`, `packages/core/src/ripgrep/binary.ts`, `packages/opencode/src/cli/cmd/run.ts`, `packages/web/src/content/docs/network.mdx`, `packages/web/src/content/docs/providers.mdx`
- [opencode issue 2242](https://github.com/anomalyco/opencode/issues/2242), [opencode issue 56359](https://github.com/anomalyco/opencode/issues/56359)
- [Determinate nix-installer README](https://github.com/DeterminateSystems/nix-installer/blob/main/README.md)
- [nix develop](https://nix.dev/manual/nix/stable/command-ref/new-cli/nix3-develop), [nix print-dev-env](https://nix.dev/manual/nix/stable/command-ref/new-cli/nix3-print-dev-env)
