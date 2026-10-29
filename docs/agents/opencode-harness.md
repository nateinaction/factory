# opencode harness adapter

foreman maps its guarantees to opencode v1.19.3 in `foreman/internal/opencode` (pure: command line, environment, stream parsing, retry rules) and `foreman/internal/foreman/agent.go` (the retry loop and state directory).

| Guarantee | opencode mechanism |
|---|---|
| Named agent per agent role, model from `factory_models` | `agent.<role>` in the rendered `opencode.json`, run as `opencode run --agent <role>` |
| Context cap | the agent's model is an alias (`<role>-200k`) whose `limit.input` is the cap |
| Compaction on or off per agent role | `OPENCODE_CONFIG_CONTENT={"compaction":{"auto":<bool>}}` set per run |
| No repository config or skills | `OPENCODE_DISABLE_PROJECT_CONFIG=1`, `OPENCODE_DISABLE_EXTERNAL_SKILLS=1` |
| Fresh state, deleted after the run and its retro | `OPENCODE_DB` in `<work root>/state-<role>-<issue>/`; `:memory:` for the Reviewer |
| Retro in the run's session | `--session <id>` from the run's JSON stream, never `--continue` |
| Usage | parsed from `--format json` `step_finish` events |

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
