# factory

Ansible that provisions Raspberry Pi hosts (Debian) to run
[dagu](https://github.com/dagucloud/dagu) workflows with GitHub access.

## What it sets up

| Ansible role | Result |
| --- | --- |
| `base` | hostname, `en_US.UTF-8` locale, a login banner (load, memory, disk, temp, dagu status), emptied `/etc/motd` |
| `nix` | [Determinate Nix](https://github.com/DeterminateSystems/nix-installer), system-wide `git`, and `direnv` + `nix-direnv` for the admin user |
| `users` | `dagu` (system account, `nologin`) and `factory` (regular account, locked password). Neither has sudo. |
| `dagu` | pinned, checksum-verified dagu binary and a systemd unit that runs it as `dagu`. The server (`dagu_server` group) runs the UI, scheduler and coordinator on localhost; workers (`dagu_worker` group) run `dagu worker` only |
| `agent` | the issue-to-PR agent: `foreman` (built on the controller with Go), pinned opencode, the factory's own forked skills (`roles/agent/files/skills/`, each naming the upstream path and ref it was forked from; nothing is cloned or pinned upstream at run time), and the `factory-*` DAGs (see below) |
| `poe_fan` | PoE HAT fan trip points raised to 75-79 °C so it only runs as a safety net (applies at next reboot) |
| `host_health` | `node_exporter` on every host, loopback only, for memory and filesystem use, plus a timer that writes the SoC temperature and the firmware's throttling bits (`vcgencmd get_throttled`) to its textfile collector. Each host's `collector` scrapes both and forwards them |
| `collector` | the OpenTelemetry Collector (contrib, from nixpkgs) on every host, listening on loopback only (OTLP 4317/4318) for foreman's telemetry. It tails the factory DAGs' run logs so foreman's diagnostics and security events reach Loki with their trace ids, keeps only allowlisted attributes (`factory_collector_allowed_attributes`), derives run counts and durations from spans, buffers within a fixed size while the backend is unreachable, and forwards traces, metrics and diagnostics to the telemetry backend (`factory_collector_backend_host`). It reads dagu's 0600 run logs with one read-only capability and runs unprivileged otherwise. The egress proxy refuses loopback, so a sandboxed run cannot reach it |
| `telemetry` | the telemetry backend on the dagu server, from nixpkgs: Prometheus (OTLP metrics), Tempo (traces), Loki (diagnostics) and Grafana in front. Every service binds loopback and `tailscale serve` exposes it to the tailnet only; every store is bounded by age or size; see ADR 0002 |
| `github_app` | a GitHub App credential readable only by `dagu`, plus a git credential helper that mints short-lived installation tokens |

## Use

Requires [Nix](https://nixos.org/download) and [direnv](https://direnv.net/);
`direnv allow` loads Ansible. The target hosts need an admin user with SSH key
login and passwordless sudo (`ansible_user` in `inventory/hosts.yml`).

```sh
ansible-playbook site.yml --check --diff   # preview
ansible-playbook site.yml                  # apply
ansible-playbook site.yml --limit factory-3
```

Every task is idempotent: a second run reports no changes.

To add a host, add it under `factory.hosts` in `inventory/hosts.yml`.

## Checks

The dev shell installs the `pre-commit` git hook. It runs the hooks in
`.pre-commit-config.yaml` — `gitleaks`, `go fix`, `go mod tidy`, `golangci-lint`
and the Go tests in `foreman/`, then the Ansible syntax check, `ansible-lint`,
`yamllint` and `tests/*.test` — and CI runs the same set. Run them all yourself
with:

```sh
pre-commit run --all-files
```

The dev shell also provides `emulate`, the GitHub emulator foreman's Go tests run against; see
[`foreman/docs/emulate.md`](foreman/docs/emulate.md) for how the suites use it and the GitHub
behaviour it does not model.

## GitHub App credentials

One GitHub App per agent role, named `<owner>-factory-<agent role>` (at most 34
characters), for example `nateinaction-factory-reviewer`. The private keys never
live in this repo.

1. Create each App (no webhook) with the permission ceiling the spec gives its
   agent role (see "Identity and tokens" in
   [#33](https://github.com/nateinaction/factory/issues/33)). The Implementer and
   Reviewer need Contents, Issues and Pull requests set to write.

   | App | Contents | Pull requests | Issues |
   |---|---|---|---|
   | Planner | read | none | write |
   | Researcher | write | none | write |
   | Implementer | write | write | write |
   | Reviewer | write | write | write |
   | Curator | none | none | write |

   Every App also has Metadata read; the Implementer and Reviewer have Checks,
   Actions and Commit statuses read, and no other App has them. The Curator
   works only from issues and comments, so it has no Contents: its runs clone
   nothing and read no repository files, even in a private repository.
2. Save each private key somewhere outside the repo.
3. `cp secrets/github_app.example.yml secrets/github_app.yml` and list the Apps you
   created, by agent role. `secrets/*` is gitignored.
4. Install an App on a repository (selected repositories only) to assign it to
   the factory. Installing any App assigns it; uninstalling all of them unassigns
   it. The factory works only the agent roles whose Apps are installed there,
   and other agent roles' issues wait in their queue.
5. On each assigned repository, require code-owner review in branch protection
   (or a ruleset) with a human in `CODEOWNERS`, so no factory App's approval can
   merge a pull request alone. No App can satisfy code-owner review. For example:

   ```
   # .github/CODEOWNERS
   * @your-github-login
   ```

Skip the Ansible role with `-e factory_github_app=false`. If the Ansible role runs and the
secrets file is missing, the play fails rather than skipping.

Each App's files go in `/etc/github-app/<agent role>/` (`app-id`,
`private-key.pem`), owned by dagu, directories 0700 and files 0600. On a host,
`github-app-token token --app <agent role> --repo <owner/name>` prints a fresh
one-hour token of that App scoped to the repository, and
`github-app-token repositories --app <agent role>` lists where it is installed.
No App ever uses another App's token. A token that can't be minted makes the
repository unassigned for that agent role.

## Dagu UI and coordinator

Both stay on localhost; `tailscale serve` (`dagu_tailscale_serve`, default on)
exposes them to the tailnet. It needs tailscale already installed and up, and
HTTPS enabled on the tailnet.

- UI: `https://<server>.<tailnet>.ts.net/` (server only).
- Coordinator: raw TCP on port 50055, for the workers.

dagu's own login is `builtin`: the first visitor to `/setup` creates the admin
account, so do that straight after provisioning. Anyone on the tailnet can still
reach the UI and the coordinator port, so restrict both with tailscale ACLs. Set
`dagu_tailscale_serve: false` to fall back to `ssh -L 8080:127.0.0.1:8080 <host>`.

## Telemetry backend

Grafana is at `https://<server>.<tailnet>.ts.net:3000` (`factory_telemetry_grafana_port`). Like the
dagu UI it is reachable by anyone on the tailnet, and so are the three ingest ports the other hosts'
collectors push to (Tempo 14317, Prometheus 9090, Loki 3100); restrict all of them with tailscale
ACLs. Grafana's admin login comes from `factory_telemetry_grafana_admin_user` and
`factory_telemetry_grafana_admin_password`, which the play writes into `grafana.ini`; the group_vars
default documents the login as `admin` / `admin`, so override the password (or put it in an
ansible-vault file) before provisioning and change it in Grafana afterwards.

To follow a run, open Explore with the Tempo datasource and search by the run's trace id. The trace
links to the run's diagnostics (Loki) and metrics (Prometheus); in Loki, `{} | trace_id="<id>"`
finds the diagnostics directly. The diagnostics come from the run's dagu stderr, which each host's
collector tails: foreman writes one JSON record per line and the record carries the trace's ids, so
the whole run — poll and run — is one trace. Retention is `factory_telemetry_*_retention*`; the
memory the stack may use is `factory_telemetry_memory_budget_mib`.

Prometheus evaluates the alert rules in `roles/telemetry/templates/alerts.yaml.j2`: egress refused a
non-public address, a request to either proxy without the run token, an output file carrying a factory
marker, an agent role's failure rate, the daily model cost and a dead run reaped by a poll. The
per-event alerts name the agent role, repository and issue and link straight to the trace that raised
them; the aggregate alerts name the agent role and repository and link to a search for their offending
traces and to the repository's failed issues. Every threshold, window and the cost budget is a
`factory_alerts_*` setting in `group_vars/factory.yml`. The rules fire into Prometheus's own list at
`http://127.0.0.1:9090/alerts` on the server; no Alertmanager is configured, so they are read there
or in Grafana rather than pushed.

The issue reaches a per-event alert as a label on the security-event metric only. That is a deliberate
exception to "an issue number is a span attribute, never a metric label": a Prometheus rule can
template only labels, and the alert has to name the issue it needs a human to look at. Every other
metric keeps the issue out.

Grafana also provisions the **Factory host health** dashboard (folder Factory). It puts each worker
host's run duration p95 beside that host's SoC temperature, throttling bits, memory use and disk
use, so a slow run can be told apart from a hot or throttled host. The run-duration panel matches
the run's root span (`factory_telemetry_run_span`, `foreman.run` by default); the dashboard's "Run
root span" variable overrides it for one view. Every host reports these with its own collector, via
`host_health` (see `factory_host_health_*`); node_exporter and the collector both listen on loopback.

## Issue agent

Every open issue in an assigned repository is the factory's unless it carries
`factory:paused`. `factory:` labels hold an issue's state, and every handoff
also posts a marker comment recording the agent role, run id, from-state,
to-state and commit SHA.

There are five agent roles. The Planner, Researcher, Implementer and Reviewer
work an issue; the Curator looks after the repository's retro dashboard. The
**human gate** sits between planning and building: the Planner asks questions
in rounds and ends with a final round holding a proposal, and only a writer's
reply of "yes", "yep", "yeah", "ok", "okay" or "lgtm" (or a writer's explicit
"ready to build", which also works with no final round pending) lets foreman
apply the proposal and queue the Implementer. The gate opens only from
`factory:waiting`, on the latest Planner round, and never on a non-writer's
comment. A person merges every PR; foreman has
no code path that merges.

1. `factory-poll` runs on the server every 5 minutes. It uses no model.
   It creates any missing `factory:` labels, repairs a stranded issue from its
   latest marker, and routes a new issue to its first queue: a
   `wayfinder:research` ticket to `factory:research`, and everything else to
   `factory:plan`. It redraws each repository's retro dashboard and, when a
   retro added findings, a group became ready or a writer commented `curate`,
   queues one Curator run (at most one at a time, once a day). It then works the
   issue queues in order (Reviewer, Implementer, Planner, Researcher, oldest
   issue first): for the first agent role with runs to spare, it claims up to
   that agent role's free run capacity, posts each claim marker, swaps the label, and
   queues the `factory-<agent role>` DAG with `REPO` and `ISSUE`. The Curator
   has no issue queue and so comes last in the order. An agent role's capacity is each host's run cap
   (`factory_agent_run_cap`) times the worker hosts that take it, less its runs
   in progress; a run waiting in dagu blocks new claims for its agent role. Nothing
   ready means it exits. Only a writer's issues and comments count, and a paused
   issue is skipped.
2. `factory-<agent role>` runs on a worker host that takes that agent role,
   up to the host's run cap (`factory_agent_run_cap`, one at a time by default).
   It mints a one-hour token of that agent role's App scoped to the repo
   (`github-app-token token --app <agent role> --repo`), writes it to
   `/run/factory/gh-token-<agent role>-<run id>` (one file per run, written
   atomically), and runs
   `foreman run <agent role> <owner/repo> <issue>` as `factory` through a
   single-purpose sudoers rule.
3. Plan (`run`): the Planner reads the issue and its thread and writes a round
   of numbered questions or a final round with a proposal (one issue, slices,
   a wayfinder map or a ticket resolution). Foreman posts the round, hides a
   proposal in its comment, and the issue waits in `factory:waiting` for a
   writer. A writer's reply wakes the Planner for another round; a writer opening
   the human gate on the final round applies the proposal.
4. Research (`run`): clones the repo, runs `opencode run` with the `research`
   skill, commits the findings under `docs/research/`, and pushes them to
   `research/<name>`. It never opens a PR: the branch is a record, never merged.
   Foreman posts the resolution (with a link to the branch), appends the
   decision line to the ticket's map, and the ticket closes with no factory
   label. A re-run reuses the branch and updates the same resolution comment.
   A retro follows the push, keyed by the findings branch SHA.
5. Implement (`run`): clones the repo onto `agent/issue-N`, runs `opencode run`
   with the `implement` skill, pushes, and opens a PR. If the PR already exists
   (a rework), it continues from that branch with the latest review in the
   prompt. Then it continues the same opencode session, by explicit session id,
   with the `retro` skill: the retro writes `retro.json`, and foreman posts it
   whole as a marker comment on the issue, keyed by agent role and commit SHA. A
   repeat for the same key updates that comment. The retro has its own step cap
   and deadline and is best effort: a dropped or invalid retro is logged and
   does not fail the run. The issue becomes `factory:review`.
6. Review (`run`): first a CI gate. An issue in `factory:review` waits for its
   PR's checks; a failure sends it back to `factory:implement` with no model
   run, and checks still pending after `factory_ci_pending_deadline_minutes`
   fail it. Once CI passes, the Reviewer checks out the PR branch, runs the
   `code-review` skill against the issue as spec, and posts the verdict as a
   PR review under its own App. With no high or medium findings it approves,
   asks a human writer to review (the issue's author, or the latest writer who
   commented), and the issue becomes `factory:done`. Otherwise it goes back to
   `factory:implement`. A review round is one commit that was reviewed or failed
   CI; CI failures count toward the same limit as findings. At
   `factory_review_round_limit` rounds (3 by default) the issue is
   `factory:failed` instead. A retro follows the verdict, keyed by the reviewed
   PR head SHA.
7. Any failure leaves the issue `factory:failed` with a marker and a comment. A
   writer's comment sends it back to the agent role that failed.

Setup: `cp secrets/opencode.example.yml secrets/opencode.yml` and add an
OpenRouter key. The Apps need the permissions above, and installations must accept them. Each agent role's
model, context cap and compaction setting come from `factory_models`.

Things to know:

- The worker's dagu unit sets `NoNewPrivileges=false` so `sudo` can run.
- foreman writes diagnostics to stderr as `log/slog` JSON records, one object per line (`time`, `level`, `msg`, plus
  the record's attributes as further keys). `FACTORY_LOG_LEVEL` sets the level (`debug`, `info`,
  `warn`, `error`); unset or unknown means `info`. Stdout carries only program
  output, never diagnostics. The model's transcript (private repository code,
  issue text) is logged only at `debug`, with every secret the run holds redacted;
  at the default level the run log carries no transcript.
- Each run leaves its usage record out of band, at `<work root>/usage/<run id>.json` on the host
  that ran it: tokens, cost, duration, attempts and outcome. `foreman status` prefers the run's root
  span in the telemetry backend when one is configured (`foreman status --telemetry-url
  http://127.0.0.1:3200` on the server, or `FACTORY_TELEMETRY_URL`), so it reports a run a worker
  host executed; without a backend, or for a run the backend has not seen, it reads the local record.
  It never reads a run's stdout, so a line there that mimics a record cannot forge a usage figure.
  The run also prints the same record as one JSON line on stdout, which `foreman logs` and the dagu
  UI show to a person reading the run log.
- Foreman exports OTLP to the host's collector: the dagu unit sets `OTEL_EXPORTER_OTLP_ENDPOINT` and
  `OTEL_SERVICE_NAME` (`factory_foreman_otlp_endpoint`), and `start` keeps them across sudo by name.
  No `OTEL_*` variable reaches the model: its environment is built from the run's own settings.
- `foreman --help` lists the verbs (`poll`, `start`, `run`, `status`, `logs`, `history`). Settings
  default to the host layout and can be overridden by `FACTORY_*` environment variables or
  by the matching flag (`--work-root`, `--config`, ...); a flag wins over the
  environment, which wins over the default.
- A run past an hour loses its read-only token. Each agent role has its own step
  cap and deadline (`factory_agent_limits`: Planner 20 minutes, Researcher 45,
  Implementer 90, Reviewer 30, Curator 30); hitting either fails the run, and a
  poll calls a run dead once its label ages `factory_dead_run_margin_minutes`
  past the deadline.
- The Reviewer has its own App, so it is not the PR's author and its `APPROVE`
  or `REQUEST_CHANGES` counts as a native review. It cannot satisfy code-owner
  review, which needs a human: that is why an approval asks a writer to review.
  If GitHub still refuses a review as the PR author's own, foreman posts it as a
  comment review.
- Each agent role runs opencode under the sandbox, with permission denied by
  default: the agent role allows only the edits, shell commands and skills its
  work needs, and the sandbox bounds what it may read, write and reach. Only a
  writer's issues and comments move the factory, so only people you trust should
  have write access.
- After a failed play, handlers (service restarts) did not run. Rerun the play
  or restart `dagu` on the hosts.

## Not covered

- Bootstrapping the admin user and SSH keys (do this when imaging the Pi).
- Hooking direnv into the admin user's shell.
- Rotating the app key: replace the key file and rerun the play.

### Cutover from finances-agent

The first apply after the foreman change is the cutover. Before anything else
changes, the play asks dagu whether a `finances-agent-*` run is active and fails
if one is. Drain first: suspend `finances-agent-poll` in the dagu UI, let any run
finish, then apply. The play then installs `/usr/local/bin/foreman`, deploys
`factory-poll` and one `factory-<agent role>` DAG per agent role, and removes the old
DAG files and `/usr/local/bin/finances-agent`. The removal tasks stay for one
release. Needs `go` on the controller (the flake's dev shell has it).
