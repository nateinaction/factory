# Agent roles in Poteto mode

Research for #7, a ticket on the map #4 ("Wayfinder: agent roles for the factory").

**Question.** What does pstack's `poteto-mode` skill, and the rest of the pstack
plugin, ask of an agent? How does it divide work into phases or roles, what does
each produce, and which parts apply to a factory of issue-driven agent roles?

**Sources.** All links point at `cursor/plugins` at commit
[`c742dd6`](https://github.com/cursor/plugins/tree/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack),
read on 2026-10-27. `S/` below abbreviates
`https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/`.

## Short answer

- Poteto mode is a **router plus a style**, not a fixed pipeline of roles. One
  chat receives `/poteto-mode`, matches the task to one of about 23
  **playbooks**, copies that playbook's steps into a todo list, and calls other
  skills as the steps need them. ([S/skills/poteto-mode/SKILL.md], Playbooks;
  [S/docs/guide/02-poteto-mode.md])
- Its role split is **coordinator versus delegate**. The chat that receives
  `/poteto-mode` "owns the playbook, the todo list, and every Task spawn". It
  plans, reviews and verifies. Delegates (the `poteto-agent` subagent) write the
  code. ([S/agents/poteto-agent.md]; Feature playbook "You own the design. Plan,
  review, verify. Delegate implementation.")
- **Verification is always done by an agent that did not write the code**, on
  a different model family where it can be. This is the rule pstack enforces
  most firmly. ([S/skills/poteto-mode/playbooks/shipping.md] step 1;
  [S/skills/poteto-mode/playbooks/orchestrate.md] Topology)
- The closest analogue to the factory is the dormant **benny** automation
  pack. It is a two-stage, event-triggered line (triage, then reproduce and
  fix). Stages hand off through a **machine-readable marker** posted by a
  trusted identity. Each stage can stop the line. Agents open **draft PRs
  only** and never merge.
  ([S/automations/benny/FOR_AGENTS.md]; [S/docs/guide/07-overnight.md] "Let
  loops start themselves")
- For the factory, this supports Implementer and Reviewer as the baseline, with
  the Reviewer on a different identity and preferably a different model family.
  It also supports an optional **Triager** stage in front, handing off by label.
  It does not support a standing Designer agent role. In pstack, design is a
  step inside the Implementer's run (`how` then `architect`), not a separate
  agent.

## What poteto-mode asks of an agent

From [S/skills/poteto-mode/SKILL.md]:

- **Restate the goal first.** The first message restates the user's goal, the
  problem, and the smallest change expected to meet it, without waiting for
  confirmation. Every subagent brief, PR `## Why`, and final report opens with
  that goal. (Non-negotiables, first bullet)
- **Do not ask what an experiment can answer.** A "which approach" fork whose
  answer can be observed gets a prototype, not a question. Only genuine product
  or preference calls go to the human. (Non-negotiables)
- **Trigger-to-skill mapping.** Nontrivial change goes to `how`. Code crossing
  a function boundary goes to `architect`. Fan-out goes to `swarm` or `arena`.
  A contested design goes to `interrogate`. Prose goes through `unslop` and
  `technical-writing`. Before commit, `deslop`. Before review, `no-comments`.
  Long or unattended work keeps a decision trail (`show-me-your-work`).
  (Non-negotiables)
- **Principles as named leaf skills.** About 23 `principle-*` skills (Laziness
  Protocol, Prove It Works, Make Operations Idempotent, Never Block on the
  Human, Encode Lessons in Structure, and others). The reply must name each
  principle that shaped a decision, citing only ones whose leaf was read.
  (Principles)
- **Autonomy policy.** Reversible work and external actions such as ticket
  updates proceed without asking. Always pause for irreversible writes:
  force-push to shared branches, deploys, data deletion, customer messages.
  (Autonomy)
- **Fresh subagents by default.** Each fix round, retry, or next queue item goes
  to a fresh subagent carrying the consolidated scope (original brief, later
  directives, the prior report and branch). Reuse an agent only when it holds
  costly local state. "A role such as a PR owner outlives its agent."
  (Subagents)
- **Model per role.** Each role names its model. Defaults are a fast code model
  for code and a strong judgment model for prose and judgment. `/setup-pstack`
  writes a rule with one line per role (`feature, refactoring`, `bug-fix`,
  `judgment and prose`, `hardest tasks`, `swarm workers`, `interrogate
  reviewers`, and others). ([S/skills/setup-pstack/SKILL.md] step 5)
- **Own what delegates produce.** "Review the diff and write your own summary,
  don't pass through what it said. A second opinion is the same prompt against
  a different model. Agreement is high-signal." (Subagents)

## How work divides: playbooks, not personas

Poteto mode has two agent definitions. Every other division is a playbook step
or a routed skill.

| Agent | What it is | Source |
| --- | --- | --- |
| `poteto-agent` | The delegate for code-writing and helper tasks. Reads `poteto-mode/SKILL.md` in full before work. Never runs the playbook itself. | [S/agents/poteto-agent.md] |
| `comment-sicko` | A narrow reviewer that only reports comments to delete and symbols to rename (`MUST KILL`). Never writes application code. | [S/agents/comment-sicko.md] |

The playbooks that map most directly onto issue-driven work:

| Playbook | Who owns what | What it produces |
| --- | --- | --- |
| Feature | Lead owns design, plan, review and verify. A delegate writes code. | `how` explanation, `architect` sketch, a throughput checkpoint (blocking steps, independent workstreams, shared state, smallest decomposition), small ordered commits, a PR. |
| Bug fix | Lead owns the task. Delegates investigate and fix. | A reproduction on the real surface, a root cause, the failing repro committed before the fix, a PR, and failing-then-passing output in the reply. |
| Opening a PR | Run at the end of every playbook. | A ready (not draft) PR with `## Why`, `## What changed`, `## Scope`, `## Tradeoffs`, `## Blast Radius`, `## Verification`. |
| Babysit | Driving a PR to merge-ready. Only when asked, never on opening a PR. | Conflicts, review threads and CI resolved. |
| Shipping | Lands a green stack. | One verifier per PR, not its author, posts `PASS`, `PASS+NOTES` or `FAIL`. Only the contiguous verified run from the root lands. |
| Autopilot-full | Root owns verdicts, never PRs. One owner per PR builds through merge. | A swarm verdict at the code-ready head. The owner merges only what the root verified. |
| Autopilot-stack | As Autopilot-full, without merging. | Verified PRs handed to the operator to land. |
| Orchestrate | Coordinator runs the program, never the code. | Each kickoff names goal, done evidence, writable paths and forbidden actions. Each PR gets a verifier that is not its author, on a different model family. |

Sources: [S/skills/poteto-mode/playbooks/feature.md],
[S/skills/poteto-mode/playbooks/bug-fix.md],
[S/skills/poteto-mode/playbooks/opening-a-pr.md],
[S/skills/poteto-mode/playbooks/shipping.md],
[S/skills/poteto-mode/playbooks/autopilot-full.md],
[S/skills/poteto-mode/playbooks/orchestrate.md].

Routed skills that act like sub-roles inside a playbook:

- `architect`. Phases Ground, Sketch, Agree (an opt-in human checkpoint, off
  by default), Implement, Scrap. It runs at least two structurally distinct
  design candidates on different models and synthesizes them. Output: a type
  and signature sketch plus a rationale. ([S/skills/architect/SKILL.md])
- `swarm`. Phases Frame, Fan out, Aggregate, Report. Workers report `PASS`,
  `ISSUES` or `BLOCKED` with evidence, and each names the exact SHAs it
  checked. ([S/skills/swarm/SKILL.md])
- `interrogate`. Adversaries on several models try to break a change. A
  finding survives only if a rerun command proves it. It reports and does not
  fix. ([S/skills/interrogate/SKILL.md])
- `reflect`. Three reviewers (judgment, tooling, divergent) mine the transcript
  and a synthesizer returns Accepted, Rejected and Backlog edits to skills.
  Accepted edits wait for explicit human approval because "skill changes affect
  every future agent". Items a lint or script could enforce go to the Backlog.
  ([S/skills/reflect/SKILL.md])

## The issue-driven line: benny

[S/automations/benny/] is the part of pstack built for event-triggered work.
The guide frames it as stages: "triage a report, reproduce it, fix it, verify
the fix", with two rules: **every stage can stop the line**, and **every stage
hands over evidence**. ([S/docs/guide/07-overnight.md])

- **Triage** ([S/automations/benny/skills/triage-issue-reports/SKILL.md]).
  Triggered by a new report. Classifies it (bug, performance, feature request,
  question, reroute), traces the likely owning layer, dedupes against the
  tracker, creates a ticket only for a clear new bug. Output: exactly one reply
  ending in one marker: `[benny:bug]`, `[benny:performance]` or `[benny:other]`.
- **Reproduce and fix**
  ([S/automations/benny/skills/reproduce-and-fix-issues/SKILL.md]). Waits for
  a marker from the configured triage identity and proceeds only on bug or
  performance. Gates: stop if a person claims the fix. Switch to verify mode
  if an existing PR or commit may fix it, and "do not author over them".
  Reproduce twice through the real UI. Attempt a bounded fix only after a
  confirmed repro, with TDD when the test is cheap. Prove before and after.
  Open a **draft PR only**. "Never merge or deploy from this workflow."
- **Shared rules** ([S/automations/benny/FOR_AGENTS.md]). Fail closed when
  configuration, tracker access, or the control adapter is missing. The
  coordinator is the only agent that posts. Subagents are read-only and get no
  credentials. User-owned configuration lives outside the copied pack so that
  refreshing the pack cannot overwrite it.

## What applies to the factory

Mapping onto the factory's current `finances-agent` (README "Issue agent":
`run` with the `implement` and `retro` skills, `review-run` with
`code-review`):

1. **Implementer and Reviewer survive as the baseline.** pstack's strongest
   invariant is that the verdict comes from an agent that did not write the
   code. The factory already has this split. pstack adds two refinements. The
   reviewer should run on a **different model family** from the implementer
   (Orchestrate, Topology). Its verdict covers **one head SHA**, so a new push
   needs a new verdict (Orchestrate, Supervision). This supports giving the
   Reviewer its own identity (#2): pstack's "never give or bypass an approval
   the forge enforces" (Autopilot-full step 6) assumes the approver is a
   separate party.
2. **Design is a step, not an agent role.** In Feature and Bug fix, the same
   lead runs `how` and `architect` before delegating code. Architect's human
   checkpoint is opt-in. That is evidence against a standing Designer agent
   role. A Designer would have to earn its place as a **human-gated design
   checkpoint** on large issues, which pstack offers only as an opt-in.
3. **A Triager is the one extra stage pstack ships.** benny puts triage in
   front of implementation and hands off through a fixed marker from a trusted
   identity. The factory's equivalent is a label such as `ready-for-agent`
   applied by a Triager. This stage could also enforce what the README calls
   the real gate ("only people you trust should be able to apply it").
4. **Handoff by machine-readable marker from a trusted identity.** benny
   matches an exact marker and checks its author, and stops silently on
   conflicting or untrusted markers. The factory's labels already act as
   markers. The lesson is to **check who applied the label** (the issue's
   timeline event actor) and fail closed when the actor is not trusted.
5. **Every stage can stop the line, with a named outcome.** benny has
   `Could not reproduce`, `Blocked`, and an ownership gate ("do not race" a
   person who claimed the fix). The factory's `agent-failed` is a single
   outcome. Distinct stop states such as blocked, declined, or needs-human
   would keep a stage's useful "no" from reading as a failure.
6. **Every stage hands over evidence.** pstack requires proof on the real
   artifact. Green CI or "it compiles" does not count (Prove It Works; Bug fix
   step 4). The Implementer's PR should carry its `## Verification` section,
   and the Reviewer should rerun the checks rather than trust it.
7. **Fresh agent per round, durable role.** pstack starts a fresh subagent for
   each fix round with consolidated scope (brief, directives, prior review,
   branch). The factory's rework pass already "continues from that branch with
   the latest review in the prompt". That matches pstack, so keep it.
8. **Model choice is per agent role.** `/setup-pstack` writes one line per role
   (fast code model for implementation, strongest judgment model for hardest
   tasks, prose and review). That shape suits the map's "Model choice per
   agent role" item and could become per-agent-role variables in place of the
   single `factory_agent_model`.
9. **Retros feed skills behind a human gate.** `reflect` routes findings to
   concrete skill edits and does not auto-apply them. Lessons that a check can
   enforce become a lint or script instead (Encode Lessons in Structure). This
   answers the map's "How retros feed back into skills": the retro proposes
   skill edits as an issue or PR, and a person approves them.
10. **Human gates are for irreversible actions.** Poteto mode's "Always pause"
    list is force-push to shared branches, deploys, data deletion, customer
    messages. benny's agents never merge. Autopilot-stack hands verified PRs to
    the operator to land. For the factory, the defensible default is: agents
    open and review PRs, and a person merges.

### What does not transfer

- Poteto mode assumes one interactive coordinator chat spawning in-session
  subagents (Cursor `Task`, cloud VMs, `/loop`). The factory is
  scheduler-driven, with each dagu run a separate process. The coordinator's
  job of picking the next issue and tracking state sits in the poll DAG and
  issue labels, not in a long-lived chat.
- Swarm and arena fan-out (several verifiers or design runners per change)
  costs more agents per issue than one worker host at a time can supply (see
  #1, out of scope here).
- UI control skills (`control-ui`, `control-cli`) and the reproduce-twice
  requirement presume a runnable app surface. For generic repositories, the
  equivalent is "run the repository's own checks and a repro command".

[S/skills/poteto-mode/SKILL.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/skills/poteto-mode/SKILL.md
[S/docs/guide/02-poteto-mode.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/docs/guide/02-poteto-mode.md
[S/docs/guide/07-overnight.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/docs/guide/07-overnight.md
[S/agents/poteto-agent.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/agents/poteto-agent.md
[S/agents/comment-sicko.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/agents/comment-sicko.md
[S/skills/poteto-mode/playbooks/feature.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/skills/poteto-mode/playbooks/feature.md
[S/skills/poteto-mode/playbooks/bug-fix.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/skills/poteto-mode/playbooks/bug-fix.md
[S/skills/poteto-mode/playbooks/opening-a-pr.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/skills/poteto-mode/playbooks/opening-a-pr.md
[S/skills/poteto-mode/playbooks/shipping.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/skills/poteto-mode/playbooks/shipping.md
[S/skills/poteto-mode/playbooks/autopilot-full.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/skills/poteto-mode/playbooks/autopilot-full.md
[S/skills/poteto-mode/playbooks/orchestrate.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/skills/poteto-mode/playbooks/orchestrate.md
[S/skills/architect/SKILL.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/skills/architect/SKILL.md
[S/skills/swarm/SKILL.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/skills/swarm/SKILL.md
[S/skills/interrogate/SKILL.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/skills/interrogate/SKILL.md
[S/skills/reflect/SKILL.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/skills/reflect/SKILL.md
[S/skills/setup-pstack/SKILL.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/skills/setup-pstack/SKILL.md
[S/automations/benny/]: https://github.com/cursor/plugins/tree/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/automations/benny
[S/automations/benny/FOR_AGENTS.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/automations/benny/FOR_AGENTS.md
[S/automations/benny/skills/triage-issue-reports/SKILL.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/automations/benny/skills/triage-issue-reports/SKILL.md
[S/automations/benny/skills/reproduce-and-fix-issues/SKILL.md]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/automations/benny/skills/reproduce-and-fix-issues/SKILL.md
