# Agent roles in Google's Elephant-Goldfish Model

Research for [#6](https://github.com/nateinaction/factory/issues/6), part of the
agent roles map [#4](https://github.com/nateinaction/factory/issues/4).

**Question.** What is <https://github.com/google/elephant-goldfish-model>? What
roles, memory split (long-lived vs fresh-context agents) or division of labour
does it prescribe, how do its parts hand work to each other, and what of it
applies to a factory of issue-driven agent roles?

**Sources.** The repository itself, read at commit
[`a8380ab`](https://github.com/google/elephant-goldfish-model/tree/a8380ab5f0aba392c97e31c79f9a6666b3f09f14)
(2026-10-27). Links below are to that commit; `EGM:` paths are relative to the
repo root. The model's origin is Dave Rensin's essay, which the repo cites but
which was not read here: every claim below is from the repo.

## What it is

A plugin of **skills** (slash commands) and **subagent personas** for
spec-driven development, packaged for Antigravity, Claude Code, Claude Cowork,
Codex and ChatGPT Work. Apache-2.0, "not an officially supported Google
product". It drafts a design spec with a deep-context agent and stress-tests it
with zero-context subagents *before* code is written; for "a small, clear bug
fix, use your normal coding workflow". ([README][readme])

Four parts ([architecture][arch]): skills (`skills/`), stateless reviewer
personas (`agents/`), versioned rubrics (`resources/document_standards/`), and
living docs checked in under `docs/designs/`.

It is a **design-time, human-in-the-loop** tool. It does not implement code:
phase 5 just says "coding agents implement code strictly adhering to the
approved design document", and "EGM is not opinionated" about implementation
skills. ([architecture][arch], [getting started][gs])

## The roles

The README names exactly three ([README, "How it works"][readme]):

| Role | Context | Responsibility |
| --- | --- | --- |
| **Elephant** | Your project, requirements and earlier decisions | Explore the problem and draft the specification |
| **Goldfish** | The document and its referenced source material, *without the earlier conversation* | Find missing assumptions, unclear requirements, implementation risks |
| **You** (human) | The goals and constraints that matter | Resolve trade-offs, approve the design, decide what to build |

A fourth actor, the implementation agent, appears only in the lifecycle diagram
and gets no skill or persona. ([architecture][arch])

### Elephant: the long-lived context

- The primary, persistent conversation session, "loaded with system history,
  CUJs, and codebase maps". It runs `egm-brainstorm`, `egm-draft` and
  `egm-resolve`. ([architecture][arch])
- Resolution belongs in the Elephant on purpose: `egm-resolve` "should be run in
  the persistent conversation session that generated the target document (the
  Elephant) to recall earlier reasoning and craft higher-quality fixes".
  ([egm-resolve][resolve])
- Long-term memory lives in the repo, not the session: design docs under
  `docs/designs/` with a `SITEMAP.md` index, per-directory READMEs from
  `egm-bootstrap`, and an optional `<project>-elephant` skill that just
  "tell[s] the LLM about your project, and give[s] it links to the context".
  ([getting started][gs])

### Goldfish: fresh-context reviewers

- Each Goldfish is a subagent "spawned in isolated, fresh context windows to
  catch unstated assumptions without historical bias". ([egm-review-loop][loop])
- It reads only the target document plus an explicit reading list
  (`GoldfishContext`). ([egm-review-loop][loop], [review design][review])
- Enforced read-only. The persona says so ("You have NO tools to modify the
  workspace") ([design_critic persona][critic]), and the Claude Code adapter
  restricts tools to `[Read, Grep, Glob]` and sets `omitClaudeMd: true`, so
  project instructions do not leak in ([Claude adapter][critic-adapter]).
- Isolation is a hard requirement: "If your harness has no subagent tool, stop
  and tell the user: a review in the same context is not a Goldfish review."
  ([egm-review-loop][loop])
- Seven lens personas, each with its own rubric ([review design][review]):

  | Persona | Lens |
  | --- | --- |
  | `zoom_out` | Skeptical architect: YAGNI, simplicity, reuse of existing code |
  | `comprehension` | Naive reader: is the doc self-contained? |
  | `design_critic` | SPOFs, scale, failure modes, unjustified assumptions |
  | `implementation_readiness` | Tech lead: complete file list, deterministic steps, test plan |
  | `codebase_details` | Checks cited paths and signatures against live code |
  | `structural` | Template and heading conformance |
  | `style` | Tone and concision |

- A **Synthesizer** sits between Elephant and Goldfish. It spawns reviewers in
  parallel (optionally two independent instances, A and B, of each "to maximize
  finding recall"), deduplicates, sorts by severity (Blocker / Major / Nit),
  writes `goldfish_review_report.md`, and returns only a count and a path, so
  the Elephant's context is not flooded. ([synthesizer persona][synth],
  [synthesizer design][synth-design]) Its Claude adapter gets
  `[Agent, Read, Grep, Glob, Write]` and no shell. ([adapter][synth-adapter])

### Human: owns the decisions

"The human engineer is responsible for architectural decisions, trade-offs, and
owns the outcome." ([architecture][arch]) The skills gate every stage
transition on explicit approval, one question at a time
([execution instructions][exec]), and the spec goes to a human peer as a PR
before implementation ([getting started][gs]).

## How work is handed off

Every handoff is a **file**, not a chat message ("Artifact-First UX: chat dump
prohibition"). ([architecture][arch])

```text
human ──/egm-brainstorm──▶ Elephant ──6-lens Goldfish swarm──▶ concepts_brief.md
human ──/egm-draft───────▶ Elephant ──section by section────▶ docs/designs/FEATURE.md
human ──/egm-review──────▶ Synthesizer ──Goldfish swarm─────▶ goldfish_review_report.md
Elephant (egm-resolve) triages findings ──human gate──▶ edits FEATURE.md ──▶ loop
human ──PR──▶ human peer review ──merge──▶ implementation agent builds to FEATURE.md
weekly: /egm-drift-report compares docs/designs/ with code; a human picks doc or code to fix
```

Sources: [architecture][arch], [getting started][gs], [drift report][drift].

**Staged review.** `/egm-review` runs five stages in order, with a checkpoint
between each: zoom out → comprehension → critic (`design_critic`, `structural`,
`style`) → zoom out again, to prune complexity the fixes added → implementation
readiness (`implementation_readiness`, `codebase_details`, `style`). The
draft is split into a high-level `FeatureRange` and a `DetailedRange`
(implementation plan), and each stage reviews only its range. ([review design][review])

**Triage contract.** The Elephant gives each finding exactly one verdict
([egm-resolve][resolve]):

- `PROPOSE FIX`: valid, here is the exact edit.
- `CONTEST`: wrong, *citing evidence the reviewer could read* (a file and
  symbol, or a decision recorded in the doc). "This was intentional" is not
  evidence.
- `ESCALATE`: wrong, but only knowledge outside the readable files refutes it.
  The human decides.
- `DECLINE`: a nit, recorded and not fixed.

Comprehension gaps cannot be contested; severities cannot be changed except by
contesting; "Do not add context only to satisfy a reviewer." The loop repeats
until zero findings or the human halts, and the driver must wait for the human
before each new round. ([egm-review-loop][loop])

## What applies to the factory

How the factory works today (from `README.md` and `roles/agent/files/finances-agent`):
the Implementer (`run`) is a new opencode session on every pass, given the issue
and, on rework, the latest review's text; the retro then *continues that same
session*. The Reviewer (`review-run`) is a separate session that reads the issue
as spec plus the diff and returns strict JSON with high / medium / low findings.
The Implementer must "fix every high and medium finding".

1. **The Reviewer is already a Goldfish; EGM shows how to harden it.** It runs
   in a fresh session, judging the change against a written spec. EGM adds:
   enforce read-only tools rather than just asking; give it an explicit reading
   list (issue, diff, cited files) instead of letting it wander; and decide on
   purpose whether repo instructions reach it (EGM strips `CLAUDE.md` from its
   reviewers; a code reviewer checking house standards probably wants them as
   an explicit input instead).
2. **Split review into lenses only if one generalist misses things.** EGM's
   lenses (YAGNI/reuse, spec compliance, codebase accuracy) and A/B duplicate
   reviewers buy recall at a cost in tokens and time. On Pi workers, one
   reviewer per pass is the cheaper baseline. Lenses are a later upgrade, not
   a new agent role.
3. **Give the Implementer a contest and escalate path.** Today it must fix
   every high and medium finding. EGM's rule set (contest only with evidence
   the reviewer could read, escalate what only a human knows, never re-grade
   severity) maps to a reply on the PR, and to a label that hands the issue to
   a human (a human gate) instead of burning the 3-review cap.
4. **The factory has no long-lived Elephant session, so the memory must live
   in the repo.** Each Implementer run starts cold. EGM's answer for durable
   context is checked-in artifacts (`docs/designs/`, a sitemap, a per-project
   context skill), not a kept-alive conversation. For a generalized factory
   this means: the issue (and any design doc it links) is the spec, and each
   assigned repository supplies its own context skill or docs.
5. **A Designer agent role is EGM's centre of gravity, but EGM's Designer is
   interactive.** Brainstorm and draft are conversations with a human that
   ask one question at a time. Unattended in the factory, a Designer could
   draft `docs/designs/<feature>.md` from an issue and run a Goldfish pass on
   it, but the approval EGM requires before implementation would have to be a
   human gate: a PR approval or a label. EGM itself says to skip design for
   small, clear changes, so a Designer only earns its place for issues that
   need design decisions. Most `ready-for-agent` issues may not.
6. **Handoffs as artifacts, with thin summaries, fits the factory.** Issue
   comments, PR reviews and checked-in docs are the factory's artifacts. EGM's
   Synthesizer rule (write the full report to a file, pass back only a count
   and a link) is a good default for any agent that hands off to another.
7. **Drift report is a candidate scheduled job, not yet an agent role.** EGM
   runs `egm-drift-report` weekly to compare docs with code. It would fit a
   dagu cron that opens an issue, but only for repositories that keep design
   docs.
8. **EGM says nothing about implementation, identity or permissions beyond
   tool lists.** It has no notion of a PR author, a second GitHub identity, or
   a queue. Those parts of the map (#2, scheduling, harness) need other
   sources.

[readme]: https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/README.md
[arch]: https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/docs/designs/architecture.md
[review]: https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/docs/designs/review.md
[gs]: https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/docs/getting_started.md
[drift]: https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/docs/designs/drift_report.md
[loop]: https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/skills/egm-review-loop/SKILL.md
[resolve]: https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/skills/egm-resolve/SKILL.md
[critic]: https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/agents/design_critic.md
[critic-adapter]: https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/.claude-plugin/agents/design_critic.md
[synth]: https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/agents/synthesizer.md
[synth-design]: https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/docs/designs/synthesizer.md
[synth-adapter]: https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/.claude-plugin/agents/synthesizer.md
[exec]: https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/resources/execution_instructions.md
