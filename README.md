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

## GitHub App credential

The private key never lives in this repo.

1. Create a GitHub App (no webhook) with the permissions your workflows need
   and install it on the repos they touch.
2. Save its private key somewhere outside the repo.
3. `cp secrets/github_app.example.yml secrets/github_app.yml` and fill it in.
   `secrets/*` is gitignored.

Skip the role with `-e factory_github_app=false`. If the role runs and the
secrets file is missing, the play fails rather than skipping.

On a host, `github-app-token token` prints a token (cached until 5 minutes
before expiry). git picks it up through the credential helper in
`/var/lib/dagu/.gitconfig`, so DAG steps can `git clone https://github.com/...`
with no further setup. For the API: `GH_TOKEN=$(github-app-token token)`.

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

Labelling an issue `ready-for-agent` opens a PR for it, and the agent then
reviews that PR.

1. `factory-poll` runs on the server every 5 minutes. It uses no model.
   It takes the oldest open `ready-for-agent-review` issue, or failing that the
   oldest `ready-for-agent` one, swaps the label to `agent-reviewing` or Nothing ready means it exits.
   `agent-working`, and queues the `factory-<role>` DAG with `REPO` and `ISSUE`.
2. `finances-agent-work` runs on a worker labelled `runner=agent`, one at a
   time. It mints a one-hour token scoped to the repo
   (`github-app-token token --repo`), writes it to `/run/factory/gh-token`, and
   runs `finances-agent run N` (or `review-run N`) as `factory` through a
   single-purpose sudoers rule.
3. Implement (`run`): clones the repo onto `agent/issue-N`, runs `opencode run`
   with the `implement` skill, pushes, and opens a PR. If the PR already exists
   (a rework), it continues from that branch with the latest review in the
   prompt. Then it continues the same opencode session with the `retro` skill
   and posts the result as a comment on the open issue labelled `retro` (it
   creates "Agent retros" the first time). The retro is best effort: a failure
   is logged and does not fail the run. The issue becomes
   `ready-for-agent-review`.
4. Review (`review-run`): checks out the PR branch, runs the `code-review` skill
   against the issue as spec, and posts the findings on the PR as a review. With
   no high or medium findings it approves and the issue becomes `agent-done`.
   Otherwise the issue goes back to `ready-for-agent` for another implement
   pass, up to 3 reviews, after which it is `agent-failed`.
5. Any failure leaves the issue `agent-failed` with a comment. Re-apply
   `ready-for-agent` (or `ready-for-agent-review`) to retry.

Setup: `cp secrets/opencode.example.yml secrets/opencode.yml` and add an
OpenRouter key. The GitHub App needs Contents, Issues and Pull requests set to
write, and the installation must accept them. Each agent role's
model, context cap and compaction setting come from `factory_models`.

Things to know:

- The worker's dagu unit sets `NoNewPrivileges=false` so `sudo` can run.
- A run past an hour loses its token, and the agent has a one-hour cap.
- GitHub does not let a PR's author approve it, and the App opens the PRs, so the
  review agent's approval is rejected. It then posts a comment review saying so;
  `agent-done` is the real signal. Approving needs a second identity.
- opencode runs with all permissions allowed. The `ready-for-agent` label is
  the gate, so only people you trust should be able to apply it.
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
