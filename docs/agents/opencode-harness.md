# opencode harness adapter

foreman maps its guarantees to opencode v1.19.3 in `foreman/internal/opencode` (pure: command line, environment, stream parsing, retry rules) and `foreman/internal/foreman/agent.go` (the retry loop and state directory).

| Guarantee | opencode mechanism |
|---|---|
| Named agent per agent role, model from `factory_models` | `agent.<role>` in the rendered `opencode.json`, run as `opencode run --agent <role>` |
| Context cap | the agent's model is an alias (`<role>-200k`) whose `limit.input` is the cap |
| Compaction on or off per agent role | `OPENCODE_CONFIG_CONTENT={"compaction":{"auto":<bool>},"provider":...}` set per run |
| Skills: only the factory's own, exactly the role's list | `agent.<role>.permission.skill` allows the role's skills and denies the rest; `OPENCODE_DISABLE_EXTERNAL_SKILLS=1` |
| Permissions: deny by default, never `ask` | `permission.*` in `opencode.json`, rendered per agent role from `group_vars` |
| No repository config | `OPENCODE_DISABLE_PROJECT_CONFIG=1` |
| Fresh state, deleted after the run and its retro | `OPENCODE_DB` in `<work root>/state-<role>-<issue>/`; `:memory:` for the Reviewer |
| Step cap and deadline per agent role | `agent.<role>.steps` from `factory_agent_limits`, and foreman's own count and timeout; see below |
| Retro in the run's session | `--session <id>` from the run's JSON stream, never `--continue` |
| The OpenRouter key never reaches the model | `OPENROUTER_API_KEY` is never set. `OPENCODE_CONFIG_CONTENT` points `provider.openrouter.options.baseURL` at the run's loopback proxy, which adds `Authorization` |
| Usage | the proxy sums each response's `usage` (tokens, cached tokens, cost); `step_finish` is the fallback |

## The OpenRouter key proxy

`foreman/internal/openrouter` is a loopback forwarding proxy that foreman starts for each run and
stops when the run ends. It binds `127.0.0.1` on a free port, holds the key, adds
`Authorization: Bearer <key>` to every request, and forwards only to `https://openrouter.ai/api/v1`.
A request naming any other path or destination is refused, and the proxy's client ignores
`HTTPS_PROXY`, so the key cannot be sent anywhere else. A proxy that cannot bind fails the run, with
no unsandboxed fallback.

opencode sees only `http://127.0.0.1:<port>/api/v1` in `provider.openrouter.options.baseURL`. The
proxy reads `usage.prompt_tokens`, `usage.completion_tokens`, `usage.prompt_tokens_details.cached_tokens`
and `usage.cost` from each response (the last SSE message when streaming), and the run's usage line
takes those totals over the adapter's own.

Not yet checked against the pinned v1.19.3: whether opencode starts a run with no `OPENROUTER_API_KEY`
or provider auth entry. If it insists on one, set a placeholder `provider.openrouter.options.apiKey`
(a literal such as `proxy`, never the key): opencode sends it and the proxy replaces the header.


## How foreman enforces the step cap and the deadline

The deadline is a `context.WithTimeout` around every attempt of the run, from
`factory_agent_limits.<role>.deadline_minutes` (or `factory_retro_deadline_minutes` for the retro).
A run that runs out of time fails.

The step cap is counted twice. opencode stops the agent at `agent.<role>.steps`, and foreman counts
the `step_finish` events in the stream across every attempt of the run. A run whose success would
land at or past the cap fails with the `step cap` failure kind, which is never retried, so an agent
that opencode has forced to summarise does not count as a success. Assume `step_finish` marks one
agentic step; check that against v1.19.3 output on a worker host.

Hitting either fails the issue to `factory:failed`, as the spec says. A retro that hits its cap or
deadline is dropped, logged, and never fails the run.

## The retro epilogue

After foreman has applied the run's output, the same dagu run resumes the model's session by its
explicit id (`--session <id>`, never `--continue`) with the `retro` skill. The retro has its own
step cap and deadline (`factory_retro_steps`, `factory_retro_deadline_minutes`), and the same state
database, so it sees the run as it happened.

- The model writes `retro.json`; `workflow.ParseRetro` validates it strictly, all or nothing.
- The prompt carries only the existing group keys and one-line claims for the reporting agent role
  on that issue, never another retro's evidence. Another agent role's retros are left out, so the
  Reviewer's context never contains the Implementer's.
- foreman posts the retro whole as a marker comment on the issue, keyed by issue, agent role and
  run key (the Implementer's commit SHA, the Reviewer's PR head SHA). A repeat updates that comment.
- A failed run gets a retro too, except a context overflow, running out of credits or an unknown
  model. A dropped or invalid retro is logged and never changes the run's outcome.

## What the permissions cannot express

opencode's `external_directory` is static in the rendered config, so it allows the work root
(`~/work/**`), which holds every run's clone and scratch files, and denies the rest. The sandbox is
the first layer that narrows a run to its own directories; the permission is the second. A denied
action still reaches the dagu run log, because foreman echoes the whole opencode stream.

`git push` and `gh` writes are denied for every agent role. The read-only roles may run a fixed list
of inspection commands; the Implementer and Reviewer may run anything else.


## Does opencode still read AGENTS.md with project config disabled?

Not yet checked against v1.19.3: the dev shell has opencode v2.0.30, not the pinned version, and a
real check needs a model key. To check on a worker host, run `opencode run --agent implementer` in a
clone whose `AGENTS.md` holds a sentinel, with both `OPENCODE_DISABLE_*` variables set, and ask the
model to repeat the sentinel without foreman's prompt.

The answer does not change correctness. foreman puts `AGENTS.md`, `GLOSSARY.md` and
`docs/agents/*.md` into every prompt itself, after the agent role's prompt and before the issue
material, so a repository steers runs the same way either way. If opencode does read `AGENTS.md`,
it is read twice.

## Stream shapes assumed

The adapter assumes `step_finish` events carry `part.tokens.{input,output,cache.read}` and
`part.cost`, every event carries `sessionID`, and failures arrive as `error` events with
`error.name` and `error.data.statusCode` (`ContextOverflowError` for an overflow). Fakes in
`foreman/adapter_test.go` use these shapes. Check them against v1.19.3 output when first run on a
worker host.
