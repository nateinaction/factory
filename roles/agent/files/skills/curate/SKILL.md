---
name: curate
description: Curate a repository's retro dashboard. Use when a Curator run has the retro log and the dashboard and must propose merges, links and promoted issues.
---

<!-- Factory-only skill: written for the factory, with no upstream fork. -->

Curate the repository's **retro dashboard**: turn the append-only retro log into a shorter, more truthful set of groups a writer can act on. You never write to GitHub and never change the dashboard; you propose, and foreman applies.

## Inputs

The run's prompt names them:

- **The retro log**, whole, with each finding's target, group key, severity, claim, evidence, and the agent role and run that reported it.
- **The dashboard's state**: every group with its count, the reports behind it, whether it is ready, and what writers have already done (promoted, merged, ignored, rejected).
- **A read-only clone** of the default branch, for checking a claim against the code it names.

## What you may propose

- **A merge** of two groups with the same target, into one group. Keep the more precise key, or coin a better one, and give a one-line reason.
- **A link** between groups with different targets that describe one underlying problem. A link needs no writer command: it is information for the reader.
- **A merge of a new group into an existing promoted or ignored group**, when the new reports are the same problem.
- **An issue draft** for a ready group: a title, a body, and the group key as its idempotency key. In a private repository keep the general lesson and the private evidence in separate fields, so only the lesson can be published later.
- **Nothing at all.** An empty proposal is a good outcome when the log is already well grouped.

## What you never do

- Split, rename or reopen a group.
- Touch a group a writer has merged.
- Re-propose a pair a writer rejected. The dashboard state lists rejections; a rejected pair never appears again.
- Promote a `docs:`-target part of a private repository, or publish anything from one.
- Guess at evidence. A proposal cites the report ids it rests on; if the log doesn't support it, drop it.
- Write to GitHub, push, or change the clone.

## Output

Write the run's output file, `curator-output.json`, as its prompt describes:

- `version`: always `1`.
- `merges`: each `id`, the `groups` involved, and a one-line `reason`.
- `links`: each pair of groups and why they are related.
- `issues`: one draft per ready group, with `group`, `title`, `body` and, in a private repository, `lesson` and `private_evidence` kept apart.

Give every proposal a stable key derived from the groups it names, so applying the same file twice changes nothing. Order the proposals by how much they would help a writer: merges that collapse the most reports first.
