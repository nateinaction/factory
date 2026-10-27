# Agent roles in Matt Pocock's skills

Research for [Agent roles in Matt Pocock's skills (#5)](https://github.com/nateinaction/factory/issues/5), part of the map [Wayfinder: agent roles for the factory (#4)](https://github.com/nateinaction/factory/issues/4).

## Question

What lifecycle phases and handoffs does https://github.com/mattpocock/skills imply (triage, spec, tickets, implement, code-review, retro, wayfinder, ...)? For each: what it takes in, what it produces, which labels and artifacts carry work between phases, and where a human is expected in the loop. Which of these map onto distinct agent roles for an unattended factory, and which assume an interactive human?

## Sources

Every claim below cites a SKILL.md (or a reference file beside it) in `mattpocock/skills` at commit [`49dd158`](https://github.com/mattpocock/skills/tree/49dd158d1076134a641b33efb035946536778336), which is the ref the factory pins (`mattpocock_skills_ref` in `group_vars/factory.yml`). Upstream `main` was 3 commits ahead on 2026-10-27 (`59db609`). None of those commits touches a skill discussed here, apart from small edits to `setup-matt-pocock-skills` and its tracker templates (repo reachability and an "own repo only" rule for filing issues).

Short links used below (all at `49dd158`):

- [ask-matt]: the router, which is the repo's own map of flows.
- [PHASE-BOUNDARIES]: ask-matt's reference on what to do between phases.
- [triage], [AGENT-BRIEF], [OUT-OF-SCOPE]
- [to-spec], [to-tickets], [implement], [implement-spec], [tdd], [code-review], [retro], [pr]
- [wayfinder], [grilling], [grill-with-docs], [research], [prototype]
- [setup], [triage-labels], [tracker-github]
- [README]: the repo README, which documents the user-invoked vs model-invoked split.

## Answer in brief

- **Upstream's lifecycle is a single main flow (idea → grill → spec → tickets → implement → review → retro) plus on-ramps that join it: triage, diagnosing-bugs and wayfinder.** Every phase up to and including tickets is a conversation with a human. Implementation, review and retro are the only phases written so an agent can run them with nobody at the keyboard.
- **The handoff from human to agent is one label: `ready-for-agent`.** `/triage` applies it after a human decides. `/to-spec` and `/to-tickets` apply it when they publish. Upstream defines no labels after that point: nothing for in-progress, in-review, done or failed. The factory's `agent-working`, `ready-for-agent-review`, `agent-reviewing`, `agent-done`, `agent-failed` and `retro` labels are its own extension.
- **Upstream already names two agent roles: the implementation agent and the review agent.** [retro]'s "Implementation vs Review" section assigns coding standards to the reviewer because the reviewer has the least **context pressure**. That is the factory's Implementer/Reviewer baseline, endorsed upstream.
- **Retro is a step inside the Implementer, not a separate role.** It needs the implementation session as its primary source. Its output goes to a human: it "presents candidates" and changes nothing.
- **Research is the only other skill designed to run AFK** (a "background agent"; wayfinder's `research` tickets are labelled AFK). It is the strongest candidate for a third agent role. `/implement-spec` is AFK-shaped too, but it is an orchestrator that overlaps with what dagu and `finances-agent poll` already do.
- **Everything else assumes a human:** grilling, grill-with-docs, wayfinder charting and its grilling/prototype tickets, to-spec, to-tickets, triage decisions, improve-codebase-architecture, setup, to-questionnaire and wizard. Some of these explicitly forbid the agent from answering for the human.

## The flow upstream describes

From [ask-matt] ("The main flow: idea → ship"):

1. `/grill-with-docs` sharpens the idea by interview. It updates `GLOSSARY.md` and ADRs.
2. Optional detour: `/handoff` → `/prototype` → `/handoff` back, for questions that need runnable code to answer.
3. A multi-session build goes `/to-spec` → `/to-tickets`. Then either `/implement` per ticket ("`/clear`ing context between each one"), or `/implement-spec` for the whole graph in one run. Both drive `/tdd` and close with `/code-review`. `/pr` shapes the PR body.
4. `/retro` "closes the loop" by suggesting changes to the agent's **environment**, not the code.

On-ramps ([ask-matt] "On-ramps"):

- `/triage` takes raw issues. It is "only for issues **you didn't create**". Tickets from `/to-tickets` "are already agent-ready, so **don't triage them**".
- `/diagnosing-bugs` handles hard bugs, then `/retro` in the same session.
- `/wayfinder` handles foggy efforts. It "hands off, it doesn't build": it merges onto the main flow at `/to-spec`.

Context hygiene ([ask-matt] "Context hygiene"): keep grill → spec → tickets in one unbroken context window, start each `/implement` fresh from its ticket, and run `/retro` "in the session it's looking back on, before you clear".

[PHASE-BOUNDARIES] question 4 names the AFK test: "Is it scoped tightly enough to run with you away from the keyboard, no steering? Then send it to a **subagent** ... Automated review is the standard case."

## Phase by phase

"Invoked by" comes from each skill's frontmatter. `disable-model-invocation: true` means only a person typing the command can start it. The README says user-invoked skills "orchestrate", model-invoked skills "hold the reusable discipline", and "a user-invoked skill may invoke model-invoked skills, but never another user-invoked one" ([README] "Reference").

| Phase / skill | Invoked by | Takes in | Produces | Carriers (labels, artifacts) | Human in the loop |
| --- | --- | --- | --- | --- | --- |
| [setup] (precondition) | user | the repo | `docs/agents/issue-tracker.md`, `triage-labels.md`, `domain.md`; an `## Agent skills` block in CLAUDE.md/AGENTS.md; creates the labels | label mapping table [triage-labels] | Yes: "Explore, present what you found, confirm with the user, then write" |
| [triage] | user | raw issues and external PRs | category label + state label; an agent brief comment, triage notes, or a close | `bug`/`enhancement`; `needs-triage` → `needs-info` / `ready-for-agent` / `ready-for-human` / `wontfix`; `.out-of-scope/<concept>.md` [OUT-OF-SCOPE]; the "Agent Brief" comment [AGENT-BRIEF] | Yes. "Recommend ... Wait for direction". The maintainer drives by natural-language request. Grilling when needed |
| [grill-with-docs] / [grilling] | user / model | an idea | shared understanding; `GLOSSARY.md`, ADRs | `GLOSSARY.md`, `docs/adr/` | Yes, by definition: "The _decisions_ are the user's: put each to them and wait" |
| [prototype] | model | one design question | throwaway code | `prototype/<name>` branch kept as a primary source ([ask-matt] "Standalone") | Yes. HITL in wayfinder: "a cheap, rough, concrete artifact to react to" |
| [research] | model | a question | one cited Markdown file in the repo | the file (in wayfinder, on a `research/<name>` branch, never merged) | No: "Spin up a **background agent**" |
| [wayfinder] | user | a foggy idea | a map issue + decision tickets; decisions, "not deliverables" | `wayfinder:map`; `wayfinder:research` / `prototype` / `grilling` / `task`; sub-issues; native blocked-by edges; **assignee = claim**; a resolution comment; the map's Decisions-so-far | Mixed. Charting is grilling. Grilling and prototype tickets are HITL. Research is AFK. Task is either. Tickets "never carry a triage label like `ready-for-agent`" |
| [to-spec] | user | the current conversation | a spec issue | spec template (Problem, Solution, User Stories, Implementation Decisions, Testing Decisions, Out of Scope); labelled `ready-for-agent` | Light. "Do NOT interview the user", but "Check with the user that these seams match their expectations" |
| [to-tickets] | user | a spec, plan or conversation | tracer-bullet tickets with blocking edges | one issue per ticket, sub-issues of the source, native blocked-by edges, `ready-for-agent` ("agent-grabbable by construction"). Local tracker: `.scratch/<feature>/issues/NN-*.md` | Yes: "Quiz the user ... Iterate until the user approves the breakdown" |
| [implement] | user | a spec or ticket reference | commits on the current branch | the ticket; the branch | Mostly no. "If the reference is ambiguous, ask." It runs `/tdd`, which confirms seams with the user (next row) |
| [tdd] | model | a behaviour to build | red-green slices | tests at agreed seams | Gate: "write down the seams under test and confirm them with the user. No test is written at an unconfirmed seam." |
| [implement-spec] | user | a spec + its tickets | the whole spec on one **integration branch**; tickets resolved | task graph, **frontier**; implementer subagents in worktrees; a merger subagent; a draft PR; **context pointers** | No. It orchestrates subagents and ends with one `/code-review` |
| [code-review] | model | a fixed point + the diff + the spec | two reports, `## Standards` and `## Spec`, deliberately not merged or reranked | issue refs in commit messages; `CODING_STANDARDS.md` / `CONTRIBUTING.md`; spec files | Light. Asks for the fixed point and the spec if they aren't given. Runs two parallel sub-agents. No severities, no verdict |
| [pr] | model | a change | a PR body: Summary visual, Before/After evidence, Merge Danger (one-way or two-way door, blast radius) | the PR | No |
| [retro] | user | the session that just ran (or its log) | candidates for environment changes, most severe first | navigation pointers, automated checks, `CODING_STANDARDS.md`, AGENTS.md, tooling | Yes at the output: "Present these candidates to the user". It changes nothing itself |
| diagnosing-bugs | model | a hard bug | a fix plus a regression test | a feedback loop command | Not inherently |
| improve-codebase-architecture | user | the codebase (hot spots from git log) | an HTML report of **deepening opportunities**, then a grilling | feeds `/grill-with-docs` | Yes: "grill through whichever one you pick" |
| handoff, to-questionnaire, wizard, wait-what, teach | mixed | | | `$TMPDIR` handoff file; questionnaire; `.env` and secrets | Yes. wizard exists "for the steps only a **human** can take" |

## Labels and artifacts that carry work

**Triage labels** ([triage] "Roles", [triage-labels]). There are two category labels (`bug`, `enhancement`) and five state labels (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). Each repo can remap the strings in `docs/agents/triage-labels.md`.

- `ready-for-agent` means "fully specified, ready for an AFK agent". Its contract is the agent brief comment, not the issue body: "The original body and discussion are context: the agent brief is the contract" ([AGENT-BRIEF]).
- `ready-for-human` is upstream's explicit "not for the factory" state. Its brief notes "why it can't be delegated (judgment calls, external access, design decisions, manual testing)" ([triage] step 5).
- For a PR (when the tracker treats PRs as a request surface), `ready-for-agent` means "an agent should take the next step on the diff" and `ready-for-human` means "ready for a human to merge". This bears on [Move review labels from the issue to the pull request? (#3)](https://github.com/nateinaction/factory/issues/3).

**Wayfinder labels** ([wayfinder] "Tickets"). These are `wayfinder:map` plus `wayfinder:research` / `prototype` / `grilling` / `task`, and they are the only labels a map carries. A claim is the **assignee**, not a label: "an open, unassigned ticket is unclaimed". Blocking uses native tracker dependencies. The **frontier** is the set of open, unblocked, unassigned children ([tracker-github] "Wayfinding operations").

**Blocking edges.** [to-tickets] publishes native blocked-by links, and "any ticket whose blockers are done can be grabbed" ([ask-matt]). [tracker-github] documents how to count open blockers (`issue_dependencies_summary.blocked_by`).

**Repository artifacts:** `GLOSSARY.md` and `docs/adr/` (grilling, triage, spec); `.out-of-scope/` (triage); `CODING_STANDARDS.md`, which "is read during review, not implementation" ([retro] "Files"); `docs/agents/*.md` (setup); `prototype/<name>` and `research/<name>` branches; the integration branch ([implement-spec]).

**What upstream does not define:**

- labels for work in flight or done (in progress, in review, reviewed, failed);
- severities or a pass/fail verdict on reviews;
- a retro sink. Retro talks to the user who ran it.

The factory invented all three in `roles/agent/files/finances-agent`.

## Where a human is expected

Explicit gates, in flow order:

1. **Setup:** confirms the tracker, labels and doc layout ([setup] step 2–3).
2. **Triage:** the maintainer picks the outcome. The agent recommends, verifies and drafts. Every AI comment starts with `> *This was generated by AI during triage.*` ([triage]).
3. **Grilling:** every decision. "Do not act on it until the user confirms you have reached a shared understanding" ([grilling]).
4. **Spec:** the test seams are confirmed ([to-spec] step 2).
5. **Tickets:** the breakdown is approved ([to-tickets] step 4).
6. **TDD:** the seams are confirmed ([tdd]). In the main flow, step 4 has already pre-agreed them, which is why [implement] says "at pre-agreed seams".
7. **Retro:** the user receives the candidates and decides what to apply ([retro] step 4).
8. **Merge:** for a PR, `ready-for-human` means "ready for a human to merge" ([triage]). Nothing upstream merges automatically.

[wayfinder] states the HITL rule most sharply: "the agent never stands in for the human's side of it (a grilling agent that answers its own questions has broken this)".

## Mapping to agent roles for an unattended factory

### Distinct AFK agent roles the skills support

| Agent role | Entry skill (user-invoked) | Discipline it pulls in | Input carrier | Output carrier | HITL leaks to neutralise |
| --- | --- | --- | --- | --- | --- |
| **Implementer** | `implement` | `tdd`, `code-review` (self-check), `pr` | `ready-for-agent` issue + agent brief / spec ticket; review findings on rework | commits on a branch, a PR | `implement` "ask" when a reference is ambiguous; `tdd` confirming seams with the user. Both are resolved when the brief or spec already fixes the seams ([to-spec] step 2) and the prompt forbids questions, as `finances-agent` does |
| **Reviewer** | none upstream; `code-review` is model-invoked | `code-review` | a diff against a fixed point + the spec | the two-axis report | It asks for the fixed point and the spec if missing, so pass both. Upstream has no verdict, so the factory supplies severities and approve/request-changes. Its parallel sub-agents need a harness that can spawn them; this research did not verify that for `opencode` |
| **Researcher** (candidate) | none; `research` is model-invoked | `research` | `wayfinder:research` ticket (AFK by type) | a cited Markdown file on a `research/<name>` branch; a resolution comment; the ticket closed | None in the skill itself. Under [wayfinder] the session claims by assignee and must not edit the map, which the coordinator owns |

Upstream's own vocabulary backs the first two: [retro] "Implementation vs Review" says "all work goes through two stages: implementation and review", and says the review agent, not the implementation agent, "should be responsible for imposing coding standards".

### Steps, not roles

- **Retro** belongs to the Implementer's session. [ask-matt] says to run it "in the session it's looking back on". `finances-agent` already does this with `opencode run --continue`. Its output is advice for a person, so the factory has to give it a sink (today, the `Agent retros` ticket) and a human to apply it.
- **implement-spec** is an orchestrator: task graph, frontier, implementer and merger subagents, integration branch. It is AFK, but it is an alternative to per-ticket dispatch, not a peer of the Implementer. The factory's deterministic `poll` → `dagu enqueue` already plays the dispatcher. A factory could adopt it as a "spec mode" for the Implementer, but it would compete with dagu for scheduling.

### Interactive-only (keep with a human)

These should stay out of the factory, or at most run as an agent that drafts and then stops at a label a human must flip:

- grilling, grill-with-docs, grill-me
- wayfinder charting, and its grilling and prototype tickets
- to-spec, which synthesises a conversation that only exists in a human's session
- to-tickets, which waits for the human's approval
- improve-codebase-architecture
- setup-matt-pocock-skills
- to-questionnaire, wizard, handoff, wait-what, teach

Triage could become a partly AFK **Triager**. Steps 1 and 3 (gather context, redundancy and prior-rejection checks, reproduce the bug) and posting a recommendation are agent work. The transition to `ready-for-agent` / `ready-for-human` / `wontfix` is the maintainer's call, so such an agent role would end at `needs-triage` plus a recommendation comment.

## Implications for the factory (observations, not decisions)

- **The factory's intake boundary is `ready-for-agent`.** Upstream puts every human gate before it and none after it, apart from merge and applying retro advice.
- **Issues from to-tickets are grabbable only when their blockers are closed** ([to-tickets], [ask-matt]). `finances-agent poll` picks the oldest `ready-for-agent` issue and does not check native blocked-by edges. A generalised Implementer would start blocked tickets early.
- **Wayfinder tickets never carry `ready-for-agent`** ([wayfinder]), so a label-driven Implementer won't touch them. A Researcher would key on `wayfinder:research` plus "unassigned and unblocked" (the frontier query in [tracker-github]), not on a triage label.
- **Two claim conventions coexist.** Upstream claims by assignee (wayfinder). `finances-agent` claims by label swap, which also fails closed. Upstream doesn't specify which a repo-agnostic factory should use for triage-labelled work.
- **Each upstream role has one user-invoked entry skill** that pulls in model-invoked discipline. A user-invoked skill can't call another one ([README]), so `implement` can't call `retro` itself. That is why the factory runs retro as its own prompt.
- **Per-repo setup is a precondition.** `to-spec`, `to-tickets`, `implement-spec`, `code-review` and `wayfinder` expect the tracker doc and label mapping that `/setup-matt-pocock-skills` writes into `docs/agents/`. An assigned repository needs that (or an equivalent the factory writes) before these skills behave as designed.

[ask-matt]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/ask-matt/SKILL.md
[PHASE-BOUNDARIES]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/ask-matt/PHASE-BOUNDARIES.md
[triage]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/triage/SKILL.md
[AGENT-BRIEF]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/triage/AGENT-BRIEF.md
[OUT-OF-SCOPE]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/triage/OUT-OF-SCOPE.md
[to-spec]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/to-spec/SKILL.md
[to-tickets]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/to-tickets/SKILL.md
[implement]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/implement/SKILL.md
[implement-spec]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/implement-spec/SKILL.md
[tdd]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/tdd/SKILL.md
[code-review]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/code-review/SKILL.md
[retro]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/retro/SKILL.md
[pr]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/pr/SKILL.md
[wayfinder]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/wayfinder/SKILL.md
[grilling]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/productivity/grilling/SKILL.md
[grill-with-docs]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/grill-with-docs/SKILL.md
[research]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/research/SKILL.md
[prototype]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/prototype/SKILL.md
[setup]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/setup-matt-pocock-skills/SKILL.md
[triage-labels]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/setup-matt-pocock-skills/triage-labels.md
[tracker-github]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/setup-matt-pocock-skills/issue-tracker-github.md
[README]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/README.md
