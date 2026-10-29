---
name: code-review
description: "Review the changes since a fixed point (commit, branch, tag, or merge-base) along two axes: Standards (does the code follow this repo's documented coding standards?) and Spec (does the code match what the originating issue/spec asked for?), then record one verdict."
---

<!-- Forked from mattpocock/skills skills/engineering/code-review at 49dd158d1076134a641b33efb035946536778336. -->

Two-axis review of the diff between `HEAD` and a fixed point the run's prompt supplies:

- **Standards**: does the code conform to this repo's documented coding standards?
- **Spec**: does the code faithfully implement the originating issue / spec?

## In the factory

- The run's prompt gives the fixed point and the path of the spec. There is no user to ask: if either is missing, say so in the output file instead of asking.
- The `task` tool is denied, so **run both axes yourself, one after the other in this context**: Standards first, then Spec. Do not dispatch sub-agents. Reset your attention between them and keep their findings separate; write the Spec report without letting the Standards report colour it.
- The verdict leaves only through the run's output file. Never post a review, comment or check.

## Process

### 1. Pin the fixed point

The prompt names it (a commit SHA, branch name, tag, `main`, `HEAD~5`, etc.).

Capture the diff command once: `git diff <fixed-point>...HEAD` (three-dot, so the comparison is against the merge-base). Also note the list of commits via `git log <fixed-point>..HEAD --oneline`.

Before going further, confirm the fixed point resolves (`git rev-parse <fixed-point>`) and the diff is non-empty. A bad ref or empty diff should fail here, not mid-review.

### 2. Read the spec

The prompt gives the spec's path. Read it in full. If there is no spec, say "no spec available" in the Spec section and review Standards only.

### 3. Identify the standards sources

Search the repo for every file that documents how code should be written. When `CODING_STANDARDS.md` or `CONTRIBUTING.md` exists, it must be on the list.

On top of whatever the repo documents, the Standards axis always carries the **smell baseline** below: a fixed set of Fowler code smells (_Refactoring_, ch.3) that applies even when a repo documents nothing. Two rules bind it:

- **The repo overrides.** A documented repo standard always wins; where it endorses something the baseline would flag, suppress the smell.
- **Always a judgement call.** Each smell is a labelled heuristic ("possible Feature Envy"), never a hard violation. Like any standard here, skip anything tooling already enforces.

Each smell reads *what it is* → *how to fix*; match it against the diff:

- **Mysterious Name**: a function, variable, or type whose name doesn't reveal what it does or holds. → rename it; if no honest name comes, the design's murky.
- **Duplicated Code**: the same logic shape appears in more than one hunk or file in the change. → extract the shared shape, call it from both.
- **Feature Envy**: a method that reaches into another object's data more than its own. → move the method onto the data it envies.
- **Data Clumps**: the same few fields or params keep travelling together (a type wanting to be born). → bundle them into one type, pass that.
- **Primitive Obsession**: a primitive or string standing in for a domain concept that deserves its own type. → give the concept its own small type.
- **Repeated Switches**: the same `switch`/`if`-cascade on the same type recurs across the change. → replace with polymorphism, or one map both sites share.
- **Shotgun Surgery**: one logical change forces scattered edits across many files in the diff. → gather what changes together into one module.
- **Divergent Change**: one file or module is edited for several unrelated reasons. → split so each module changes for one reason.
- **Speculative Generality**: abstraction, parameters, or hooks added for needs the spec doesn't have. → delete it; inline back until a real need shows.
- **Message Chains**: long `a.b().c().d()` navigation the caller shouldn't depend on. → hide the walk behind one method on the first object.
- **Middle Man**: a class or function that mostly just delegates onward. → cut it, call the real target direct.
- **Refused Bequest**: a subclass or implementer that ignores or overrides most of what it inherits. → drop the inheritance, use composition.

### 4. Review Standards

Working from the diff and the standards sources, report per file/hunk where relevant:

(a) every place the diff violates a documented standard: cite the standard (file + the rule);
(b) any baseline smell you spot: name it and quote the hunk.

Distinguish hard violations from judgement calls: documented-standard breaches can be hard, but baseline smells are always judgement calls, and a documented repo standard overrides the baseline. Skip anything tooling enforces.

### 5. Review Spec

Working from the diff and the spec, report:

(a) requirements the spec asked for that are missing or partial;
(b) behaviour in the diff that wasn't asked for (scope creep);
(c) requirements that look implemented but where the implementation looks wrong.

Quote the spec line for each finding.

### 6. Record the verdict

The output file is JSON, in the shape the run's prompt states. Its `summary` keeps the two axes under `## Standards` and `## Spec` headings, verbatim or lightly cleaned. Do **not** merge or rerank findings, because the two axes are deliberately separate (see _Why two axes_).

The verdict follows from the findings, not from how the summary reads:

- `REQUEST_CHANGES` when any finding is `high` or `medium`, and there must be at least one finding.
- `APPROVE` when every finding is `low`, or there are none.

End the summary with a one-line count per axis, and the worst issue _within each axis_ (if any). Don't pick a single winner across axes: that's the reranking the separation exists to prevent.

## Why two axes

A change can pass one axis and fail the other:

- Code that follows every standard but implements the wrong thing → **Standards pass, Spec fail.**
- Code that does exactly what the issue asked but breaks the project's conventions → **Spec pass, Standards fail.**

Reporting them separately stops one axis from masking the other.
