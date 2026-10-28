# Auditable handoff signals between agent roles on GitHub

Research for [Auditable handoff signals between agent roles on GitHub](https://github.com/nateinaction/factory/issues/16),
part of the map [Wayfinder: agent roles for the factory](https://github.com/nateinaction/factory/issues/4).
It is the fact base for the decision ticket
[How is an issue handed off between agent roles?](https://github.com/nateinaction/factory/issues/13).
The decision belongs to that ticket; this file only compares options.

Researched 2026-10-27 against docs.github.com (article bodies as published
that day), the GitHub GraphQL schema (introspected live), the REST timeline of
this repo's own issues, and the named tools' repositories at pinned commits.
Anything the sources do not settle is marked **unconfirmed**.

## Question

What can agent roles use to signal handoffs to each other on GitHub, and how
auditable is each option? For each candidate (labels, assignees, Projects status
fields, comments with machine-readable markers, PR review states, sub-issues and
dependencies, check runs, and the timeline/events API that records them):

- does GitHub record who changed it and when, can that be read back through the
  API, and is it kept indefinitely;
- can the change be made atomically, or at least so two agents can't both claim
  the same work;
- can a GitHub App set it with a repo-scoped installation token;
- is it visible to a human in the issue/PR UI;
- what do other tools use for the same job?

## Today

`finances-agent` (`roles/agent/files/finances-agent`) moves an issue through
`ready-for-agent -> agent-working -> ready-for-agent-review -> agent-reviewing
-> agent-done | agent-failed`, back to `ready-for-agent` on blocking findings.
A **claim** is `DELETE .../labels/{src}` followed by `POST .../labels` with
`dst`; a 404 on the delete means "someone else got there first". Every
transition also posts a plain-text comment. Reviews and retros carry an HTML
comment marker keyed by commit SHA (`<!-- agent-review:{sha} -->`) so a re-run
updates or skips. All writes use one GitHub App's repo-scoped installation token.

## Findings

### 1. The record: issue timeline, issue events, and their limits

- **Two read APIs record handoff changes with an actor and a timestamp.** Every
  issue event has `actor` ("The person who generated the event") and
  `created_at` ([event types][evtypes]). `GET /repos/{o}/{r}/issues/{n}/timeline`
  and `.../events` both exist ([timeline][timeline], [events][events]).
  Events made by an App also carry `performed_via_github_app`
  ([timeline][timeline], [events][events]).
- **Which events land where.** `labeled`, `unlabeled`, `assigned`, `unassigned`,
  `review_requested`, `reviewed`, `review_dismissed`, `closed`, `reopened`,
  `cross-referenced` are documented REST event types ([evtypes]). `commented`
  appears only in the timeline API, not the issue events API ([evtypes]).
  `assigner` is in issue events but not in timeline events ([evtypes]).
- **Newer relationships are recorded too, but not yet in the REST event-types
  page.** The GraphQL `IssueTimelineItemsItemType` enum (introspected
  2026-10-27) includes `PROJECT_V2_ITEM_STATUS_CHANGED_EVENT`,
  `ADDED_TO_PROJECT_V2_EVENT`, `SUB_ISSUE_ADDED_EVENT`, `PARENT_ISSUE_ADDED_EVENT`,
  `BLOCKED_BY_ADDED_EVENT`, `BLOCKING_ADDED_EVENT` (and their removals),
  `ISSUE_FIELD_CHANGED_EVENT`, `ISSUE_TYPE_CHANGED_EVENT` and
  `COMMENT_DELETED_EVENT`. `ProjectV2ItemStatusChangedEvent` has `actor`,
  `createdAt`, `previousStatus`, `status`, `project` and `wasAutomated`.
  **Observed:** the REST timeline of [Auditable handoff signals between agent roles on GitHub](https://github.com/nateinaction/factory/issues/16)
  returns `parent_issue_added`, `blocking_added` and `assigned` with actor and
  time; that of [How is an issue handed off between agent roles?](https://github.com/nateinaction/factory/issues/13)
  returns `blocked_by_added` and `commented`.
- **Retention.** The issue timeline and issue events pages state **no retention
  limit** ([timeline], [events]). That they are kept for the life of the issue
  is the normal reading but is **unconfirmed** by any explicit statement. Two
  things that look similar *do* expire and are not substitutes:
  - the repository activity events API (`/repos/{o}/{r}/events`) holds "up to
    300 events" from "the past 30 days" ([activity events][actevents]);
  - the organization audit log covers "the last 180 days" and only owners can
    read it ([audit log][auditlog]); a personal-account repo has none.
- **What can erase the record.** Deleting the issue removes it permanently; on
  a personal-account repo only that account can do it ([deleting an issue][delissue]).
  Deleting a comment leaves a `comment_deleted` timeline event, but the deleter's
  name is shown only to people with write access ([managing comments][modcomments]).
  Whether `labeled` events keep the label name after the label is renamed or
  deleted from the repo is **unconfirmed**.
- **Who did it, for an App.** The actor of every App write is the App's bot
  user. With one App for all agent roles, the record says "the App" did it, not
  which agent role. Telling roles apart from the record alone needs one App per
  agent role (see [GitHub identities for agent roles](https://github.com/nateinaction/factory/issues/9))
  or a role name inside a comment marker.

### 2. Atomicity: GitHub offers no compare-and-swap on issue state

- **No conditional writes are documented.** REST conditional requests
  (`etag` / `last-modified`) are described for `GET` only, as a rate-limit
  saving ([best practices][bestprac]). The labels page mentions no `If-Match`
  ([labels]). No GraphQL mutation used here takes an expected-state input
  (**unconfirmed** beyond the mutations checked).
- **GitHub advises against concurrency for its own reasons:** "make requests
  serially instead of concurrently" to avoid secondary rate limits
  ([best practices][bestprac]).
- **What *does* fail closed:**
  - `DELETE .../labels/{name}` "returns a 404 Not Found status if the label does
    not exist" ([labels]). `finances-agent` relies on this meaning "not on this
    issue"; the sentence is ambiguous between "not on the issue" and "not in the
    repo". Whether two concurrent deletes of the same label are serialized, so
    exactly one gets 200, is **unconfirmed**. The swap is two requests, so a
    crash between them leaves the issue with neither label (stranded, but not
    double-claimed: it fails closed).
  - **Sub-issue parent is exclusive.** Adding a sub-issue takes
    `replace_parent`, "when true, instructs the operation to replace the
    sub-issues current parent issue" ([sub-issues][subissues]); so without it an
    issue that already has a parent is not silently re-parented. The exact error
    returned is **unconfirmed**. Limits: 100 sub-issues per parent, eight levels
    ([adding sub-issues][subissuesabout]).
  - **Git refs.** `PATCH .../git/refs/{ref}` with `force` false "will make sure
    you're not overwriting work" (fast-forward only) ([refs]), which is a true
    compare-and-swap on a branch. That creating an existing ref fails with 422 is
    observed practice; the page only says 422 is "Validation failed"
    (**unconfirmed** wording).
- **What does not exclude anyone:** assignees (up to 10 per issue, "Users already
  assigned to an issue are not replaced" ([assignees]),
  [assigning issues][assigndoc]), PR reviews (any number per PR), comments
  (append-only), check runs (many per commit), blocked-by edges, and Projects
  single-select status (last write wins; **unconfirmed** that there is no
  conflict check).
- **Practical pattern without CAS:** claim, then read back and verify. For
  example: post a claim comment with a unique run id, then list comments and let
  the earliest claim win. This needs comment ids or `created_at` to order
  totally; that ordering is **unconfirmed** in the docs.

### 3. Per-candidate facts

#### Labels

- Record: `labeled` / `unlabeled` with actor and time, in both read APIs
  ([evtypes]). Shown in the issue timeline UI.
- Atomic: no. The single-request 404 on removal is the only fail-closed hook
  (see above). `PUT .../labels` replaces the whole set in one request
  ([labels]) but has no expected-state check.
- App with repo token: yes. Issue label endpoints work with installation tokens
  under Issues **or** Pull requests write ([App permissions][appperms]).
- Queryable as a work queue: yes, `GET /repos/{o}/{r}/issues?labels=...`.
  Anyone with triage or write access can also flip them by hand, which is both
  the human gate and a way to spoof a handoff.

#### Assignees

- Record: `assigned` / `unassigned` with actor, plus `assigner` in issue events
  ([evtypes]). Shown in the UI sidebar and timeline.
- Atomic: no; additive, up to 10. Invalid assignees "are silently ignored"
  ([assignees]), so a claim can fail without an error.
- App with repo token: the endpoints accept installation tokens
  ([App permissions][appperms]). But the docs list who can be assigned as
  people (and Copilot) ([assigning issues][assigndoc]); whether an App's own bot
  user can be assigned is **unconfirmed** and believed not. So an App can assign
  a *person*, not itself: an assignee can name the human driving the work but not
  which agent role holds it.

#### Projects (v2) status field

- Record: `project_v2_item_status_changed` with actor, previous and new status,
  and `wasAutomated` (GraphQL schema, introspected). Whether REST timeline
  returns it is **unconfirmed**. A community thread reports GraphQL
  `timelineItems` returning only `labeled` events in one migration case
  ([community 57326][comm57326], not GitHub staff).
- Atomic: no.
- App with repo token: **no for this repo, as far as the docs show.** Projects
  write is an *organization* permission for Apps ([App permissions][appperms]);
  there is no user-level Projects permission in that list, so an installation
  token cannot reach a project owned by a personal account like `nateinaction`
  (**unconfirmed** by an explicit statement; inferred from the permission list).
  In an org it needs an org-scoped permission beyond the repo.
- Visible: in the project board, and as a timeline entry on the issue.
- Related: **issue fields** (custom fields on issues, `.../issue-field-values`)
  are defined at the organization level ([issue field values][issuefields],
  [App permissions][appperms]), so they share the "org only" limit.

#### Comments with machine-readable markers

- Record: the comment itself (author, `createdAt`), `commented` in the timeline
  API ([evtypes]). Edits keep a history readable by anyone with read access
  (`IssueComment.userContentEdits`, "edited" dropdown); GitHub keeps the
  original plus the latest 99 edits ([comment history][commenthistory]).
  Authors and writers can delete a revision's content, but "the person who made
  the edit and when they made the edit is still visible" ([comment history][commenthistory]).
  Anyone with write access can edit or delete any comment ([managing comments][modcomments]).
- Atomic: no (append-only). Good for keyed idempotence (`finances-agent`'s
  `<!-- agent-review:{sha} -->`), not for exclusion on its own.
- App with repo token: yes, Issues or Pull requests write ([App permissions][appperms]).
- Visible: yes; an HTML-comment marker is invisible in rendered Markdown but in
  the raw body; a bracketed marker (`[role:verdict]`) is visible.
- Expressiveness: the richest. It can carry the role, run id, commit SHA and
  verdict, which no other signal can.

#### PR review states

- Record: the review (author, `commit_id`, `submitted_at`, state) and
  `reviewed` / `review_dismissed` / `review_requested` timeline events
  ([reviews], [evtypes]). "Submitted reviews cannot be deleted"; they can be
  dismissed, which is itself recorded ([reviews]).
- Atomic: no. Not a claim mechanism; it is a verdict bound to a commit.
- App with repo token: yes, Pull requests write ([App permissions][appperms]).
  An App cannot approve a PR it authored; see
  [GitHub identities for agent roles](https://github.com/nateinaction/factory/issues/9).
- Visible: yes, on the PR. Only exists on PRs, not issues.

#### Sub-issues and dependencies (blocked-by)

- Record: `sub_issue_added`, `parent_issue_added`, `blocked_by_added`,
  `blocking_added` and removals, with actor (schema; observed on this repo).
- Atomic: parent is exclusive (see section 2); dependencies are not.
- App with repo token: yes, Issues write ([App permissions][appperms]).
- Visible: yes, in the issue sidebar and timeline.
- Role in a handoff: ordering (what is unblocked) and decomposition, not "whose
  turn". Wayfinder uses them exactly so ([wayfinder][mp-wayfinder]).

#### Check runs

- Record: on the commit, not the issue timeline. GitHub deletes the oldest
  check runs once a suite exceeds 1000 with the same name ([check runs][checkruns]);
  other retention is **unconfirmed**.
- Atomic: no.
- App with repo token: yes, and **only** an App can write them: "Write
  permission for the REST API to interact with checks is only available to
  GitHub Apps" ([check runs][checkruns], [checks guide][checksguide]). So a
  check run's author is unforgeable by a person.
- Visible: on the PR's checks; can be a required status check.
- Role in a handoff: a Reviewer verdict that gates merge, keyed to the head SHA.
  Not useful for issue-level state.

### 4. What other tools use

| Tool | Handoff signal | Claim | Source |
|---|---|---|---|
| Matt Pocock's skills | Triage-state labels (`needs-triage`, `ready-for-agent`, `ready-for-human`, ...), the agent brief comment as the contract; for wayfinder, type labels, native blocked-by, a resolution comment | **Assignee** ("That assignee _is_ the claim"); `gh issue edit <n> --add-assignee @me` as the session's first write | [wayfinder][mp-wayfinder], [issue-tracker-github][mp-tracker] |
| Poteto mode (pstack) | Core: in-session delegation to subagents, then a PR, CI and review bots (watched by `babysit`). The issue-driven `benny` pack: a Slack reply ending in exactly one marker such as `[benny:bug] tracker=...` | Trust by **author identity**: accept a marker only from the configured triage identity, in the source thread, exactly one marker; stop on conflicting or untrusted markers. "Stop when a person clearly claims the fix" | [poteto-mode SKILL.md][pm-skill], [benny reproduce-and-fix][benny-fix], [benny triage][benny-triage] |
| Elephant Goldfish | Files: drafts and review reports are written to workspace files or side-panel artifacts ("Artifact-First UX"); designs are checked-in "Living Specifications". No GitHub signal | None (one human-gated session) | [architecture][egm-arch], [egm-review-loop][egm-loop] |
| GitHub Copilot cloud agent | **Assignee**: assign the issue to Copilot; it opens a draft PR and requests your review when done. On a PR comment it adds an eyes reaction and a "Copilot started work" timeline event. Commits link to session logs | Assignment; API assignment requires a **user** token, and the agent tasks API rejects installation tokens ("Server-to-server tokens ... are not supported") | [use on GitHub][cop-gh], [via the API][cop-api], [managing sessions][cop-track] |

Others (for example GitHub Agentic Workflows) were not examined.

## Comparison

"Repo token" = settable with a repo-scoped GitHub App installation token.

| Signal | Who/when recorded, readable via API | Kept | Exclusive claim | Repo token | Visible to a human | Used by |
|---|---|---|---|---|---|---|
| Labels | Yes: `labeled`/`unlabeled`, actor, time (timeline + events) | No documented limit (unconfirmed "forever") | Partial: removal 404 fails closed; two requests; concurrency unconfirmed | Yes | Yes, list + timeline | Matt Pocock (triage), `finances-agent` |
| Assignees | Yes: `assigned`, actor (+ `assigner` in events) | As above | No (additive, up to 10, invalid silently ignored) | Yes, but can only assign people; App self-assign unconfirmed | Yes | Matt Pocock (wayfinder), Copilot |
| Projects status | GraphQL `project_v2_item_status_changed` with previous/new; REST unconfirmed | As above | No | **No** for a personal-account project; org permission in an org | Yes, board + timeline | none of the four |
| Marker comments | Yes: author, time, edit history (100 edits), deletion event | As above; content can be redacted or deleted by writers | No (append-only; read-back election possible) | Yes | Yes | benny (Slack), `finances-agent` reviews/retros, Matt Pocock (brief, resolution) |
| PR review states | Yes: review + `reviewed`/`review_dismissed`, bound to `commit_id` | Cannot be deleted once submitted | No | Yes (not on own PR) | Yes, PR only | `finances-agent`, Copilot (review request) |
| Sub-issues / blocked-by | Yes: add/remove events, actor | As above | Parent exclusive; deps no | Yes | Yes | Matt Pocock (wayfinder) |
| Check runs | On the commit, App-authored only | Oldest dropped past 1000 per name per suite | No | Yes (App only) | Yes, PR checks | none of the four |
| Git ref (branch) | Push/ref history; not on the issue timeline | Activity API 30 days; ref itself persists | **Yes**: fast-forward-only update is CAS | Yes (Contents write) | Indirectly (Development / branch) | `finances-agent` (`agent/issue-N`) |

## Recommendation for the decision ticket

For [How is an issue handed off between agent roles?](https://github.com/nateinaction/factory/issues/13)
to decide; offered, not decided:

1. **Keep labels as the state pointer.** They are the only signal that is at
   once queryable as a work queue, recorded with actor and time, settable with
   the repo token, visible, and flippable by a human as the gate. This is also
   what Matt Pocock's skills use up to `ready-for-agent`.
2. **Pair every transition with a marker comment** naming the agent role, run
   id, from-state, to-state and commit SHA. That turns the label record (which
   only says "the App") into an auditable handoff log, and gives the next role a
   contract to read, as the agent brief and benny's markers do. Trust a marker
   only from the expected identity, as benny does.
3. **Keep the claim fail-closed and add a read-back.** No GitHub issue signal is
   a true compare-and-swap. The label-removal 404 plus a check that the claim
   comment is the earliest for that transition covers the race GitHub doesn't.
   If a hard guarantee is needed, a branch ref is the one real CAS available.
4. **Prefer one App per agent role** so the timeline's actor names the role
   without relying on comment text; this ties to
   [GitHub identities for agent roles](https://github.com/nateinaction/factory/issues/9).
5. **Don't use Projects status** while the factory serves personal-account repos
   (installation tokens can't reach user-owned projects, per the permission
   list), and **don't use assignees as an agent claim** (an App can't name
   itself, and failures are silent). Assignees stay useful for naming the human.
6. **Use review states and, optionally, a check run on the PR** for the
   Reviewer's verdict; both bind to a commit SHA and a check run is unforgeable
   by a person.

## Sources

[evtypes]: https://docs.github.com/en/rest/using-the-rest-api/issue-event-types
[timeline]: https://docs.github.com/en/rest/issues/timeline
[events]: https://docs.github.com/en/rest/issues/events
[labels]: https://docs.github.com/en/rest/issues/labels
[assignees]: https://docs.github.com/en/rest/issues/assignees
[assigndoc]: https://docs.github.com/en/issues/tracking-your-work-with-issues/using-issues/assigning-issues-and-pull-requests-to-other-github-users
[subissues]: https://docs.github.com/en/rest/issues/sub-issues
[subissuesabout]: https://docs.github.com/en/issues/tracking-your-work-with-issues/using-issues/adding-sub-issues
[reviews]: https://docs.github.com/en/rest/pulls/reviews
[checkruns]: https://docs.github.com/en/rest/checks/runs
[checksguide]: https://docs.github.com/en/rest/guides/using-the-rest-api-to-interact-with-checks
[refs]: https://docs.github.com/en/rest/git/refs
[appperms]: https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps
[issuefields]: https://docs.github.com/en/rest/issues/issue-field-values
[bestprac]: https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api
[actevents]: https://docs.github.com/en/rest/activity/events
[auditlog]: https://docs.github.com/en/organizations/keeping-your-organization-secure/managing-security-settings-for-your-organization/reviewing-the-audit-log-for-your-organization
[delissue]: https://docs.github.com/en/issues/tracking-your-work-with-issues/administering-issues/deleting-an-issue
[commenthistory]: https://docs.github.com/en/communities/moderating-comments-and-conversations/tracking-changes-in-a-comment
[modcomments]: https://docs.github.com/en/communities/moderating-comments-and-conversations/managing-disruptive-comments
[comm57326]: https://github.com/orgs/community/discussions/57326
[mp-wayfinder]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/wayfinder/SKILL.md
[mp-tracker]: https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/setup-matt-pocock-skills/issue-tracker-github.md
[pm-skill]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/skills/poteto-mode/SKILL.md
[benny-fix]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/automations/benny/skills/reproduce-and-fix-issues/SKILL.md
[benny-triage]: https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/automations/benny/skills/triage-issue-reports/SKILL.md
[egm-arch]: https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/docs/designs/architecture.md
[egm-loop]: https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/skills/egm-review-loop/SKILL.md
[cop-gh]: https://docs.github.com/en/copilot/how-tos/use-copilot-agents/cloud-agent/use-cloud-agent-on-github
[cop-api]: https://docs.github.com/en/copilot/how-tos/use-copilot-agents/cloud-agent/use-cloud-agent-via-the-api
[cop-track]: https://docs.github.com/en/copilot/how-tos/copilot-on-github/use-copilot-agents/manage-and-track-agents

- Issue event types: <https://docs.github.com/en/rest/using-the-rest-api/issue-event-types>
- Timeline events: <https://docs.github.com/en/rest/issues/timeline>
- Issue events: <https://docs.github.com/en/rest/issues/events>
- Labels: <https://docs.github.com/en/rest/issues/labels>
- Assignees: <https://docs.github.com/en/rest/issues/assignees>, <https://docs.github.com/en/issues/tracking-your-work-with-issues/using-issues/assigning-issues-and-pull-requests-to-other-github-users>
- Sub-issues: <https://docs.github.com/en/rest/issues/sub-issues>, <https://docs.github.com/en/issues/tracking-your-work-with-issues/using-issues/adding-sub-issues>
- Reviews: <https://docs.github.com/en/rest/pulls/reviews>
- Check runs: <https://docs.github.com/en/rest/checks/runs>, <https://docs.github.com/en/rest/guides/using-the-rest-api-to-interact-with-checks>
- Git refs: <https://docs.github.com/en/rest/git/refs>
- App permissions: <https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps>
- Issue field values: <https://docs.github.com/en/rest/issues/issue-field-values>
- REST best practices: <https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api>
- Activity events: <https://docs.github.com/en/rest/activity/events>
- Org audit log: <https://docs.github.com/en/organizations/keeping-your-organization-secure/managing-security-settings-for-your-organization/reviewing-the-audit-log-for-your-organization>
- Deleting an issue: <https://docs.github.com/en/issues/tracking-your-work-with-issues/administering-issues/deleting-an-issue>
- Comment history and moderation: <https://docs.github.com/en/communities/moderating-comments-and-conversations/tracking-changes-in-a-comment>, <https://docs.github.com/en/communities/moderating-comments-and-conversations/managing-disruptive-comments>
- GraphQL schema: `IssueTimelineItemsItemType`, `ProjectV2ItemStatusChangedEvent`, `CommentDeletedEvent`, `IssueComment`, introspected via `https://api.github.com/graphql` on 2026-10-27
- Community report (not GitHub staff): <https://github.com/orgs/community/discussions/57326>
- Matt Pocock's skills at `49dd158`: <https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/wayfinder/SKILL.md>, <https://github.com/mattpocock/skills/blob/49dd158d1076134a641b33efb035946536778336/skills/engineering/setup-matt-pocock-skills/issue-tracker-github.md>
- Poteto mode / pstack at `c742dd6`: <https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/skills/poteto-mode/SKILL.md>, <https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/automations/benny/skills/reproduce-and-fix-issues/SKILL.md>, <https://github.com/cursor/plugins/blob/c742dd61706fbacc1b95f1a20b7b988cf096ce6c/pstack/automations/benny/skills/triage-issue-reports/SKILL.md>
- Elephant Goldfish at `a8380ab`: <https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/docs/designs/architecture.md>, <https://github.com/google/elephant-goldfish-model/blob/a8380ab5f0aba392c97e31c79f9a6666b3f09f14/skills/egm-review-loop/SKILL.md>
- Copilot cloud agent: <https://docs.github.com/en/copilot/how-tos/use-copilot-agents/cloud-agent/use-cloud-agent-on-github>, <https://docs.github.com/en/copilot/how-tos/use-copilot-agents/cloud-agent/use-cloud-agent-via-the-api>, <https://docs.github.com/en/copilot/how-tos/copilot-on-github/use-copilot-agents/manage-and-track-agents>
