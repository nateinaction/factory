# opencode harness adapter

foreman maps its guarantees to opencode v1.19.3 in `foreman/internal/opencode` (pure: command line, environment, stream parsing, retry rules) and `foreman/internal/foreman/agent.go` (the retry loop and state directory).

| Guarantee | opencode mechanism |
|---|---|
| Named agent per agent role, model from `factory_models` | `agent.<role>` in the rendered `opencode.json`, run as `opencode run --agent <role>` |
| Context cap | the agent's model is an alias (`<role>-200k`) whose `limit.input` is the cap |
| Compaction on or off per agent role | `OPENCODE_CONFIG_CONTENT={"compaction":{"auto":<bool>},"provider":...}` set per run |
| No repository config or skills | `OPENCODE_DISABLE_PROJECT_CONFIG=1`, `OPENCODE_DISABLE_EXTERNAL_SKILLS=1` |
| Fresh state, deleted after the run and its retro | `OPENCODE_DB` in `<work root>/state-<role>-<issue>/`; `:memory:` for the Reviewer |
| Retro in the run's session | `--session <id>` from the run's JSON stream, never `--continue` |
| The OpenRouter key never reaches the model | `OPENROUTER_API_KEY` is never set. `OPENCODE_CONFIG_CONTENT` points `provider.openrouter.options.baseURL` at the run's egress proxy, which forwards `/api/v1` to the loopback key proxy that adds `Authorization` |
| Usage | the key proxy sums each response's `usage` (tokens, cached tokens, cost); `step_finish` is the fallback |
| Every model process runs under srt | `opencode run` is wrapped by `srt --settings <run>/srt-settings.json`; see `foreman/docs/sandbox.md` |
| Contacted domains | the run's egress proxy records each host for the usage line's `domains` |

## The OpenRouter key proxy

`foreman/internal/openrouter` is a loopback forwarding proxy that foreman starts for each run and
stops when the run ends. It binds `127.0.0.1` on a free port, holds the key, adds
`Authorization: Bearer <key>` to every request, and forwards only to `https://openrouter.ai/api/v1`.
A request naming any other path or destination is refused, and the proxy's client ignores
`HTTPS_PROXY`, so the key cannot be sent anywhere else. A proxy that cannot bind fails the run, with
no unsandboxed fallback.

opencode sees only `http://127.0.0.1:<port>/api/v1` in `provider.openrouter.options.baseURL`, where
that port is the run's egress proxy (`foreman/internal/egress`). The egress proxy forwards the
`/api/v1` path to this key proxy and enforces the role's egress policy. The
proxy reads `usage.prompt_tokens`, `usage.completion_tokens`, `usage.prompt_tokens_details.cached_tokens`
and `usage.cost` from each response (the last SSE message when streaming), and the run's usage line
takes those totals over the adapter's own.

Checked against the pinned v1.19.3: opencode refuses a run with no provider key. The adapter sets the
placeholder `provider.openrouter.options.apiKey` (`factory-egress-proxy`, never the real key), which
opencode sends and the egress proxy's forward to the key proxy replaces. The acceptance test in
`foreman/internal/foreman/acceptance_test.go` exercises this under srt.


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
