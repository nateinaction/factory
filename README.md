# factory

Ansible that provisions Raspberry Pi hosts (Debian) to run
[dagu](https://github.com/dagucloud/dagu) workflows with GitHub access.

## What it sets up

| Role | Result |
| --- | --- |
| `base` | hostname, `en_US.UTF-8` locale, a login banner (load, memory, disk, temp, dagu status), emptied `/etc/motd` |
| `nix` | [Determinate Nix](https://github.com/DeterminateSystems/nix-installer), system-wide `git`, and `direnv` + `nix-direnv` for the admin user |
| `users` | `dagu` (system account, `nologin`) and `factory` (regular account, locked password). Neither has sudo. |
| `dagu` | pinned, checksum-verified dagu binary and a systemd unit that runs it as `dagu`. The server (`dagu_server` group) runs the UI, scheduler and coordinator on localhost; workers (`dagu_worker` group) run `dagu worker` only |
| `agent` | the issue-to-PR agent: `foreman` (built on the controller with Go), pinned opencode, the pinned mattpocock skills, and the `factory-*` DAGs (see below) |
| `poe_fan` | PoE HAT fan trip points raised to 75-79 °C so it only runs as a safety net (applies at next reboot) |
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
`.pre-commit-config.yaml` — `go fix`, `go mod tidy`, `golangci-lint` and the
Go tests in `foreman/`, then the Ansible syntax check and `tests/*.test` — and
CI runs the same set. Run them all yourself with:

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
2. Save each private key somewhere outside the repo.
3. `cp secrets/github_app.example.yml secrets/github_app.yml` and list the Apps you
   created, by agent role. `secrets/*` is gitignored.
4. Install an App on a repository (selected repositories only) to assign it to
   the factory. Installing any App assigns it; uninstalling all of them unassigns
   it. The factory works only the agent roles whose Apps are installed there,
   and other agent roles' issues wait in their queue.

Skip the role with `-e factory_github_app=false`. If the role runs and the
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

## Issue agent

Every open issue in an assigned repository is the factory's unless it carries
`factory:paused`. `factory:` labels hold an issue's state, and every handoff
also posts a marker comment recording the agent role, run id, from-state,
to-state and commit SHA. Applying `factory:implement` starts the Implementer
path; the Reviewer takes over from `factory:review`.

1. `factory-poll` runs on the server every 5 minutes. It uses no model.
   It creates any missing `factory:` labels, repairs a stranded issue from its
   latest marker, and routes a new issue to its first queue: a
   `wayfinder:research` ticket to `factory:research`, and everything else to
   `factory:plan`. It then takes the oldest unblocked `factory:review` issue, or
   failing that the oldest `factory:implement` one, posts the claim marker,
   swaps the label to `factory:reviewing` or `factory:implementing`, and queues
   the `factory-<role>` DAG with `REPO` and `ISSUE`. Nothing ready means it
   exits. Only a writer's issues and comments count, and a paused issue is
   skipped.
2. `factory-<role>` runs on a worker labelled `runner=agent`, one at a time. It
   mints a one-hour token of that agent role's App scoped to the repo
   (`github-app-token token --app <agent role> --repo`), writes it to
   `/run/factory/gh-token-<agent role>`, and runs
   `foreman run <role> <owner/repo> <issue>` as `factory` through a
   single-purpose sudoers rule.
3. Research (`run`): clones the repo, runs `opencode run` with the `research`
   skill, commits the findings under `docs/research/`, and pushes them to
   `research/<name>`. It never opens a PR: the branch is a record, never merged.
   Foreman posts the resolution (with a link to the branch), appends the
   decision line to the ticket's map, and the ticket closes with no factory
   label. A re-run reuses the branch and updates the same resolution comment.
   A retro follows the push, keyed by the findings branch SHA.
4. Implement (`run`): clones the repo onto `agent/issue-N`, runs `opencode run`
   with the `implement` skill, pushes, and opens a PR. If the PR already exists
   (a rework), it continues from that branch with the latest review in the
   prompt. Then it continues the same opencode session, by explicit session id,
   with the `retro` skill: the retro writes `retro.json`, and foreman posts it
   whole as a marker comment on the issue, keyed by agent role and commit SHA. A
   repeat for the same key updates that comment. The retro has its own step cap
   and deadline and is best effort: a dropped or invalid retro is logged and
   does not fail the run. The issue becomes `factory:review`.
5. Review (`review-run`): checks out the PR branch, runs the `code-review` skill
   against the issue as spec, and posts the findings on the PR as a review. With
   no high or medium findings it approves and the issue becomes `factory:done`.
   Otherwise the issue goes back to `factory:implement` for another implement
   pass, up to 3 reviews, after which it is `factory:failed`. A retro follows
   the verdict, keyed by the reviewed PR head SHA.
6. Any failure leaves the issue `factory:failed` with a marker and a comment. A
   writer's comment sends it back to the agent role that failed.

Setup: `cp secrets/opencode.example.yml secrets/opencode.yml` and add an
OpenRouter key. The Apps need the permissions above, and installations must accept them. Each agent role's
model, context cap and compaction setting come from `factory_models`.

Things to know:

- The worker's dagu unit sets `NoNewPrivileges=false` so `sudo` can run.
- foreman writes diagnostics to stderr as `log/slog` text records (leveled, with
  key/value attributes). `FACTORY_LOG_LEVEL` sets the level (`debug`, `info`,
  `warn`, `error`); unset or unknown means `info`. Stdout carries only program
  output, never diagnostics.
- `foreman --help` lists the verbs (`poll`, `start`, `run`). Settings default to
  the host layout and can be overridden by `FACTORY_*` environment variables or
  by the matching flag (`--work-root`, `--config`, ...); a flag wins over the
  environment, which wins over the default.
- A run past an hour loses its token, and the agent has a one-hour cap.
- GitHub does not let a PR's author approve it, and the App opens the PRs, so the
  review agent's approval is rejected. It then posts a comment review saying so;
  `factory:done` is the real signal. Approving needs a second identity.
- opencode runs with all permissions allowed. Only a writer's issues and
  comments move the factory, so only people you trust should have write access.
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
`factory-poll` and one `factory-<role>` DAG per agent role, and removes the old
DAG files and `/usr/local/bin/finances-agent`. The removal tasks stay for one
release. Needs `go` on the controller (the flake's dev shell has it).
