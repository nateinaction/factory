## Running commands

Run every command in this repo inside the flake's dev shell (`nix develop -c <command>`, or the direnv-loaded shell). `gitleaks`, `srt` and the Go toolchain are only on the path there, and the pre-commit hooks fail without them: `gitleaks` is missing, and the sandbox specs fail without `srt`.

## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues on nateinaction/factory, via the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Uses the default label names: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Code standards

See `CODING_STANDARDS.md`.

### Domain docs

Single-context: one `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
