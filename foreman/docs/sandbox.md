# The sandbox for agent role runs

foreman owns one OS-agnostic spec per agent role (`foreman/internal/sandbox`): what the run may read
and write, what it may reach, and which paths stay hidden. A sandbox adapter compiles that spec to
**srt** (`@anthropic-ai/sandbox-runtime`, `sandbox-runtime` in nixpkgs), pinned at **0.0.81**. srt
uses Seatbelt on macOS and bubblewrap on Linux. There are no containers and no VMs.

The whole `opencode run` — retry attempts and the retro included — runs under srt. foreman stays
outside and does every clone, fetch, commit, push and GitHub write itself, running git on the host.
The model's `git` can read the clone but cannot write `.git`, so the code it runs never becomes a
commit outside the sandbox. Two secrets are inside the sandbox: the read-only installation token, as `GH_TOKEN`, and the
per-run proxy token, which is opencode's `provider.openrouter.options.apiKey`. The proxy token is
random for each run, is worth nothing outside this run's two loopback proxies, and never reaches a
file or a log. The token file the installation token was minted from stays in the deny list, and the
OpenRouter key itself never enters the sandbox.

## Filesystem

| Access | What |
|---|---|
| Read-write | the clone's working tree (Implementer and Reviewer); the run's scratch/output directory; the run's own `HOME` and XDG directories |
| Read-only | the forked skills; `/nix/store`; the dev environment resolved from the default branch; `.git` |
| Not visible | the real home (`~/.ssh` included), the work root and so every other run and every other run's scratch directory, dagu's home, the token directory, the key file and `/etc/factory-agent` |

The Reviewer gets a throwaway writable clone under the work root. The Planner, Researcher and Curator
get a clone they can only read. `HOME`, `XDG_CACHE_HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`,
`XDG_STATE_HOME` and `TMPDIR` are created under `<work root>/run-<role>-<issue>/` and deleted with the
run and its retro. Only `/nix/store` is shared between runs, read-only.

### Hide the host, then allow the operating system

srt's read model is deny-then-allow: reads are open unless denied. foreman compiles every run with
`denyRead: ["/"]`, which srt 0.0.81 expands to every directory in `/` (it exempts `/proc`, `/dev` and
`/sys`; see the residual visibility below). `allowRead` then binds back only two things:

- the host's own share in `hostRead` (`foreman/internal/sandbox`): `/nix/store`, `/usr`, `/bin`,
  `/sbin`, `/lib`, `/lib64`, `/etc`, `/dev`, `/proc`, `/sys`, and on macOS `/System`, `/private/etc`
  and `/private/var`. These let a process start and use the toolchain; they hold no factory or run
  data. `/private/var` stays because macOS's `/var` is a symlink to it and tools consult `/var/run`
  and the dynamic loader's `/var/db`; the factory's own paths under it stay in the deny list.
  `/opt` and `/Library` were dropped: the factory's toolchain comes from `/nix/store`, so neither
  is needed to start a process.
- this run's paths: its run directory, its clone (read-only for the roles that never commit), the
  forked skills, the resolved toolchain, and its scratch/output directory.

The named sensitive paths stay in `denyRead` as well. srt keeps a deny that is more specific than an
allow, so `/etc/factory-agent` stays hidden even though `/etc` is readable. Another run is hidden by
the work root's deny: its run directory and its `scratch-<role>-<issue>` directory are nowhere in
this run's `allowRead`, so neither is reachable. The two-run acceptance spec proves it with a real
`srt` and two real directories.

### Residual visibility in srt 0.0.81

srt cannot hide quite everything, and this file records the gap rather than claiming otherwise:

- `denyRead: ["/"]` still leaves **`/proc`, `/dev` and `/sys`** visible on Linux and macOS. On Linux
  srt starts bubblewrap with `--unshare-pid`, so `/proc` holds only the sandbox's own processes and no
  host process's command line or environment. `/dev` and `/sys` expose device nodes and kernel
  metadata.
- `/etc` is readable because a toolchain needs TLS certificates, DNS and NSS configuration. Any
  world-readable host configuration there (`/etc/hostname`, `/etc/machine-id`, `/etc/passwd`) is
  therefore visible. `/etc/factory-agent`, the `/etc/github-app` credential directories (mode 0700
  and 0600, owned by dagu) and the OpenRouter key file remain denied where the factory can name
  them; the token directory is denied by name.
- On macOS `/private/var` is readable for `/var/run` and the dynamic loader's `/var/db`. Its
  world-readable logs, databases and temporary files are therefore visible, while the factory's
  own paths under it stay in the deny list.
- A path under a symlinked parent is bound where it really is. macOS's `/tmp` is a symlink to
  `/private/tmp`, so an accepted run directory is a real path, not one reached through `/tmp`; the
  acceptance test resolves its temp directory for this reason.

The concrete residual risk is therefore a prompt-injected run reading **host system files that are
world-readable under `/etc`, `/dev`, `/proc`, `/sys` or, on macOS, `/private/var`** — for example
`/etc/machine-id` — but not the
factory's secrets, another run's clone or scratch directory, the real home, dagu's directories or the
token directory. Those stay in the explicit deny list; the list is the mitigation, not a claim that
`denyRead: ["/"]` alone is one.

## Toolchain

foreman resolves the dev environment outside the sandbox from a **shallow clone of the default
branch**, not from the run's clone:

```sh
git clone --depth 1 --branch <default> https://github.com/<repo>.git <work root>/toolchain-<role>-<issue>
nix print-dev-env --json --option accept-flake-config false --no-write-lock-file \
  --extra-experimental-features "nix-command flakes" --flake path:<toolchain dir>
```

The resulting `PATH` is the run's toolchain, and its `/nix/store` paths are readable read-only inside
the sandbox. A change to `flake.nix` on the working branch has no effect until it merges. Nix's
daemon socket is in the deny list, and no Nix process runs inside the sandbox. A missing flake or a
failed evaluation fails the run closed.

## Egress, and what srt cannot express

Egress is open by default. A human can set an allowlist per agent role in `factory_egress`
(`group_vars/factory.yml`); a repository cannot. Every domain contacted is logged in the run's usage
line.

**Checked: srt 0.0.81 cannot express "any domain" for open egress.** Its `network.allowedDomains`
schema rejects a bare `*`, and it also rejects `*.com` as too broad; the widest allowed entry is
`*.example.com`. Only `deniedDomains` accepts `*`. So srt's own proxy cannot be configured open,
while the default egress policy must be open.

The adapter therefore gives srt one loopback port and delegates the decision to
`foreman/internal/egress`:

- srt settings set `network.httpProxyPort` to the run's egress proxy, so srt still blocks direct
  outbound traffic and routes HTTP(S) through that port.
- The egress proxy enforces the agent role's allowlist (empty means open), refuses anything else, and
  records every host it is asked for for the usage line: `domains` lists the allowed hosts and
  `refused_domains` the refused ones, apart. A host is listed as allowed only once a connection to
  it was made, and a refusal never erases an earlier success. Each refused request is counted and,
  within a rate limit, raised as a security event (below).
- It also refuses a destination whose address is not publicly routable, whatever the allowlist
  says: loopback, private space, shared space (`100.64.0.0/10`, which is where a tailnet lives),
  link-local addresses including `169.254.169.254`, `0.0.0.0/8`, multicast and unspecified
  addresses, with IPv4-mapped IPv6 checked as IPv4. A name with any refused address is refused
  whole. Both the `CONNECT` path and the plain-HTTP path go through this check, so an open agent
  role still cannot reach dagu on the tailnet or another run's loopback proxy.
- Each destination is resolved once and the proxy dials the resolved address, so a second DNS
  answer cannot point the connection at an address the check never saw.
- The same proxy answers the model's `/api/v1` path and forwards it to the run's OpenRouter key
  proxy, adding the key there. The key proxy is foreman's own loopback listener, so that one
  forward is dialed without the public-address check. One loopback port therefore serves both the
  API and general egress, which matters on Linux: srt only bridges its configured proxy ports into
  the sandbox network namespace. `OPENCODE_CONFIG_CONTENT` points
  `provider.openrouter.options.baseURL` at that port.
- srt's own proxy is skipped for HTTP because an external proxy is named. Its SOCKS side still runs
  with the empty allowlist srt requires, so non-HTTP TCP egress is denied; opencode's traffic is
  HTTP(S).

This is a deliberate deviation from "srt's proxy is always in front": srt's proxy cannot be open, so
the forwarding decision and the domain log live in foreman's proxy, which srt forces all HTTP
traffic through. The OpenRouter key never enters the sandbox; only the key proxy holds it. The
decision is recorded in `docs/adr/0001-egress-proxy-in-front-of-srt.md`.

### Security events

A **security event** is one structured `slog` record at warn level, defined in
`foreman/internal/security`: `msg` is always `security event`, so one filter finds them all. Later
tickets reuse this shape.

| Field        | Meaning                                                                          |
| ------------ | -------------------------------------------------------------------------------- |
| `msg`        | always `security event`, so one filter finds them all                            |
| `event`      | stable event name, naming what was detected                                      |
| `outcome`    | why, stable and alertable                                                        |
| `host`       | the destination asked for (`openrouter.ai` for an API-path refusal); empty when the detection concerns no destination |
| `agent_role` | the run's agent role                                                             |
| `repository` | the run's repository, `owner/name`                                               |
| `issue`      | the run's issue number                                                           |

The egress proxy raises one for each request it does not allow, on the `CONNECT`, plain-HTTP and API
paths; an allowed request raises none. Every refusal is counted, but the event records are rate
limited: one outcome may write 5 records back to back, then one more per second. A model that floods
refusals therefore cannot fill the log, while the count — and the `factory_security_events_total`
measurement built from it — stays exact. Both proxies raise their refusals through the run's one
tally, so the limit is shared between them. The refused hosts the usage line carries are capped at
256 (`MaxRefusedHosts`) for the same reason.

| `event`                    | `outcome`                | What it records                                                        |
| -------------------------- | ------------------------ | ---------------------------------------------------------------------- |
| `egress_refused`           | `refused_by_policy`      | a host the allowlist leaves out, a path the proxy does not serve, or a malformed `CONNECT` destination |
| `egress_refused`           | `refused_not_routable`   | a destination whose address is not publicly routable                   |
| `egress_refused`           | `refused_tunnel_limit`   | a tunnel past `MaxTunnels`                                             |
| `egress_refused`           | `refused_bad_token`      | a missing or wrong run token                                           |
| `key_proxy_refused`        | `refused_bad_token`      | a call to the OpenRouter key proxy without the run token               |
| `output_rejected`          | `factory_marker`         | a model output file carrying a factory marker, an attempt to forge a transition |
| `output_rejected`          | `invalid_output`         | an output file that is readable but does not fit its shape             |
| `output_rejected`          | `unreadable_output`      | an output file foreman could not read at all                           |
| `dashboard_command_refused`| `unauthorized_author`    | a non-writer's command on the retro dashboard                          |
| `dashboard_command_refused`| `permission_unreadable`  | a command settled with no action because the permission check failed closed |
| `run_capped`               | `step_cap`               | a run that reached its step cap                                        |

A rejected output still posts its comment and applies nothing, a non-writer's command is still
minimized with no action, an unreadable permission still fails closed, and a step cap still fails the
run: the event only records what already happened. A **retro** that reaches its own step cap (or its
deadline) is dropped instead: the run has already settled, so it emits no event, and the only trace
is the `retro failed` diagnostic. The event carries only these fields: never the run
token, the key, a request header, or the model's file text.


## Fail closed

There is no unsandboxed fallback and no flag that disables the sandbox:

- a run on a host OS the adapter cannot serve fails before it touches anything;
- a missing `srt` binary, a settings compile failure, or a proxy that cannot bind fails the run;
- the session refuses to start `opencode` unless it has both a backend path and a settings file.

Ansible refuses to enroll a worker host that is not Linux arm64/amd64, and refuses to leave a host at
an srt version other than `factory_srt_version`.

## The first acceptance test

`foreman/internal/foreman/acceptance_test.go` runs a real `opencode run` under srt, with a fake
OpenRouter provider behind foreman's egress proxy, and asserts a session started and the API domain
was logged.

The spec also checks srt's filesystem boundary: a path outside the run cannot be read, while a file
in the run directory can be written and read back.

A third spec runs two sandboxed runs at the same time, each with its own run directory and the other's
hidden, and checks neither can see the other. That is the safety a per-host run cap above 1 rests on:
`factory_agent_run_cap` sets the worker's `--worker.max-active-runs`, and the sandbox keeps concurrent
runs apart.

**Result on this macOS/arm64 development host, when the test was first run (dated; the spec count
has since grown, so rerun it for a current figure):** **the specs passed** with the pinned opencode
**1.19.3** and srt **0.0.81**, run from the dev shell:

```sh
nix develop --command sh -c 'export PATH=/path/to/opencode-1.19.3:$PATH; go test ./internal/foreman -args -ginkgo.focus=acceptance'
# Ran 3 of 84 Specs ... SUCCESS! -- 3 Passed
```

The run needed the per-run token in `OPENCODE_CONFIG_CONTENT`'s `apiKey`, which opencode 1.19.3
requires and both proxies check, and a streamed `text/event-stream` provider answer. Both are in the
acceptance test and the production adapter.

The spec skips, rather than failing, when the host cannot exercise it:

- `srt` is not on `PATH`: `srt is not installed on this host; ...`
- `opencode` is absent: `opencode is not installed on this host`
- a different opencode version: `opencode 2.2.0 is installed, but the factory pins 1.19.3` — a 2.x
  run starts a managed local service that needs a local bind the sandbox does not grant, which is a
  harness difference, not a sandbox failure.

The rest of the suite runs with a faked runner, so no other spec needs the real backend.

## Known follow-ups

- Resource caps per run (memory, PIDs, CPU) are out of scope for this slice.
- Non-HTTP TCP egress is denied by srt's own SOCKS proxy, whose allowlist cannot be opened; the model
  reaches its provider over HTTP(S) through foreman's proxy.
