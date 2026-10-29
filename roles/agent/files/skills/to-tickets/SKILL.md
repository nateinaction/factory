---
name: to-tickets
description: Break a plan, spec, or the current conversation into a set of tracer-bullet tickets, each declaring its blocking edges, proposed as slices in the Planner's output file.
disable-model-invocation: true
---

<!-- Forked from mattpocock/skills skills/engineering/to-tickets at 49dd158d1076134a641b33efb035946536778336. -->

# To Tickets

Break a plan, spec, or conversation into a set of **tickets**: tracer-bullet vertical slices, each declaring the tickets that **block** it.

The repository's `docs/agents/issue-tracker.md` and `docs/agents/triage-labels.md` are in your context. In the factory you never write to the tracker and never pick a label: the tickets are the Planner's `slices` proposal, each slice carrying its `blocked_by` slugs, and foreman creates the issues, links them as sub-issues and derives the labels.

## Process

### 1. Gather context

Work from whatever is already in the conversation context. If the run's prompt passes a reference (a spec path, an issue number or URL), fetch it with `gh` and read its full body and comments.

### 2. Explore the codebase (optional)

If you have not already explored the codebase, do so to understand the current state of the code. Ticket titles and descriptions should use the project's domain glossary vocabulary, and respect ADRs in the area you're touching.

Look for opportunities to prefactor the code to make the implementation easier. "Make the change easy, then make the easy change."

### 3. Draft vertical slices

Break the work into **tracer bullet** tickets.

<vertical-slice-rules>

- Each slice cuts a narrow but COMPLETE path through every layer (schema, API, UI, tests): vertical, NOT a horizontal slice of one layer
- A completed slice is demoable or verifiable on its own
- Each slice is sized to fit in a single fresh context window
- Any prefactoring should be done first

</vertical-slice-rules>

Give each ticket its **blocking edges**: the other tickets that must complete before it can start. A ticket with no blockers can start immediately.

**Wide refactors are the exception to vertical slicing.** A **wide refactor** is one mechanical change (rename a column, retype a shared symbol) whose **blast radius** fans across the whole codebase, so a single edit breaks thousands of call sites at once and no vertical slice can land green. Don't force it into a tracer bullet; sequence it as **expand–contract**. First expand: add the new form beside the old so nothing breaks. Then migrate the call sites over in batches sized by blast radius (per package, per directory), each batch its own ticket blocked by the expand, keeping CI green batch to batch because the old form still exists. Finally contract: delete the old form once no caller remains, in a ticket blocked by every migrate batch. When even the batches can't stay green alone, keep the sequence but let them share an integration branch that all block a final integrate-and-verify ticket; green is promised only there.

### 4. Check the breakdown against the writer's answers

There is no writer to quiz in this run. Check it yourself against the issue thread, the spec and the writer's answers already in "Issue material":

- Does the granularity feel right? (too coarse / too fine)
- Are the blocking edges correct: does each ticket only depend on tickets that genuinely gate it?
- Should any tickets be merged or split further?

Revise until the breakdown holds, then put any remaining judgement back to the writer as a question round instead of guessing.

### 5. Write the tickets into the proposal

Write the approved tickets as the Planner's `slices` proposal: each slice's title, what it delivers, its acceptance criteria and its `blocked_by` slugs (you supply the slugs; `#N` names an issue that already exists). foreman publishes them in dependency order, so each blocking edge can reference a real identifier.

The template is below. Use it per slice; never combine slices into one. Avoid specific file paths or code snippets: they go stale fast. Exception: if a prototype produced a snippet that encodes a decision more precisely than prose can (state machine, reducer, schema, type shape), inline it and note briefly that it came from a prototype. Trim to the decision-rich parts, not a working demo, just the important bits.

<issue-template>

## Parent

A reference to the parent issue on the tracker (if the source was an existing issue, otherwise omit this section).

## What to build

The end-to-end behaviour this ticket makes work, from the user's perspective, not layer-by-layer implementation.

## Acceptance criteria

- [ ] Criterion 1
- [ ] Criterion 2

## Blocked by

- A reference to each blocking ticket, or "None (can start immediately)". Omit this section when blockers were set as native edges.

</issue-template>

A slice's `blocked_by` uses the slugs the proposal assigns, so it stays true when foreman creates the issues.
