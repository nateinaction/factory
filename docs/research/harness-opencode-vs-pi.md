# Harness for agent roles: opencode vs pi

Research for issue #8 (map #4, "Wayfinder: agent roles for the factory").

**Question.** To run each agent role unattended on an arm64 Raspberry Pi, compare
opencode (its native agents and modes) with the pi coding-agent harness on: per-role
prompt, model, tool and permission configuration; headless runs; continuing a session
(as the retro does today); skills (`SKILL.md`) loading; OpenRouter; install and
pinning. Which can express agent roles natively, and what would each force us to
build ourselves?

This document gathers facts and trade-offs. Choosing the harness is a later grilling
ticket ("Which harness runs agent roles?").

## Sources and versions

- **opencode**: `github.com/sst/opencode`, which now redirects to
  `github.com/anomalyco/opencode`. The factory pins **v1.19.3**
  (`group_vars/factory.yml`); the latest release is v1.19.4. `main` already carries
  version 2.2.0 (unreleased), so every doc claim below was checked against the
  **v1.19.3 tag** of `packages/web/src/content/docs/` (`agents.mdx`, `cli.mdx`,
  `skills.mdx`, `permissions.mdx`, `config.mdx`, `providers.mdx`). Behaviour of
  `opencode run` comes from `packages/cli/src/run/noninteractive.ts` on `main`.
- **pi**: the pi coding agent by Mario Zechner. `github.com/badlogic/pi-mono` now
  redirects to **`github.com/earendil-works/pi`**. The npm package is
  `@earendil-works/pi-coding-agent` (binary `pi`). Latest release **v1.4.0**
  (2026-10-21). Docs are `packages/coding-agent/docs/*.md` (`cli.md`, `skills.md`,
  `settings.md`, `configuration.md`, `security.md`, `providers.md`, `quickstart.md`,
  `extensions.md`, `environment-variables.md`) plus `packages/coding-agent/examples/`.
- **Is this the right "pi"?** Yes. This is the only coding-agent harness called "pi"
  (site pi.dev). The other well-known "Pi", Inflection AI's chat assistant, is not a
  coding harness. No other candidate turned up.

## Side by side

| Concern | opencode v1.19.3 | pi v1.4.0 |
|---|---|---|
| Named agent definitions | **Native.** `agent.<name>` in `opencode.json`, or one Markdown file per agent in `~/.config/opencode/agents/` or `.opencode/agents/`. Each sets `description`, `mode` (`primary`/`subagent`/`all`), `model`, `prompt` (inline or `{file:...}`), `temperature`, `top_p`, `steps` (an iteration cap), `permission`, `disable`, `hidden` (agents.mdx "Configure", "Options") | **None.** The README states it "skips features like sub-agents and plan mode". A role is the set of flags or the config directory a process starts with (see the next rows) |
| Per-role prompt | `prompt` on the agent | `--system-prompt <text\|path>` (replaces) or `--append-system-prompt` (repeatable) (cli.md "Prompts and process") |
| Per-role model | `model: provider/model` on the agent; `opencode run --model` overrides it | `--model provider/id[:thinking]`, `--thinking` (cli.md "Models") |
| Per-role tools | `permission` keys per tool; the `tools` map is deprecated in favour of `permission` | `--tools` allowlist, `--exclude-tools`, `--no-tools`; `defaultTools` in settings (cli.md "Tools") |
| Per-role permissions | **Native, fine-grained.** `allow`/`ask`/`deny` per tool (`read`, `edit`, `glob`, `grep`, `list`, `bash`, `task`, `skill`, `external_directory`, `lsp`, `webfetch`, ...). Glob patterns on `bash` commands (for example `"git push*": "deny"`), on paths and on skill names. Per-agent rules override global ones and the last matching rule wins (agents.mdx "Permissions", permissions.mdx) | **No permission layer.** "Pi ... does not ask for approval before every tool call"; tools run with the OS user's permissions; the docs recommend a container or VM as the real boundary (security.md). Narrowing is by tool selection only. Anything finer, such as blocking `git push` inside `bash`, needs an **extension** that handles the `tool_call` event and returns `{block: true}` (extensions.md "tool_call"; examples `permission-gate.ts`, `protected-paths.ts`) |
| Headless run | `opencode run [message]` with `--agent`, `--model`, `--format json`, `--dir`, `--title`, `--attach` to a running `opencode serve`, and `--auto` (cli.md "run") | `pi -p/--print` (final text to stdout), `--mode json` (JSONL events), `--mode rpc` (JSONL over stdin/stdout), and a TypeScript SDK (cli.md "Invocation and output") |
| Unattended approvals | `ask` cannot be answered headless. Without `--auto`, the run auto-rejects the request: on `main` the tool fails with feedback and the model carries on; in v1-compatibility mode the run is marked rejected. Questions are cancelled with feedback. `--auto` approves every `ask` but still enforces explicit `deny` (noninteractive.ts `replyPermission`; permissions.mdx). The factory sets `"permission": "allow"` globally today | Nothing to approve. Project-local config and resources are trust-gated: pass `--approve` or `--no-approve` for an explicit decision in automation (security.md "project trust") |
| Continue a session | `--continue` (last session), `--session <id>`, `--fork` with either (cli.md "run") | `-c/--continue` (most recent session **for the current project**, meaning the working directory), `--session <path\|id>`, `--session-id <id>` (opens that exact ID, or creates it), `--fork`, `--session-dir`, `--no-session` (cli.md "Sessions") |
| Skills (`SKILL.md`) | Native `skill` tool, loaded on demand. Discovered in `.opencode/skills/`, `~/.config/opencode/skills/`, `.claude/skills/`, `~/.claude/skills/`, `.agents/skills/` and `~/.agents/skills/`, walking up to the git worktree. Per-agent `permission.skill` globs allow, deny (deny also hides the skill) or ask; the tool can be switched off per agent; `OPENCODE_DISABLE_EXTERNAL_SKILLS` and `OPENCODE_DISABLE_CLAUDE_CODE_SKILLS` stop discovery outside the opencode directories (skills.mdx; cli.mdx env table) | Implements the Agent Skills spec. Names and descriptions go into the system prompt and the model reads `SKILL.md` when it needs one. Discovered in `~/.pi/agent/skills/`, `.pi/skills/`, `~/.agents/skills/` and `.agents/skills/` (not `.claude/skills/`). Per process: `--skill <path>` (repeatable) plus `--no-skills` gives an **exact skill set per invocation**; there is also a `skills` setting (skills.md; cli.md "Resources"; settings.md) |
| OpenRouter | Built-in provider; the factory already uses `OPENROUTER_API_KEY` with `openrouter/<model>` (providers.mdx "OpenRouter") | Built-in provider; `OPENROUTER_API_KEY` or `/login openrouter`. The model list is filtered to what the key may use, through OpenRouter's `/models/user`. Print and RPC modes use the cached list (providers.md "OpenRouter") |
| Isolating config per role | `OPENCODE_CONFIG`, `OPENCODE_CONFIG_DIR` or `OPENCODE_CONFIG_CONTENT`. Configs **merge** (global, then custom, then project, then inline), so a project `opencode.json` in the assigned repo can override the factory's. `OPENCODE_DISABLE_PROJECT_CONFIG` blocks that (config.mdx; cli.mdx env table) | `PI_CODING_AGENT_DIR` (one config directory per role) and `PI_CODING_AGENT_SESSION_DIR`. Project `.pi/` loads only with trust, so `--no-approve` ignores the assigned repo's config. `-nc` skips `AGENTS.md`/`CLAUDE.md` (environment-variables.md; cli.md) |
| Install on arm64 Linux | One prebuilt `opencode-linux-arm64.tar.gz` per GitHub release. The role already downloads it, checks its checksum and verifies `--version` (`roles/agent/tasks/main.yml`) | Release assets include `pi-linux-arm64.tar.gz` (a Bun-compiled binary, `scripts/build-binaries.sh`) plus `SHA256SUMS`, so the same pin-and-checksum pattern works. Also: an installer script that pins dependencies, npm (needs Node ≥ 22.19 and leaves transitive dependencies unpinned), and a Nix flake pinnable by tag `github:earendil-works/pi/v1.4.0` (README; quickstart.md) |
| Sub-agents / delegation | Native: subagents run through the `task` tool, and `permission.task` globs control which an agent may call | Not built in; there is an example `subagent` extension that runs each subagent as a separate `pi` process, with agent definitions as Markdown frontmatter (`name`, `description`, `tools`, `model`) |

## What each forces us to build

**opencode** can express agent roles natively. An Implementer and a Reviewer become two
entries under `agent` (or two Markdown files), each with its own prompt, model, step
cap and permission rules, for example a Reviewer with `edit: deny` and only read-only
`bash` patterns. They run with `opencode run --agent reviewer`. What we would still
build or decide:

- Moving from today's global `"permission": "allow"` to per-agent rules, and choosing
  between `--auto` and explicit `allow` lists. An `ask` that is left in place gets
  auto-rejected when no one is there to answer.
- Stopping the assigned repository from overriding the factory's agents. Configs merge
  and project config has the highest precedence, so this needs
  `OPENCODE_DISABLE_PROJECT_CONFIG` or a decision that the repository's config is
  welcome.
- Per-role skill sets are expressed as `permission.skill` allow and deny globs over one
  shared discovery pool. Discovery also reaches into the clone's `.claude/` and
  `.agents/` unless disabled.
- Tracking a moving target: `main` is already 2.x and differs from the pinned 1.x in
  headless permission behaviour.

**pi** has no agent-role concept. That fits its stated minimalism, and its CLI covers
most of the gap. A role is a wrapper invocation, or a `PI_CODING_AGENT_DIR` per role,
supplying `--system-prompt`, `--model`, `--tools`, `--no-skills --skill ...` and
`--no-approve`. What we would build:

- **Role definitions:** a small format (or one config directory per role) plus the code
  in `finances-agent` that turns it into CLI flags.
- **Permission boundaries:** anything finer than "this role has or lacks the `bash` and
  `edit` tools" needs a TypeScript extension on `tool_call` that we write, pin and
  test, or an OS boundary (a separate user per role, a container). The Reviewer's "may
  run `git diff` but not `git push`" cannot be expressed in configuration.
- **Delegation**, if a role needs subagents: the example extension, or our own.
- Its strengths for this use: an exact per-invocation skill set, deterministic
  `--session-id` (the retro could continue a session keyed by issue instead of "most
  recent"), and project config that loads only when trusted, which is safer for
  arbitrary assigned repositories.

## Trade-offs to carry into the grilling ticket

- Native role and permission expressiveness favours **opencode**; pi pushes both into
  our code or the OS.
- Isolation from the assigned repository's own config, and exact skill scoping, are
  simpler in **pi** (`--no-approve`, `--no-skills --skill`). opencode needs environment
  flags to get the same.
- Continuing a session: both work. pi's `--session-id` is more deterministic than
  `--continue`; opencode's `--session <id>` does the same once the ID has been
  captured, for example from `--format json` events.
- Install and pinning are comparable: both publish checksummable linux-arm64 release
  binaries. pi also has a Nix flake.
- Switching has a migration cost: the factory's role, config template, skill linking
  and `finances-agent` invocations are all written against opencode.
