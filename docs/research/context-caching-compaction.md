# Context caching, sessions and compaction in opencode and OpenRouter

Research for the issue "Context caching, sessions and compaction in opencode and
OpenRouter" (map: "Wayfinder: agent roles for the factory"). It feeds
"Context caching and compaction per agent role", which makes the decisions.

**Question.** With opencode v1.19.3 (the pinned version) running models through
OpenRouter, what can the factory control about an agent's context? That covers
prompt caching, resuming sessions across headless runs, compaction, and capping the
context window. It also asks how other harnesses (pi, Claude Code, Codex CLI, Aider)
and other agent factories handle the same things.

## Sources and versions

- **opencode** `v1.19.3`: <https://github.com/anomalyco/opencode/tree/v1.19.3>.
  The release tarball was read in full. `v1.19.4` (the current "Latest") has the
  same content in every file cited below. `v2.2.0` (released 2026-10-27) is a
  rewrite, and the places where it differs are noted.
  The factory installs from `github.com/sst/opencode`, which redirects here.
  The factory's model is `openrouter/deepseek/deepseek-v4.1-flash`
  (`group_vars/factory.yml`).
- **OpenRouter**: [prompt caching guide](https://openrouter.ai/docs/guides/best-practices/prompt-caching).
- **Provider caching docs**: [DeepSeek](https://api-docs.deepseek.com/guides/kv_cache),
  [Anthropic](https://platform.claude.com/docs/en/build-with-claude/prompt-caching),
  [OpenAI](https://developers.openai.com/api/docs/guides/prompt-caching),
  [Gemini](https://ai.google.dev/gemini-api/docs/caching).
- **models.dev** catalog (`https://models.dev/api.json`, read 2026-10-27), where
  opencode gets model limits.
- **pi** `v1.4.0`: <https://github.com/earendil-works/pi/tree/v1.4.0>
  (formerly `badlogic/pi-mono`). Docs and source were read at the tag.
- **Claude Code**: [headless](https://code.claude.com/docs/en/headless),
  [sessions](https://code.claude.com/docs/en/sessions),
  [env vars](https://code.claude.com/docs/en/env-vars),
  [model config](https://code.claude.com/docs/en/model-config).
- **Codex CLI**: [config reference](https://learn.chatgpt.com/docs/config-file/config-reference),
  [non-interactive mode](https://learn.chatgpt.com/docs/non-interactive-mode).
- **Aider**: [options](https://aider.chat/docs/config/options.html).

**Evidence tags.** Each claim says where it comes from:
**[src]** confirmed in source at the tag (with a link to the file and line),
**[doc]** documented by the owner, **[practice]** reported in practice by
third parties, **[unconfirmed]** inferred but not verified.

`oc` below is short for `https://github.com/anomalyco/opencode/blob/v1.19.3/packages`.

## Summary

1. **Caching is automatic for the factory's model, and opencode sends no breakpoints for it.**
   DeepSeek through OpenRouter caches prefixes automatically. Reads cost 0.1x input,
   and writes cost the same as normal input. opencode v1.19.3 adds `cache_control`
   only for Claude/Anthropic models (and Alibaba). For DeepSeek it sends nothing
   extra, and nothing extra is needed.
2. **Headless resume works.** `opencode run --session <id>` and `--continue` resume a
   session stored in one SQLite file, `~/.local/share/opencode/opencode.db`.
   `--continue` is scoped to the project *and the exact working directory*. Because
   the factory clones into a fresh directory each run, only `--session <id>` can
   resume across runs. Sessions move between hosts through `opencode export` /
   `opencode import` or a per-run `OPENCODE_DB`.
3. **Auto-compaction is on by default. It is global, not per agent.** It fires at
   `limit.context − min(limit.output, 32000)`. For the factory's model that is
   1,048,576 − 32,000 = **1,016,576 tokens**, so in practice it never fires. The
   `compaction.reserved` knob is **ignored** unless the model also has `limit.input`.
   Compaction can be turned off globally, or per run with `OPENCODE_DISABLE_AUTOCOMPACT`.
   Without compaction, a run that overflows ends with a `ContextOverflowError` and
   exit code 1.
4. **The window can be capped per agent, but only indirectly.** Define a model alias
   under `provider.openrouter.models` with the same `id` and a smaller
   `limit.context`, then point the agent's `model` at that alias. The cap is
   enforced only by triggering compaction. opencode never truncates a request, so a
   cap with compaction off does nothing.
5. **Steps and compaction interact.** A compaction pass runs inside the same loop and
   uses up step-cap iterations. When the last step arrives, tools are switched off
   and the agent is told to summarise.
6. **Other harnesses do not justify revisiting the harness choice.** pi has the
   better caching story for OpenRouter (it sends `x-session-id` for sticky routing,
   can keep the cache warm, and supports per-model compaction overrides and
   deterministic session ids). But none of this matters much for an automatically
   cached DeepSeek model, and opencode can match the rest through config.

## 1. Prompt caching

### Which providers cache, and how

From OpenRouter's caching guide **[doc]**:

| Provider (via OpenRouter) | Mode | Write cost | Read cost | Lifetime |
|---|---|---|---|---|
| DeepSeek | automatic | 1x input | 0.1x | "a few hours to a few days" (DeepSeek docs) |
| OpenAI | automatic (≥1024 tokens) | free (pre-GPT-5.6) / 1.25x | 0.25x–0.5x | ~5–10 min idle, up to 1 h; extended up to 24 h |
| Gemini 2.5+ | implicit, automatic | free | 0.25x | "on average 3–5 minutes" |
| Grok, Moonshot, Groq (Kimi K2), Z.AI | automatic | free | 0.25x / 0.25x / 0.5x / ~0.2x | not stated |
| Anthropic Claude | **needs `cache_control`** (per block, or one top-level field) | 1.25x (5 min) / 2x (1 h) | 0.1x (some newer models lower) | 5 min default, 1 h option |
| Alibaba Qwen | **needs `cache_control`** | 1.25x | 0.1x | 5 min |
| Gemini explicit | needs `cache_control` | input + storage | 0.25x | 5 min, not refreshed |

- **Sticky routing [doc].** After a cache hit, OpenRouter sends later requests for
  the same model to the same provider, as long as cached reads are cheaper there.
  A conversation is identified by hashing the first system message and the first
  non-system message. A `session_id` body field or `x-session-id` header overrides
  that key. Stickiness expires after 10 minutes idle and is bypassed when you set
  `provider.order`.
- **Observability [doc].** `usage.prompt_tokens_details.cached_tokens` and
  `cache_write_tokens`, plus a `cache_discount` field. DeepSeek's native API reports
  `prompt_cache_hit_tokens` and `prompt_cache_miss_tokens`.
- **DeepSeek [doc].** Caching is "enabled by default for all users". It needs an
  exact prefix match on stored "cache prefix units" and is best effort. The docs
  make no latency claim.
- **Anthropic [doc].** Up to 4 breakpoints per request. Each breakpoint looks back
  20 blocks. The minimum cacheable prefix is 512–4,096 tokens depending on the
  model. Changing tools invalidates everything after them. The lifetime counts from
  the start of the request, so a long streamed response eats into the 5 minutes.
  Long prompts generally get a faster time-to-first-token.
- **OpenAI [doc].** Reads are "discounted up to 95%". A cache hit "does not
  guarantee ... lower latency".
- **Factory model prices [doc, models.dev].** `openrouter/deepseek/deepseek-v4.1-flash`:
  context 1,048,576, output 131,072, input $0.14/M, cache read $0.0042/M
  (models.dev lists the read at 0.03x, below OpenRouter's generic 0.1x for
  DeepSeek, so treat the exact ratio as unconfirmed).

### What opencode v1.19.3 sends

- **Breakpoints only for Anthropic-family models [src].** `applyCaching` marks the
  first two system messages and the last two non-system messages with
  `cacheControl: {type: "ephemeral"}` (also `openrouter`, `bedrock`,
  `openaiCompatible` and `copilot` spellings)
  ([oc/opencode/src/provider/transform.ts#L358-L405](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/provider/transform.ts#L358-L405)).
  It runs only when the provider, model id or API id contains `anthropic`/`claude`,
  or the SDK is `@ai-sdk/anthropic` / `@ai-sdk/alibaba`
  ([#L466-L484](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/provider/transform.ts#L466-L484)).
  So `openrouter/anthropic/claude-*` gets 4 breakpoints, and
  `openrouter/deepseek/*` gets none. DeepSeek doesn't need any.
- **No TTL control [src].** The marker is always `{type: "ephemeral"}` (5 minutes).
  The schema has no setting for a 1 h TTL. A per-model `options` block can add
  provider options, but whether that can override the TTL is **[unconfirmed]**.
- **No `session_id` for OpenRouter [src].** For `@openrouter/ai-sdk-provider`,
  opencode only sets `usage: {include: true}` (and reasoning effort for Gemini)
  ([#L1250-L1257](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/provider/transform.ts#L1250-L1257)).
  `prompt_cache_key` / `promptCacheKey` = session id goes only to OpenAI, Azure,
  xAI, Mistral, DeepInfra, Cerebras and Venice, or wherever `setCacheKey: true` is
  set ([#L1323-L1336](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/provider/transform.ts#L1323-L1336)).
  OpenRouter's default stickiness key (the first system message plus the first user
  message) is stable within a session anyway, so this matters little **[unconfirmed]**.
- **Compaction breaks the cache [src].** The compaction request flattens the history
  into a single text user message
  ([oc/opencode/src/session/compaction.ts#L380](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/session/compaction.ts#L380)).
  So it reads the whole history uncached, and the turn after it starts a new prefix.
- **Cross-run reuse [unconfirmed].** A new run hits the cache only if its system
  prompt and tool list are byte-identical to an earlier run's and that cache entry
  is still alive. For DeepSeek that is hours. For Anthropic it is 5 minutes. Nobody
  measured whether opencode's system prompt is stable across runs (it carries
  environment details).

### Newer versions

v2.2.0 replaces this with a cache policy that is **on by default for every
provider**. Its `"auto"` setting places breakpoints on the last tool definition,
the first and last system parts, and the conversation tail, capped at 4. It also
accepts a `{ ttl }` value and `"none"`
([packages/ai/src/cache-policy.ts#L1-L35 at v2.2.0](https://github.com/anomalyco/opencode/blob/v2.2.0/packages/ai/src/cache-policy.ts#L1-L35)).
The OpenRouter protocol in v2.2.0 emits `cacheControl` and declares a `session_id`
routing option
([packages/ai/src/providers/openrouter.ts#L75-L80, L123](https://github.com/anomalyco/opencode/blob/v2.2.0/packages/ai/src/providers/openrouter.ts#L75-L80))
**[src, read only at the surface]**.

## 2. Sessions across runs

### Flags (`opencode run`)

- `--continue/-c` ("continue the last session"), `--session/-s <id>`, and `--fork`
  (needs one of the other two)
  ([oc/opencode/src/cli/cmd/run.ts#L147-L160](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/cli/cmd/run.ts#L147-L160),
  [#L426-L428](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/cli/cmd/run.ts#L426-L428))
  **[src][doc]**. There is also `--title`, `--agent`, `--model` and
  `--format json`. There is no `--session-id` to choose the id up front.
- **`--session <id>` [src]** loads the session with `session.get` and exits 1 with
  "Session not found" if it's missing
  ([run.ts#L457-L490](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/cli/cmd/run.ts#L457-L490)).
  Lookup by id ignores the directory
  ([oc/opencode/src/session/session.ts#L540-L544](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/session/session.ts#L540-L544)).
- **`--continue` [src]** picks the most recently updated **root** session
  (`!parentID`) from `session.list()`
  ([run.ts#L493](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/cli/cmd/run.ts#L493)).
  The local SDK client is built with the run's `directory`
  ([#L946-L950](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/cli/cmd/run.ts#L946-L950)),
  and the SDK copies it into the `directory` query on GETs. The list handler then
  filters by project id **and** directory
  ([oc/opencode/src/server/routes/instance/httpapi/handlers/session.ts#L64-L74](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/server/routes/instance/httpapi/handlers/session.ts#L64-L74),
  [session.ts#L955-L1001](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/session/session.ts#L955-L1001)).
  - So the retro's `opencode run --continue` in the same workdir right after the
    implement run picks up the right session. That is how the factory uses it
    today, and it is correct.
  - A later run in a **new** `fresh_path` clone can't find an earlier session with
    `--continue`. Use `--session <id>`. The id has to be captured from
    `--format json` events or from `opencode session list` **[unconfirmed: exact
    capture method not tested]**.
- **Exit status [src].** In non-attached mode, any `session.error` event sets
  `process.exitCode = 1`
  ([run.ts#L822-L832](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/cli/cmd/run.ts#L822-L832)).

### Where state lives

- **One SQLite database [src]**: `$XDG_DATA_HOME/opencode/opencode.db`
  (`~/.local/share/opencode/opencode.db` for the factory user)
  ([oc/core/src/database/database.ts#L43-L54](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/core/src/database/database.ts#L43-L54),
  [oc/core/src/global.ts#L11](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/core/src/global.ts#L11)).
  `OPENCODE_DB=<abs path | name | :memory:>` overrides it per process
  ([oc/core/src/flag/flag.ts#L47](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/core/src/flag/flag.ts#L47)).
  That allows one DB per agent role or per issue, or an in-memory DB for a
  Reviewer that must never resume.
- **Project id** appears to come from the repository's root commit(s)
  (`git rev-list --max-parents=0 HEAD`,
  [oc/core/src/git.ts#L211-L212](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/core/src/git.ts#L211-L212)).
  So fresh clones of the same repo share a project **[src, not traced end to end]**.
- **Keeping it on the host.** The DB persists in the factory user's home between
  runs unless something deletes it. Nothing in the factory cleans it today
  **[src: repo]**.
- **Moving between hosts [src].** `opencode export [sessionID]` writes JSON (with
  `--sanitize` to redact)
  ([oc/opencode/src/cli/cmd/export.ts#L223-L235](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/cli/cmd/export.ts#L223-L235)).
  `opencode import <file>` upserts the session, **rewriting** `projectID`,
  `directory` and `path` to the importing instance, and inserts messages and parts
  with `onConflictDoNothing`
  ([oc/opencode/src/cli/cmd/import.ts#L47-L110](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/cli/cmd/import.ts#L47-L110)).
  So an import is idempotent, and the session can then be resumed with
  `--session <id>` in the new clone. Copying the whole `opencode.db` also works
  **[unconfirmed]**.
- **Doc/source divergence.** `cli.mdx` says import accepts "an OpenCode share URL"
  ([oc/web/src/content/docs/cli.mdx#L484-L496](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/web/src/content/docs/cli.mdx#L484-L496)),
  but the source refuses share URLs because sharing was discontinued
  ([import.ts#L51-L52](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/cli/cmd/import.ts#L51-L52)).

### Newer versions

v2.2.0 moves sessions to new `session_v2` / `session_message` tables, with a
migration from v1
([packages/core/src/database/v1-migration.bun.ts at v2.2.0](https://github.com/anomalyco/opencode/blob/v2.2.0/packages/core/src/database/v1-migration.bun.ts)).
`opencode run` now lives under `packages/cli`. The flags were not re-verified at
v2.2.0 **[unconfirmed]**.

## 3. Compaction

### Configuration (global only)

- **Schema [src][doc].** Top-level `compaction` takes `auto` (default true),
  `prune` (default false), `tail_turns`, `preserve_recent_tokens` and `reserved`
  ([oc/core/src/v1/config/config.ts#L149-L168](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/core/src/v1/config/config.ts#L149-L168);
  [config.mdx#L746-L763](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/web/src/content/docs/config.mdx#L746-L763),
  which documents only `auto`, `prune` and `reserved`).
- **Not per agent [src].** The agent schema has no compaction field
  ([oc/core/src/v1/config/agent.ts#L12-L41](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/core/src/v1/config/agent.ts#L12-L41)).
  The hidden `compaction` agent (`agent.compaction`) chooses **which model and
  prompt write the summary**, falling back to the session's model
  ([compaction.ts#L358-L361](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/session/compaction.ts#L358-L361);
  [agents.mdx#L95-L99](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/web/src/content/docs/agents.mdx#L95-L99)).
  It does not decide whether compaction happens.
- **Per run [src].** `OPENCODE_DISABLE_AUTOCOMPACT` and `OPENCODE_DISABLE_PRUNE`
  force `auto: false` / `prune: false`
  ([oc/opencode/src/config/config.ts#L593-L598](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/config/config.ts#L593-L598)).
  `OPENCODE_CONFIG=<file>` or `OPENCODE_CONFIG_CONTENT=<json>` merge in a whole
  config per process
  ([flag.ts#L21-L22](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/core/src/flag/flag.ts#L21-L22),
  [config.ts#L415-L417, L482-L484](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/config/config.ts#L415-L417)).
  Since the factory runs one `opencode run` per agent role, **per-run config is
  effectively per agent role**.

### When it fires

- **Threshold [src]**
  ([oc/opencode/src/session/overflow.ts#L8-L34](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/session/overflow.ts#L8-L34)):
  - With `limit.input` set: compact when tokens ≥ `limit.input − reserved`, where
    `reserved` = `compaction.reserved ?? min(20000, maxOutput)`.
  - Without `limit.input`: compact when tokens ≥ `limit.context − maxOutput`.
    **`compaction.reserved` is ignored in this branch.**
  - `maxOutput` = `min(limit.output, 32000)`
    ([transform.ts#L1481-L1483](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/provider/transform.ts#L1481-L1483),
    `OUTPUT_TOKEN_MAX = 32_000` at [#L18](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/provider/transform.ts#L18)).
  - The token count is the last response's `total`, or input + output + cache
    read + cache write.
  - `limit.context === 0` (a model missing from the catalog with no configured
    limit) **disables compaction entirely**.
- **For the factory's model [src + models.dev]:** there is no `limit.input`, so the
  threshold is 1,048,576 − 32,000 = 1,016,576 tokens. In practice, the factory
  never compacts today.
- **Where it's checked [src].** After each finished step
  ([oc/opencode/src/session/processor.ts#L489-L496](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/session/processor.ts#L489-L496))
  and at the top of each loop iteration
  ([oc/opencode/src/session/prompt.ts#L1160-L1167](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/session/prompt.ts#L1160-L1167)).
  A provider `ContextOverflowError` also triggers compaction
  ([processor.ts#L621-L631](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/session/processor.ts#L621-L631)).
- **What it keeps [src].** A summary (the earlier summary is fed back in), plus
  recent turns verbatim up to `preserve_recent_tokens`. The default is 25% of the
  usable window, clamped to 2k–15k tokens
  ([compaction.ts#L28-L33, L115-L120](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/session/compaction.ts#L115-L120)).
  Tool outputs are truncated to 2,000 characters in the summarisation input. After
  compacting, opencode injects "Continue if you have next steps…" and keeps going.
  If even the compaction request overflows, the run stops with a
  `ContextOverflowError` ([compaction.ts#L450-L459](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/session/compaction.ts#L450-L459)).
- **Prune [src].** This is separate from compaction and off by default. When on, it
  clears old tool outputs beyond the most recent 40k tokens' worth, if that frees
  more than 20k tokens. `skill` outputs are protected
  ([compaction.ts#L28-L31, L273-L316](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/session/compaction.ts#L273-L316)).

### With compaction off

- **[src]** `isOverflow` returns false, so nothing compacts proactively.
  opencode keeps sending the full history until the provider rejects it. Then
  `ContextOverflowError` is recorded on the message (`finish: "error"`), the session
  goes idle ([processor.ts#L621-L628](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/session/processor.ts#L621-L628)),
  and `opencode run` exits 1. Whatever the agent committed so far stays on disk.
  The factory's "no new commits means failure" check still applies.

### Interaction with the step cap

- **[src]** `agent.steps` (`maxSteps` is the deprecated name) caps loop iterations
  **per `opencode run` invocation**. The counter starts at 0 in each run
  ([prompt.ts#L1085](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/session/prompt.ts#L1085)).
  So a resumed session gets a fresh budget.
- **[src]** `step++` happens before the loop handles a pending compaction task
  ([#L1132](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/session/prompt.ts#L1132),
  [#L1149-L1158](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/session/prompt.ts#L1149-L1158)),
  so **each compaction uses at least one step**.
- **[src][doc]** At `step >= steps`, the request carries `MAX_STEPS_PROMPT` (tools
  disabled, "respond with text only", summarise and list what's left)
  ([#L1178-L1179, L1281](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/session/prompt.ts#L1178-L1179);
  [agents.mdx#L291-L310](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/web/src/content/docs/agents.mdx#L291-L310)).
- **Consequence [unconfirmed].** A small window plus compaction plus a tight step
  cap can burn steps on summaries. Size `steps` with compaction passes in mind.

### Newer versions

In v2.2.0, compaction settings are `auto`, `buffer` (by default it keeps 15%, and
at least 16k tokens, free), `keep`, and a new `precompute` that summarises in the
background as the context nears the threshold
([packages/core/src/session/compaction.ts#L41-L49 at v2.2.0](https://github.com/anomalyco/opencode/blob/v2.2.0/packages/core/src/session/compaction.ts#L41-L49)).
v1.19.3 already ships a migration that maps `reserved` → `buffer` and
`preserve_recent_tokens` → `keep`
([oc/core/src/v1/config/migrate.ts#L54-L60](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/core/src/v1/config/migrate.ts#L54-L60)).
The percentage buffer would fix v1.19.3's problem of compacting only near 1M tokens.

## 4. Capping the context window per agent role

- **[src][doc]** A configured model's `limit` (`context` and `output` required,
  `input` optional) overrides the catalog field by field
  ([oc/core/src/v1/config/provider.ts#L63-L69](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/core/src/v1/config/provider.ts#L63-L69),
  [oc/opencode/src/provider/provider.ts#L1609-L1613](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/provider/provider.ts#L1609-L1613);
  [providers.mdx#L2632-L2669](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/web/src/content/docs/providers.mdx#L2632-L2669)).
- **[src]** A configured model key may differ from the API id. `id` names the
  catalog/API model, and the entry inherits everything else from it
  ([provider.ts#L1545-L1546](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/opencode/src/provider/provider.ts#L1545-L1546)).
  So a per-agent-role cap looks like this:

  ```json
  {
    "provider": { "openrouter": { "models": {
      "deepseek-v4.1-flash-200k": {
        "id": "deepseek/deepseek-v4.1-flash",
        "limit": { "context": 200000, "input": 200000, "output": 32000 }
      }
    } } },
    "agent": { "implementer": { "model": "openrouter/deepseek-v4.1-flash-200k" } }
  }
  ```

  With `limit.input` set, `compaction.reserved` takes effect, and compaction fires
  at `input − reserved`. Untested end to end **[unconfirmed]**.
- **It is a compaction trigger, not a hard cap [src].** opencode never trims a
  request to `limit.context`. With `auto: false`, a lower `limit.context` changes
  nothing, and the run continues until the provider's real limit.

## 5. Other harnesses

| | **opencode v1.19.3** | **pi v1.4.0** | **Claude Code** | **Codex CLI** | **Aider** |
|---|---|---|---|---|---|
| Breakpoints sent | Anthropic/Claude (and Alibaba) only, 4 markers, 5 min [src] | Anthropic format on Anthropic and `openrouter/anthropic/*` [src] | Anthropic API always, automatic [doc] | n/a: OpenAI caching is automatic | `--cache-prompts` (off by default) [doc] |
| Sticky routing / cache key | `prompt_cache_key` for OpenAI-family only; nothing for OpenRouter [src] | `x-session-id` to OpenRouter by default [src] | n/a | `prompt_cache_key` [practice] | none documented |
| Cache TTL / warming | none | `PI_CACHE_RETENTION=long`; `cacheWarming` (`streaming` default, `idle`) [doc] | `ENABLE_PROMPT_CACHING_1H`, `FORCE_PROMPT_CACHING_5M`, `DISABLE_PROMPT_CACHING*` [doc] | not documented | `--cache-keepalive-pings N` (5 min pings) [doc] |
| Headless resume | `run --session <id>`, `--continue` (same dir), `--fork` [src] | `-p` with `--continue` (per cwd), `--session <path\|id>`, **`--session-id <id>`** (choose the id), `--fork`, `--no-session` [doc] | `-p --resume <id\|path>`, `--continue`, `--fork-session`, `--no-session-persistence` [doc] | `codex exec resume <id>` / `--last`, `--ephemeral` [doc] | `--restore-chat-history` (markdown file) [doc] |
| Session store | SQLite `~/.local/share/opencode/opencode.db`; `OPENCODE_DB`; export/import JSON [src] | JSONL under `~/.pi/agent/sessions/<cwd>`; `--session-dir`, `PI_CODING_AGENT_SESSION_DIR` [doc] | JSONL `~/.claude/projects/<dir>/<id>.jsonl`; `CLAUDE_CONFIG_DIR`; 30-day retention [doc] | `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl` [practice] | `.aider.chat.history.md` [doc] |
| Auto-compaction default | on, at `context − min(output, 32k)` (≈1.016M for the factory model) [src] | on, at `contextWindow − reserveTokens` (16,384) [src][doc] | on; by default at the model's limit, with exceptions [doc] | on; `model_auto_compact_token_limit` (unset = model default) [doc] | summarises past `--max-chat-history-tokens` [doc] |
| Compaction config | global `compaction.{auto,prune,reserved,preserve_recent_tokens,tail_turns}`; env per run [src] | `compaction.{enabled,reserveTokens,keepRecentTokens}` + **per-model** `modelOverrides`; user or project settings [doc] | `DISABLE_AUTO_COMPACT`, `DISABLE_COMPACT`, `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE`, `CLAUDE_CODE_AUTO_COMPACT_WINDOW` (100k–1M) [doc] | `model_auto_compact_token_limit`, `compact_prompt`, profiles [doc] | `--max-chat-history-tokens`, `--weak-model` writes summaries [doc] |
| Disable per role | per process (env or `OPENCODE_CONFIG_CONTENT`), not per named agent [src] | per process (`PI_CODING_AGENT_DIR` or project settings) [doc] | per process (env or `--settings`) [doc] | per profile [unconfirmed] | per invocation |
| Cap window below model | model alias with smaller `limit.context`/`limit.input` [src] | `contextWindow` in `models.json` / `modelOverrides` [doc] | `CLAUDE_CODE_AUTO_COMPACT_WINDOW` or `CLAUDE_CODE_MAX_CONTEXT_TOKENS` [doc] | `model_context_window` [doc] | `--model-metadata-file` [doc] |
| Step / turn cap | `agent.steps` (tools off at the cap) [src] | none found in source [src] | `--max-turns`, `CLAUDE_CODE_MAX_TURNS` [doc] | not checked | n/a (not an agent loop) |
| Works with OpenRouter | yes | yes | Anthropic-protocol endpoints only [unconfirmed for OpenRouter] | `wire_api` must be `responses` [doc]; OpenRouter fit unchecked | yes |

pi details, at the tag:

- **Compaction [src].** The defaults are `enabled: true`, `reserveTokens: 16384`
  and `keepRecentTokens: 20000`
  ([packages/coding-agent/src/core/compaction/compaction.ts#L121-L129](https://github.com/earendil-works/pi/blob/v1.4.0/packages/coding-agent/src/core/compaction/compaction.ts#L121-L129)).
  The trigger is `contextTokens > contextWindow − reserveTokens`
  ([#L268-L269](https://github.com/earendil-works/pi/blob/v1.4.0/packages/coding-agent/src/core/compaction/compaction.ts#L268-L269)).
  On a provider overflow error, pi runs one compact-and-retry recovery
  ([docs/compaction.md](https://github.com/earendil-works/pi/blob/v1.4.0/packages/coding-agent/docs/compaction.md)).
- **OpenRouter caching [src].** On OpenRouter, Anthropic-format cache markers are
  applied only for `anthropic/*` models
  ([packages/ai/src/api/openai-completions.ts#L1652](https://github.com/earendil-works/pi/blob/v1.4.0/packages/ai/src/api/openai-completions.ts#L1652)).
  Session affinity uses the OpenRouter `x-session-id` format
  ([#L1691](https://github.com/earendil-works/pi/blob/v1.4.0/packages/ai/src/api/openai-completions.ts#L1691)).
  Summarisation requests turn off cache writes [doc].

### Does context management give a reason to revisit the harness choice?

Not on the current evidence. ("Which harness runs agent roles?" chose opencode.)

- pi is ahead on three things:
  - OpenRouter sticky routing via `x-session-id`.
  - Cache warming.
  - Deterministic `--session-id` (already noted in "Harness for per-role agents:
    opencode vs pi").
- But the factory's model caches automatically, and OpenRouter's default
  stickiness hash already covers one session.
- Per-role compaction, window caps and resume are all reachable in opencode
  through per-process config, model aliases and `--session <id>`.
- The real opencode gap is v1.19.3's threshold, which is near 1M tokens for
  1M-context models. A model alias with `limit.input` fixes it, and v2.x replaces
  it.
- Revisit only if the factory moves to an Anthropic model **and** measures poor
  cache hit rates. At that point, check whether a configurable TTL or 1 h caching
  matters more than staying on the pin.

## 6. How other agent factories handle context

- **Matt Pocock's skills [doc]** (`mattpocock-skills` 1.3.1, local plugin cache):
  - One ticket per fresh context window, clearing between tickets
    (`docs/engineering/implement.md`).
  - The reviewer "gets a diff and nothing else" (`docs/engineering/retro.md`).
  - At a phase boundary, use this order: continue → `/clear` → `/handoff` →
    subagent → `/compact`. `/compact` is "the default, not the first reach".
  - The "smart zone" is about 150k tokens
    (`skills/engineering/ask-matt/PHASE-BOUNDARIES.md`, line 21).
- **Poteto mode / pstack [doc, via the research for "Agent roles in Poteto mode"]**:
  each fix round, retry or queue item goes to a **fresh subagent** carrying the
  consolidated scope (brief, directives, prior report, branch). An agent is reused
  only when it holds costly local state. "A role such as a PR owner outlives its
  agent."
- **Elephant Goldfish [doc, via the research for "Agent roles in Google's Elephant
  Goldfish model"]**: one long-lived Elephant whose memory is the conversation plus
  checked-in artifacts. Goldfish reviewers start with zero memory on purpose and
  see only the doc and a reading list.
- **Claude Code [doc]**: sessions are resumable and compacted automatically by
  default. Compaction can be switched off or set to fire earlier through env vars.
  Resuming a session after the cache has expired re-reads the whole conversation
  uncached.
- **Copilot cloud agent [practice]**: each PR-comment iteration starts a new agent
  session that pushes another commit. Durable context comes from repo instruction
  files (`.github/copilot-instructions.md`, `AGENTS.md`, `CLAUDE.md`). Session
  carry-over was not confirmed in GitHub's docs.

The common pattern: **durable context lives in files and the issue/PR thread; a
fresh context per round is the default; compaction is the last resort; the
reviewer is always fresh.**

## Suggested for "Context caching and compaction per agent role" (that ticket decides)

- **Reviewer:** fresh each run. Never pass `--continue` or `--session`. Consider
  `OPENCODE_DB=:memory:` so it can't resume even by accident.
- **Implementer across review rounds:** rebuild from the branch plus the latest
  review, as the factory does now. This matches pstack. Use `--continue` only for
  the same-workdir retro.
  If carry-over is wanted, store the session id per issue and resume with
  `--session <id>`. A fresh clone can't use `--continue`.
- **Planner across question rounds:** the issue thread is the durable record.
  Rebuild from it each run, or resume with `--session <id>` if the cache
  savings matter.
- **Window:** give each agent role a model alias with `limit.input` around 150–200k,
  so compaction fires inside the smart zone and not near 1M.
- **Compaction:** allow it for the Planner and the Implementer, and consider
  turning it off for the Reviewer so an overflow fails loudly. Size `steps` to
  leave room for compaction passes.
- **Caching:** nothing to configure for DeepSeek. Watch `cached_tokens` in
  `--format json` output to measure hits before tuning anything.
