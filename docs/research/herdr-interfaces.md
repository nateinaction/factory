# Herdr's interfaces

Question (#180): what interfaces does Herdr define for driving coding agents, and what overlaps with a scheduler? Feeds the agent harness and scheduler seams of #178.

Source: `herdrdev/herdr` at commit `ee5bfb5041bb3e4a49d591c72308aae3c81ecdde` (2026-10-29), Apache-2.0, Rust. Docs are the repo's own `docs/next/website/src/content/docs/*.mdx`; code paths are relative to the repo root.

## Summary

- Herdr is a terminal multiplexer (tmux-like), not a harness abstraction. A background server owns PTYs; clients attach. Agents are **recognized processes inside panes**, not objects Herdr launches through a driver interface.
- There is **no per-harness interface in code**. `Agent` is a closed enum of 24 kinds (`src/detect/mod.rs:43`); per-harness behaviour is data (a TOML detection manifest per agent in `src/detect/manifests/`) plus an optional hook script/extension installed into the harness (`src/integration/`).
- The one open extension point is a **reporting protocol**: an agent (or a hook) calls `pane.report_agent` / `pane.report_agent_session` / `pane.release_agent` over the socket. That is the closest thing to a harness contract, and it points the opposite way: harness reports to the driver.
- The lifecycle vocabulary is five states: `idle`, `working`, `blocked`, `done`, `unknown`. There is **no failed or exited state**; failure is only visible as a pane exit, a timeout or `agent_not_running`.
- Scheduler overlap is small: persistence of live sessions, wait primitives and agent-to-agent prompting. No queue, DAG, retry, dispatch policy or run records.

## 1. How it abstracts an agent harness

### Spawn

`agent.start {name, kind, pane_id, args[], timeout_ms}` (`src/api/schema/agents.rs`, `AgentStartParams`).

- `kind` is one of 24 fixed strings (`pi, claude, codex, gemini, cursor, devin, agy, cline, omp, mastracode, opencode, copilot, kimi, kiro, droid, amp, grok, hermes, kilo, qodercli, qwen, letta, maki, muse`), each mapped to a canonical executable (`interactive_agent_executable`, `src/detect/mod.rs:153`). Native args pass after `--`, unchanged. (agent-automation.mdx, "Agent identity and launch")
- It requires an **existing shell pane at its prompt** and never creates or moves layout; layout is a separate primitive (`workspace`, `tab`, `pane.split`). It types the command into the shell. (agent-automation.mdx, "Three primitives")
- Returns only once Herdr detects the expected agent in that pane and judges it ready for input (default 30s, bounds >3000 to 300000 ms). If detection says `blocked` during startup it returns `agent_not_ready` immediately but keeps the name. (same page)
- Manually launched agents are detected too; `agent.rename` names them. Names match `[a-z][a-z0-9_-]{0,31}`, unique among live agents, cleared on exit, release or replacement.

### State detection

Layered, in this order of authority (agents.mdx "How status works"; `src/pane.rs` lifecycle authority code):

1. **Foreground process**: the PTY's foreground process group is inspected and matched to an `Agent` by normalized process name (`identify_agent_process_in_job`, `src/detect/mod.rs:254`). Wrapper escape hatch: `HERDR_AGENT=<kind>` env on the wrapper; `HERDR_PROCESS_DETECTION=child-groups` fallback on restricted Linux.
2. **Screen manifests**: per-agent TOML rules (`src/detect/manifests/*.toml`) match regexes against regions of the live bottom-of-buffer text and the OSC title (`region = "osc_title"`, `"bottom_non_empty_lines(12)"`), each with `state`, `priority` and evidence flags (`visible_working`, `visible_idle`, `visible_blocker`). Output is `AgentDetection` (`src/detect/mod.rs:24`). Manifests update remotely from herdr.dev without a binary update; local overrides in `~/.config/herdr/agent-detection/<agent>.toml` win. `herdr agent explain` shows the matched rule.
3. **Integration reports** (when installed or self-reporting): hook-reported state is "lifecycle authority" and replaces screen reading for state, though a visible blocker on screen can override a non-blocked report. PTY activity is the normal `working` authority.
4. Unknown-but-known agent with no matching rule falls back to `idle` (`default_known_agent_idle_fallback`). Docs state that unfamiliar approval dialogs can therefore read `idle`, and that idle is not authorization to type. (agents.mdx "Blocked state")

Integrations per harness (`src/integration/`): Claude, Codex, Kimi, Cursor, Copilot, etc. install a shell/PowerShell hook (`herdr-agent-state.sh`, which calls the socket from embedded python3); Pi, OMP, OpenCode, Kilo install a TypeScript extension/plugin. Each carries an integer `*_INTEGRATION_VERSION`. For Claude, the shipped hook handles only `SessionStart` and reports the session reference; Claude's state still comes from the screen (agent-guide.md: "Claude's state still comes from screen detection").

### Wait, completion, failure

- `agent.wait {target, until[], timeout_ms}`: server-owned, event-driven; pins the resolved pane occupant so a replacement cannot satisfy it; returns immediately if status already matches. Default `until` = `idle|done|blocked`. No default timeout.
- `agent.prompt {target, text, wait?{until, timeout_ms}}`: submits text plus encoded Enter as one ordered write (honours bracketed paste), and optionally starts the wait in the same request. Rejects `blocked` agents with `agent_blocked` before sending. With `wait`, from a non-working state it requires observed `working`/`blocked` within 5s, else `agent_prompt_stalled`. (`src/api/wait.rs:15` `AGENT_PROMPT_EFFECT_TIMEOUT_MS = 5000`.)
- It tracks lifecycle state, **not a turn**: if the agent is already working, finishing the active turn can satisfy the wait. `completion_seq` on `AgentInfo` marks an idle transition as completed work, distinct from startup or session change. Timeouts and stalls "do not prove no input was sent"; docs tell callers not to blindly resubmit. (agent-automation.mdx)
- `done` = idle and not yet seen; seen is per server (CLI) and per TUI client. `unknown` = agent present but unclassifiable; "does not prove successful completion".
- **Failure**: no state for it. Observable signals are `pane.exited` event (payload only `pane_id`), `agent_not_running` error on a wait whose occupant went away, `agent_not_ready` at start, `timeout`. Exit codes of the agent process are not exposed in the schema.
- Known gaps the docs concede: Antigravity and Grok go `idle` while background tasks/subagents run; unfamiliar approval UIs may read `idle`.

### Read and input

- `agent.read {target, source, lines, format, strip_ansi}`; sources `visible | recent | recent-unwrapped | detection` (`detection` is the exact snapshot the classifier sees). For an idle recognized full-screen agent, large reads scroll the alternate screen via mouse-wheel to collect history, else `agent_not_idle`.
- `agent.send_keys` takes logical keys (`esc`, `ctrl+c`, ...), validated before any byte is written.
- Fallback for unrecoverable output: ask the agent to write Markdown to a temp file and read that.

### Session resume

`pane.report_agent {pane_id, source, agent, state, message?, seq?, agent_session_id?, agent_session_path?, resume_argv?}` (`src/api/schema/panes.rs:405`). `resume_argv` is a command Herdr runs in the restored pane after a server restart (first element a plain command name, no apostrophes or control chars, at most 64 args and 8 KiB). `seq` must increase per source or the report is ignored. `source` must not start with `herdr:` for third parties. `pane.release_agent` clears it. Herdr itself maps per-harness native resume commands (`claude --resume <id>`, `codex resume <id>`, `opencode --session <id>`, `pi --session`, ...). (session-state.mdx, add-herdr-support.mdx)

## 2. CLI and socket API

**Transport**: newline-delimited JSON over a local socket (Unix socket / Windows equivalent), `{"id","method","params"}` request and `{"id","result"|"error":{code,message}}` response (socket-api.mdx, wire format section; `src/api/client.rs:31,187`). Socket path from `HERDR_SOCKET_PATH`; each pane inherits `HERDR_ENV=1`, `HERDR_PANE_ID`, `HERDR_WORKSPACE_ID`, `HERDR_TAB_ID`, `HERDR_BIN_PATH`, `HERDR_SOCKET_PATH`. Sessions are separate namespaces (`--session`). `herdr api schema --json` emits a JSON Schema for the whole protocol (generated via schemars; checked in as `docs/next/api/herdr-api.schema.json`). IDs are opaque and scoped to one server: `w1`, `w1:t1`, `w1:p1`, never reused after close.

**Method registry** (`src/api/schema.rs:47-147`, a macro table `Variant(Params) => "dot.name", ...`): about 100 methods:

- server: `ping`, `server.stop`, `server.live_handoff`, `server.reload_config`, `server.agent_manifests`, `server.reload_agent_manifests`
- workspace/worktree/tab: create, list, get, focus, rename, move, close; `worktree.create|open|remove|list`
- pane: split, swap, move, zoom, resize, layout, neighbor, read, `send_text|send_keys|send_input`, `wait_for_output`, `process_info`, `report_agent`, `report_agent_session`, `report_metadata`, `release_agent`, `clear_agent_authority`, close
- agent: `list, get, read, explain, send_keys, rename, focus, start, prompt, wait, view.set|clear`
- events: `events.subscribe` (long-lived stream), `events.wait`; kinds include `pane.exited`, `pane.agent_detected`, `pane.agent_status_changed`, `pane.output_changed`, `layout.updated`
- `session.snapshot` (bootstrap for clients that cache state), `layout.export|apply`, `integration.*`, `plugin.*`, `notification.show`.

**CLI** is a projection of the same schema: params structs are declared with a `cli_params!` macro so each field carries its CLI binding (`src/cli/projection.rs`, `src/cli/agent/bindings.rs`); commands are `herdr {agent,pane,workspace,tab,worktree,terminal,notification,integration,session,machine,api,server}`. Output is JSON on stdout; server errors are JSON on stderr with exit 1; syntax errors exit 2. `--machine <label>` forwards a command to a saved SSH machine's server without a TUI.

**What sits behind each**: a single in-process app task. Requests are sent to the app over a channel (`ApiRequestSender`, `dispatch_to_app_with_timeout`) and answered from in-memory workspace/pane state; waits and subscriptions are handled in connection threads (`src/api/wait.rs`, `src/api/subscriptions.rs`) that poll or listen on an `EventHub`. Mutating methods carry two flags in the registry (`changes_ui`, a second flag), used to decide client redraws.

**Plugins** (`plugin.*`) add actions and panes; **metadata tokens** (`pane.report_metadata`, `workspace.report_metadata`) carry display text separately from lifecycle state.

## 3. Overlap with a scheduler

Present:

- **Sessions outlive the client**: the server keeps PTYs running after detach or SSH loss. After a *server* restart processes die; layout returns and eligible agents are re-launched via resume commands. `herdr update --handoff` (experimental) transfers live PTYs to a new server. (session-state.mdx)
- **Dispatch to a live agent**: `agent.prompt`, and `agent.start` into a prepared pane. Callers do the choosing and placing.
- **Coordination between agents**: an agent in a pane can drive the CLI (a shipped skill `skills/herdr/SKILL.md`, gated on `HERDR_ENV=1`) to split panes, start peers, prompt them, wait for `blocked`, and read output. Remote control of other machines via `--machine`.
- **Worktrees**: first-class `worktree.*` methods.
- **Waiting**: `agent.wait`, `pane.wait_for_output` (substring or regex), `events.wait`/`events.subscribe`.

Absent (as far as the code and docs show): job queue, DAG or dependencies, retry/backoff, concurrency limits, scheduled or triggered runs, durable run history, per-run outcome or exit code, idempotency keys, sandboxing or credentials. It is an interactive operator surface; state is the live process tree plus a restore snapshot (`session.json`, up to 48 layout snapshots).

## 4. Takeaways for the foreman seams

These are readings of the source, not decisions.

1. **Harness seam shape differs**: Herdr never abstracts "run this agent non-interactively and give me a result". It abstracts "observe and type into a TUI". A foreman harness seam that drives opencode/Claude Code headlessly would have no counterpart in Herdr; the nearest parallel is `agent.start` + `agent.prompt` + `agent.wait` + `agent.read`.
2. **Data-driven per-harness differences**: Herdr keeps harness variation in versioned manifests plus per-harness installers rather than in code behind a trait. The closed `Agent` enum still means a new harness needs a binary release (docs: "Adding a completely new agent still requires a Herdr binary update") unless it self-reports.
3. **Self-report protocol as the open extension**: `source`, `agent`, `state`, monotonically increasing `seq`, optional `resume_argv`, explicit `release`. The `seq` rule and the "release or be noticed at idle prompt" safety net are an idempotency and staleness design worth comparing with foreman's handoff markers.
4. **State vocabulary lacks failure**; callers must infer it. Settled-state wait is also not turn-scoped (`completion_seq` was added to patch this).
5. **Identity pinning**: waits bind to the resolved occupant so a replaced agent cannot satisfy them; IDs are never reused.
6. **Versioning**: integrations carry integer versions; endpoints carry a generation; clients and servers can differ and docs tell callers to check `herdr status` before relying on new features. The schema is generated and published as JSON Schema.
