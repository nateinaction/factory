# The sandbox for agent role runs

foreman owns one OS-agnostic spec per agent role (`foreman/internal/sandbox`): what the run may read
and write, what it may reach, and which paths stay hidden. A sandbox adapter compiles that spec to
**srt** (`@anthropic-ai/sandbox-runtime`, `sandbox-runtime` in nixpkgs), pinned at **0.0.81**. srt
uses Seatbelt on macOS and bubblewrap on Linux. There are no containers and no VMs.

The whole `opencode run` — retry attempts and the retro included — runs under srt. foreman stays
outside and does every clone, fetch, commit, push and GitHub write itself, running git on the host.
The model's `git` can read the clone but cannot write `.git`, so the code it runs never becomes a
commit outside the sandbox. The read-only installation token is the only secret inside the sandbox,
as `GH_TOKEN`; the token file it was minted from stays in the deny list.

## Filesystem

| Access | What |
|---|---|
| Read-write | the clone's working tree (Implementer and Reviewer); the run's scratch/output directory; the run's own `HOME` and XDG directories |
| Read-only | the forked skills; `/nix/store`; the dev environment resolved from the default branch; `.git` |
| Not visible | the real home (`~/.ssh` included), the work root and so every other run, dagu's home, the token directory, the key file and `/etc/factory-agent` |

The Reviewer gets a throwaway writable clone under the work root. The Planner, Researcher and Curator
get a clone they can only read. `HOME`, `XDG_CACHE_HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`,
`XDG_STATE_HOME` and `TMPDIR` are created under `<work root>/run-<role>-<issue>/` and deleted with the
run and its retro. Only `/nix/store` is shared between runs, read-only.

srt's read model is deny-then-allow: reads are open unless denied. foreman denies each sensitive
location explicitly instead of denying `/` and re-allowing the operating system, which would need a
long, fragile allow list. The acceptance criterion — the real home, `~/.ssh`, other runs, dagu's
directories and foreman's secrets are unreachable — is met by the deny list above.

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
- The egress proxy enforces the role's allowlist (empty means open), refuses anything else, and
  records every host it is asked for — allowed or refused — for the usage line.
- The same proxy answers the model's `/api/v1` path and forwards it to the run's OpenRouter key
  proxy, adding the key there. One loopback port therefore serves both the API and general egress,
  which matters on Linux: srt only bridges its configured proxy ports into the sandbox network
  namespace. `OPENCODE_CONFIG_CONTENT` points `provider.openrouter.options.baseURL` at that port.
- srt's own proxy is skipped for HTTP because an external proxy is named. Its SOCKS side still runs
  with the empty allowlist srt requires, so non-HTTP TCP egress is denied; opencode's traffic is
  HTTP(S).

This is a deliberate deviation from "srt's proxy is always in front": srt's proxy cannot be open, so
the forwarding decision and the domain log live in foreman's proxy, which srt forces all HTTP
traffic through. The OpenRouter key never enters the sandbox; only the key proxy holds it.

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

**Result on this macOS/arm64 development host:** **the specs passed** with the pinned opencode
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
