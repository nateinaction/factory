# Rakazo's seams

Question (#179): how does Rakazo abstract its third-party boundaries (model, model provider, sandbox, issue tracker or forge, chat and other vendors), and how do emulators stand in for third-party APIs? Feeds the seam designs and the emulator-per-seam fog of map #178.

Source: `elie222/rakazo` at commit `b6e4bfca8dff48be86e62680a99a6dc5d353447a` (2026-10-31), TypeScript monorepo. Paths are relative to the repo root. The docs and code are the only sources: the video's captions were unavailable (HTTP 429) and were not read. The repo has no chapter or doc titled "plan principles" or "any model, any sandbox"; the closest first-party statement is `VISION.md` ("External services remain optional choices behind provider-neutral contracts, and new providers reuse shared contracts and deterministic offline conformance tests").

## Summary

- Rakazo has **one seam mechanism for everything**: a TypeScript interface per vendor-shaped boundary in `packages/adapter-kit/src/interfaces.ts`, registered by string slot in `AdapterRegistry` (`registry.ts`), with implementations in `packages/adapters/src/`. Every interface starts with `describe(): AdapterDescriptor<Capabilities>` (`id`, `contractVersion`, `adapterVersion`, `capabilities`; `types.ts:37`). Every method takes an `AdapterContext` (operationId, traceId, spaceId, userId, botId, abort signal).
- **Sandbox is the one deep, many-implementation seam**: `SandboxProvider`, about 30 methods, 11 real implementations plus `fake` and `none`. Providers are selected by the `SandboxKind` enum (`packages/contracts/src/ids.ts:54`) through a factory switch (`sandbox-factory.ts`).
- **Model and model provider are not Rakazo interfaces.** They are delegated to Pi (`@earendil-works/pi-ai`, `pi-agent-core`). Rakazo's own `ModelProvider` interface is a thin catalog lister (`listModels()` plus `byok`/`catalog` flags). The runtime seam is `AgentRuntime` (`run`/`abort`), implemented by Pi and a scripted test runtime. A model choice is a plain `{provider, modelId}` struct (`ModelBackupChoiceSchema`, `contracts/src/domain.ts:1394`), not an interface.
- **There is no forge or issue-tracker seam.** Tickets are a built-in Postgres board (`ticket-tools.ts`, backed by `@rakazo/db`). GitHub, Linear and the like arrive as generic **connectors** (`ConnectorProvider`: Composio, Pipedream, MCP, OpenAPI, GraphQL). The nearest forge-shaped seam is `CloudAgentProvider` (launch, get, reply, cancel a remote coding agent that opens a PR), with one real implementation, Cursor.
- **Chat is a seam**: `MessagingSurface`, with platforms mounted behind the Chat SDK (`chat` package) and per-platform webhook verification and payload translation behind `handleWebhook`.
- **Emulators are first-class and live in two places**: in-process fakes beside the adapter (`fake-sandbox.ts`, `cursor`/`messaging`/`sendblue` emulators, `third-party-connector-emulator.ts`) and an HTTP model emulator in `packages/testkit`. Conformance suites run one contract test across real adapters wired to emulated vendor APIs.

## 1. The mechanism

| Piece | Where | Shape |
|---|---|---|
| Interfaces | `packages/adapter-kit/src/interfaces.ts` (633 lines, 28 interfaces) | One interface per boundary |
| Shared types and errors | `adapter-kit/src/types.ts`, `*-errors.ts` | Capability structs, request/result types, typed errors per boundary (`sandbox-errors.ts`, `model-errors.ts`) |
| Registry | `adapter-kit/src/registry.ts` | `Map<string, unknown>`; `register(slot, adapter)`, `get<T>(slot)` throws if absent, `tryGet<T>`. 18 slot names (runtime, sandbox, memory, home, artifacts, secrets, jobs, realtime, notifications, models, connector, connection-auth, runner, voice, web, browser, cloud-agent, auto-review). The slot is not type-linked to the interface: the caller supplies `T` |
| Implementations | `packages/adapters/src/` | Flat directory, one file per vendor |
| Wire contracts | `packages/contracts/src/` | zod schemas shared by API, web and mobile (for example `SandboxKind`) |
| Selection | env vars (`SANDBOX_PROVIDER`, `CLOUD_AGENT_PROVIDER`, `BROWSER_PROVIDER`, `PI_DEFAULT_PROVIDER`, `AGENT_RUNTIME`) and factories (`sandbox-factory.ts`, `cloud-agent-factory.ts`, `browser-provider-factory.ts`, `voice-factory.ts`, ...) | Factories return a `None*` provider with a stated reason when a key is missing, rather than throwing |

Optional capability is expressed two ways: optional methods on the interface (for `SandboxProvider`: `pageBrowser?`, `connectTerminal?`, `isRunning?`, `snapshot`-adjacent probes, `releaseScreen?`) and a `capabilities` struct from `describe()` (`SandboxCapabilities`: `graphical`, `pty`, `snapshots`, `takeover`, `persistentHome`, `multiScreen?`). Docs state that providers opt in per method ("Providers opt in through `SandboxProvider.connectTerminal`", `docs/computer-runtime.md`).

`contractVersion: "1"` is hard-coded in every `describe()` checked (`fake-sandbox.ts:43`, `cursor-cloud-agent.ts:55`); no code was found that reads it to negotiate. Treat it as a label, not an enforced version.

## 2. Sandbox

- **Interface**: `SandboxProvider` (`interfaces.ts:97`). Lifecycle: `provision` (returns a `ComputerRef` before fallible setup), `prepare` (idempotent), `stop`, `destroy`, optional `isRunning`/`isStopped` probes. Work: `execute` (async iterable of `ProcessEvent`), `cancelRunWork?`, `observe`/`act`/`sendInput` for a graphical screen, `listFiles`/`readFile`/`writeFile`, `exportWorkspace`/`importWorkspace` (portable workspace as `PortableFile` streams), `snapshot`, `connectScreen`, optional `connectTerminal`, `pageBrowser`.
- **Reference type**: `ComputerRef {id, botId, kind: SandboxKind, providerRef, fresh?, started?}`. `providerRef` is opaque to the core.
- **Durability stance**: the provider's disk is a cache. Rakazo checkpoints a portable workspace to its own `DATA_DIR` (or an S3-compatible store via `ObjectAgentHomeStore`) and restores it into a replacement computer (`docs/self-host-sandbox-providers.md`, `docs/computer-runtime.md`). This is what makes providers swappable: the seam includes `exportWorkspace`/`importWorkspace`.
- **Implementations** (`SandboxKind`): `none`, `docker` (talks to an in-stack "supervisor" HTTP service that creates sibling containers), `e2b`, `novita`, `daytona`, `createos`, `boat`, `boxd`, `railway`, `kubernetes` (one pod per computer), `wuying`, `desktop` (the host itself; refused unless `NODE_ENV` is development or test), `fake`. E2B and Novita share `RemoteDesktopSandboxProvider` (`remote-desktop-sandbox.ts`), which takes a small SDK shape (`create`, `connect`, `isRunning`, `pause`, `kill`) as a constructor argument; `e2b-sandbox.ts` is 37 lines. Daytona, CreateOS and boxd are 0.9k to 1.9k lines each.
- **Where the interface is wide**: the surface bakes in a graphical desktop with noVNC screens, control leases, takeover and Chromium profiles. A provider without those still implements `observe`/`act`/`connectScreen` or sits behind `none`/placeholder behaviour. The interface is shaped by the product (a bot's computer), not by a minimal "run a command in isolation" contract.
- **Hosted sandboxes supported**: E2B, Novita, Daytona, CreateOS, Boat, boxd, Railway, Wuying (Alibaba), plus self-run Docker and Kubernetes.

## 3. Model and model provider

- **Rakazo's own interface is thin.** `ModelProvider` has `describe()` (`catalog`, `byok`) and `listModels()` (`interfaces.ts:340`). It lists; it does not call.
- **Inference goes through Pi.** `AgentRuntime.run(request, ctx): AsyncIterable<AgentRuntimeEvent>` plus `abort(runId)`; capabilities `streaming`, `compaction`, `tools`, `scripted`. The Pi adapter carries the vendor catalog. Rakazo adds only translation and policy around it: `deployment-model.ts` ("Vendor env names and model ids live here, in the adapter layer, not in core"), `model-connect.ts` (credential building, OpenAI-compatible endpoint probing and SSRF-style private endpoint refusal), `pi-model-compat.ts`, `chat-completions-compat.ts`, `cloudflare-ai-gateway.ts`, `openrouter-output-budget.ts`, `groq-request-budget.ts`.
- **Provider names**: OpenRouter (default, `openai/gpt-6-luna`), Anthropic (`claude-sonnet-5`), Bedrock and Vertex via host credentials (`PI_DEFAULT_CREDENTIALS=host`), any OpenAI-compatible server (a generic `openai_compatible` credential kind), ChatGPT OAuth, and the standard provider key variables Pi reads (`docs/self-host-models.md`).
- **Model as a struct**: `{provider, modelId}` plus per-connection capability facts (vision, enabled ids, thinking levels). Backups are an ordered list of the same struct, capped at 10 (`MAX_MODEL_BACKUPS`).
- **Auto-review** is a separate small seam, `AutoReviewProvider.review()`, with TypeSafe/Jev, OpenRouter and scripted implementations.

## 4. Issue tracker and forge

- No `IssueTracker` or `Forge` interface exists in `interfaces.ts`.
- Tickets are Rakazo's own board in Postgres (`packages/adapters/src/ticket-tools.ts`, `ticket-context.ts`, `@rakazo/db`); the model sees them as built-in tools.
- External trackers and forges are reached as **connectors**: `ConnectorProvider` (`describe`, `discoverTools`, `resolveCall?`, `execute` yielding `ConnectorEvent`s), `ConnectionAuthProvider` (OAuth `begin`/`complete`/`revoke`) and `ManagedConnectorProvider` (adds a catalog and connection lifecycle). Implementations are catalog services (Composio, Pipedream), HTTPS MCP servers, OpenAPI tool sources and GraphQL connectors. The tool list is discovered at runtime and typed only as `{name, description, inputSchema, readOnly?, batch?}`, so GitHub, Linear and Slack are not distinct types in the core. Approval gating is by tool name and the declared `readOnly` flag.
- **`CloudAgentProvider`** (`launch`, `get`, `reply`, `cancel`) is the closest to a forge-adjacent seam: a remote coding agent that clones a repository and opens a PR. Its only real adapter is `cursor-cloud-agent.ts`. The wire types read `git.branches[].prUrl`; PR creation is a Cursor feature, not a Rakazo forge operation. Selected by `CLOUD_AGENT_PROVIDER` (`none | emulator | cursor`).
- Consequence for map #178: Rakazo offers no precedent for a typed forge or tracker interface. Its answer to "GitHub or Jira or Linear" is to not model them.

## 5. Chat and other vendors

- **`MessagingSurface`** (`interfaces.ts:517`): `platforms()`, `handleWebhook(provider, Request)`, `onInbound(sink)`, `sendToThread`, `openDirectThread`, `acknowledge`, optional `fetchThreadHistory`, `initialize`, `shutdown`. Thread ids are opaque and provider-prefixed (`"slack:C1:..."`). Platforms mount behind the Chat SDK (`chat-sdk-surface.ts` `MessagingPlatform {provider, capabilities, adapter}`); the SDK adapter verifies webhooks and translates payloads. Platforms seen in the tree: Slack, Telegram, Sendblue (iMessage/SMS), Lark, WhatsApp.
- Other small seams, each `describe()` plus a few verbs: `VoiceProvider` (ElevenLabs, OpenAI-compatible), `WebSearchProvider`/`WebFetchProvider`/`WebProvider` (Keenable and others), `BrowserProvider` (computer, fake, emulator), `TransactionalEmailProvider` (Resend, SMTP), `BillingProvider`, `KeyProvider` (AWS KMS), `SecretStore`, `ObjectStorage`, `MemoryStore`/`SemanticMemoryProvider` (Honcho), `JobPublisher`/`JobWorkerHost`, `RealtimeFanout`, `NotificationProvider`, `ExecutionRunner`.

## 6. Emulators and fakes

Documented in `docs/agent-verification.md`, `CONTRIBUTING.md`, `docs/self-host-integrations.md`.

| Boundary | Stand-in | Shape |
|---|---|---|
| Sandbox | `FakeSandboxProvider` (`fake-sandbox.ts`, 323 lines) | Implements `SandboxProvider` in memory: map of boxes, files, screens, leases; emulated shell over a loopback websocket gateway (`fake-terminal.ts`). Selected with `SANDBOX_PROVIDER=fake`. Other providers are tested with `fakeBoatProvider()`, `fakeBoxdProvider()`, `emulatedNovita()`, `FakeWuyingDesktop`: the **real adapter** against a fake vendor SDK or API |
| Sandbox conformance | `sandbox-conformance.test.ts`, `sandbox-test-support.ts` (`assertSandboxWorkspaceContract`, `provisionPrepared`) | One test body run across fake, Boat, boxd, Novita, desktop and Wuying: same command output, same observation/action/workspace contract |
| Model | `packages/testkit/src/model-emulator.ts` | A loopback **OpenAI-compatible HTTP/SSE server**, scripted as ordered steps. Each step has an `expect(request)` assertion run before the response, and the response kinds are text, tool calls, fragmented tool arguments, HTTP error, disconnect and hold. Tests must call `assertComplete`. The **real Pi agent loop runs against it**, through the generic OpenAI-compatible connection, so the stand-in is at the vendor wire, not at the Rakazo interface. A `scripted` `AgentRuntime` also exists for fast tests (`AGENT_RUNTIME=scripted`, tests only) |
| Model replay | `computer-replay.ts`, `computer-recording.ts` | A live run is recorded as a sanitised closed-vocabulary fixture (action names, known button labels, outcomes; no model prose, URLs, IDs or screenshots) and replayed through the model emulator. A fixture is written only if the live run and an immediate offline replay both succeed |
| Cloud agent | `cloud-agent-emulator.ts` | In-process **Cursor v1 HTTP server**: agent and run lifecycle, documented error statuses, lost responses, stale run metadata, revoked key, rate limits. The real `CursorCloudAgentProvider` runs against it. `CLOUD_AGENT_PROVIDER=emulator` |
| Connectors | `third-party-connector-emulator.ts` | A `fetch` replacement routed by hostname: `api.pipedream.com`, `remote.mcp.pipedream.net`, MCP, OpenAPI, GraphQL, a catalog. Records every call; an unexpected URL throws. Compares an expected bearer at the boundary and never writes it into records |
| Chat | `messaging-team-chat-emulator.ts`, `sendblue-emulator.ts` | Implement `MessagingSurface`-level behaviour in process: inject inbound events, capture sent posts |
| Wallet | `payment-wallet-emulator.ts` | In-process wallet; a shared conformance suite runs on the emulator and on the Link adapter backed by a fake Link API |
| Email | `EMAIL_EMULATOR=true` | Captures mail; forcibly off in production |
| Generic | `DestinationEmulator`, `McpEmulator` (`packages/testkit`) | Record writes in a place the test inspector reads, independent of what the model claims |

Rules the docs state: PR and default tests never need paid inference or live credentials; missing live credentials mean "not run", not "passed"; nightly runs only web tests with emulated providers; emulator settings are for tests only and some are refused when `NODE_ENV=production`; grading reads recorded effects and persisted state, not the model's claim; fixtures are synthetic.

Pattern: two kinds of stand-in. (a) A fake **implementation of the Rakazo interface** (`FakeSandboxProvider`, scripted runtime). (b) A fake **vendor API** that the real adapter talks to (Cursor, Pipedream, Boat, Novita, the OpenAI-compatible endpoint). Where an adapter has non-trivial vendor translation, they use (b), so the adapter itself is under test.

## 7. Compared with foreman and the seams on map #178

Foreman side, from the repo: `foreman/internal/ghtest` starts the vercel-labs `emulate` GitHub emulator seeded by `foreman/emulate.config.yaml` (admin token, per-role minted tokens, tests create their own repository), and tests assert on a recording proxy. That is Rakazo's kind (b): a fake vendor API with the real client under test.

| Question | Rakazo | Implication for foreman |
|---|---|---|
| Interface per vendor boundary | Yes, one per boundary, all with `describe()` and `AdapterContext` | The pattern fits, but the string-slot registry loses compile-time linkage (`get<T>` trusts the caller). A Go consumer-side interface per seam avoids this |
| Sandbox seam | Wide, product-shaped (desktop, screens, leases); durability handled by exportable workspace; 11 providers, conformance-tested | Foreman's sandbox is `srt` over a spec; a wide interface is not required. The portable-workspace idea is Rakazo-specific |
| Model and provider | Delegated to a library (Pi); Rakazo has a struct and a catalog lister, not a call interface | Supports the map's lean that **Model is a struct**. Whether a Model provider seam is needed depends on whether foreman uses a library like Pi or speaks to providers itself |
| Agent harness | Not a seam: Pi is the one in-process harness; `AgentRuntime` is `run`/`abort` | No precedent for a second harness. Compare Herdr's research (#180): closed enum plus manifests |
| Forge, issue tracker | None. Connectors are untyped tool lists; tickets are internal | No precedent; foreman's #182 and #183 designs have to stand without it |
| Scheduler | Not a seam: Graphile Worker (`JobPublisher`, `JobWorkerHost`) | `JobPublisher`/`JobWorkerHost` is a small queue seam, not a workflow scheduler |
| Chat | Seam with opaque thread ids and per-platform adapter behind a library | Foreman has no chat boundary today |
| Versioning | `contractVersion` string present, unenforced | Supports the map's preference to version only where it buys something |
| Fake per seam | Both kinds, plus a conformance suite across implementations | Matches the map's "emulator or fake per seam" fog. A conformance suite is a way to prove "a credible second implementation fits" without building one |
| Enforcement | `packages/*` split plus a `describe()` convention; no import-boundary tool (dependency-cruiser, madge, knip) is configured | Rakazo does not answer the go-arch-lint question |

## Not found

- No "plan principles" or "any model, any sandbox" chapter text; the video was not read.
- No forge-shaped interface, no VCS interface, no harness interface besides `AgentRuntime`.
- No code reading `contractVersion`.
- No architecture-lint config was looked for beyond the package split; `biome.json` is lint and format only.
