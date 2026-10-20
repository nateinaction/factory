# factory

Ansible that provisions Raspberry Pi hosts (Debian) to run
[dagu](https://github.com/dagucloud/dagu) workflows with GitHub access.

## What it sets up

| Role | Result |
| --- | --- |
| `base` | hostname, `en_US.UTF-8` locale, a login banner (load, memory, disk, temp, dagu status), emptied `/etc/motd` |
| `nix` | [Determinate Nix](https://github.com/DeterminateSystems/nix-installer), system-wide `git`, and `direnv` + `nix-direnv` for the admin user |
| `users` | `dagu` (system account, `nologin`) and `factory` (regular account, locked password). Neither has sudo. |
| `dagu` | pinned, checksum-verified dagu binary and a systemd unit that runs it as `dagu`, bound to `127.0.0.1:8080` |
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

The unit binds the UI to localhost because dagu has no authentication here.
Reach it with `ssh -L 8080:127.0.0.1:8080 <host>`.

## Not covered

- Bootstrapping the admin user and SSH keys (do this when imaging the Pi).
- Hooking direnv into the admin user's shell.
- Rotating the app key: replace the key file and rerun the play.
