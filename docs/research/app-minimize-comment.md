# Can a GitHub App minimize a writer's comment?

Research for [Can a GitHub App minimize a writer's comment?](https://github.com/nateinaction/factory/issues/28),
under the map [Wayfinder: agent roles for the factory](https://github.com/nateinaction/factory/issues/4).
Researched 2026-10-28. Documentation only: no App was created and no comment was hidden or deleted.

## Question

Can a GitHub App installation token with Issues write minimize (hide as resolved) a comment someone else wrote,
on an issue in a repository the App is installed on, through GraphQL `minimizeComment`?
Which permission does it need? Does it work on a personal-account repository?
If not, can the App delete the comment instead?

[How do retros feed back into skills?](https://github.com/nateinaction/factory/issues/22) hides a writer's
command comments on the retro dashboard after acting on them, and deletes them as the fallback.
The repo owner, `nateinaction`, is a personal account, not an organization.

## Answer in brief

- **Hiding as resolved exists.** `minimizeComment` takes a `subjectId` and a `classifier`.
  `RESOLVED` is one of the classifiers. It works on `IssueComment`.
- **Issues write is very likely enough.** GitHub's own docs do not name the App permission for `minimizeComment`.
  But GitHub's own Agentic Workflows tool hides other people's comments with an installation token that has only `issues: write`.
  That is strong evidence, not a confirmation for a custom App.
- **Personal-account repos: probably no difference for an App.** Roles (triage, write, maintain) apply to people.
  An App's access comes from its installation permissions. No source says personal-account repos treat Apps differently. Unconfirmed.
- **Deleting is confirmed.** `DELETE /repos/{owner}/{repo}/issues/comments/{comment_id}` is listed under Issues **write**
  and is available to installation tokens. GraphQL also has `deleteIssueComment`.
- **Cheap test:** with the App's token, read `viewerCanMinimize` on a writer's comment. Then minimize one test comment and unminimize it.

## Findings

### The mutation

The live GraphQL schema (introspected 2026-10-28 with `gh api graphql`) says:

- `minimizeComment`: "Minimizes a comment on an Issue, Commit, Pull Request, or Gist."
  `unminimizeComment` reverses it.
- `MinimizeCommentInput` has `subjectId` ("The Node ID of the subject to modify"), `classifier`, and `clientMutationId`.
- `ReportedContentClassifiers` includes `RESOLVED` ("The content has been resolved"),
  plus `SPAM`, `ABUSE`, `OFF_TOPIC`, `OUTDATED`, `DUPLICATE`, `LOW_QUALITY`.
- The `Minimizable` interface is implemented by `IssueComment`, `CommitComment`, `DiscussionComment`, `GistComment`,
  `PullRequestReview` and `PullRequestReviewComment`. Its fields are `isMinimized`, `minimizedReason`,
  `viewerCanMinimize` ("Check if the current viewer can minimize this object") and `viewerCanUnminimize`.
- `minimizedReason` comes back lower-case and hyphenated (`resolved`, `off-topic`), not in the input's form.

The schema documents no permission requirement for the mutation.
Reference pages: [GraphQL mutations](https://docs.github.com/en/graphql/reference/mutations#minimizecomment),
[Minimizable](https://docs.github.com/en/graphql/reference/interfaces#minimizable).

`subjectId` is the GraphQL node ID (`IC_...`). The REST comment object carries it as `node_id`.

### Who may hide a comment: people

- [Managing disruptive comments](https://docs.github.com/en/communities/moderating-comments-and-conversations/managing-disruptive-comments):
  "Organization moderators and anyone with write access to a repository, can hide comments on issues, discussions, pull requests, and commits."
  The same page: "Anyone with write access to a repository can delete comments on issues, discussions, pull requests, and commits."
- [Repository roles for an organization](https://docs.github.com/en/organizations/managing-user-access-to-your-organizations-repositories/managing-repository-roles/repository-roles-for-an-organization):
  "Hide anyone's comments on issues, pull requests, and commits" is Write, Maintain and Admin. Triage cannot.
  Triage can only hide comments on discussions. "Edit and delete anyone's comments" is also Write and up.
- [Permission levels for a personal account repository](https://docs.github.com/en/account-and-profile/reference/permission-levels-for-a-personal-account-repository):
  a personal-account repo has only the owner and collaborators. There is no triage or maintain role.
  Collaborators can "Create, edit, and delete comments on commits, pull requests, and issues in the repository".
  The table does not mention hiding. It links to "Managing disruptive comments", which grants hiding to anyone with write access.

These rules are about people. They do not say how a GitHub App is judged.

### Who may hide a comment: Apps

- [Permissions required for GitHub Apps](https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps)
  lists REST endpoints only. Under Repository permissions for "Issues":
  - `PATCH /repos/{owner}/{repo}/issues/comments/{comment_id}`: write, user and installation tokens.
  - `DELETE /repos/{owner}/{repo}/issues/comments/{comment_id}`: write, user and installation tokens.
  There is no REST endpoint for hiding, so the page says nothing about it.
- [Forming calls with GraphQL](https://docs.github.com/en/graphql/guides/forming-calls-with-graphql) says only that a missing permission returns an error naming the permission needed.
- No GitHub doc, changelog entry or community discussion found states the App permission for `minimizeComment`.

The strongest evidence is GitHub's own code:

- [`GITHUB_TOKEN`](https://docs.github.com/en/actions/concepts/security/github_token): "The `GITHUB_TOKEN` secret is a GitHub App installation access token."
- GitHub Agentic Workflows (`github/gh-aw`) has a `hide-comment` safe output that calls `minimizeComment`
  ([docs](https://github.com/github/gh-aw/blob/0bb03657909120604882bf514c9d391277abd634/docs/src/content/docs/reference/safe-outputs.md#hide-comment-hide-comment)).
  Its job gets only `issues: write` (plus `discussions: write` if discussions are on)
  ([`safe_output_handlers.go`](https://github.com/github/gh-aw/blob/0bb03657909120604882bf514c9d391277abd634/pkg/workflow/safe_output_handlers.go#L598-L608)).
  Its token may also be a custom App token (`github-app` setting).
- gh-aw's own [`ai-moderator`](https://github.com/github/gh-aw/blob/0bb03657909120604882bf514c9d391277abd634/.github/workflows/ai-moderator.md)
  workflow uses it to hide spam comments. It skips owners, members and collaborators, so every comment it hides was written by someone else.
- [int128/hide-comment-action](https://github.com/int128/hide-comment-action/tree/73ba7c2058b1d12502b287bf38622d19cf3e6e03)
  also calls `minimizeComment` with `github.token` and can filter by any `authors`.
  Its one permission bug ([issue 224](https://github.com/int128/hide-comment-action/issues/224)) was fork PRs getting a read-only token.

So an installation token with Issues write can very likely hide another person's issue comment.
Two gaps remain, both unconfirmed:

1. `GITHUB_TOKEN` belongs to GitHub's own `github-actions` App. A custom App with the same permission has not been shown to behave the same.
2. gh-aw runs in an organization repo. Nothing found says a personal-account repo differs for Apps,
   and App access is defined by installation permissions, not repository roles. But no one has reported the case.

A third unknown: whether a comment by the repository **owner** can be hidden by an App. Nothing says owners' comments are protected. Unconfirmed.

### Deleting instead

- REST: `DELETE /repos/{owner}/{repo}/issues/comments/{comment_id}` needs Issues **write** and works with installation tokens
  ([permissions page](https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps),
  [endpoint](https://docs.github.com/en/rest/issues/comments#delete-an-issue-comment)). Returns `204`.
- GraphQL: `deleteIssueComment` ("Deletes an IssueComment object") exists in the live schema.
- Deleting is permanent. The writer loses the record of their command. Hiding keeps it, collapsed, and anyone with read access can expand it.
- A second delete of the same comment returns `404`. A retrying caller should treat `404` as already done. Not confirmed from docs; it is standard REST behaviour.

### Other caveats

- [Community discussion 19865](https://github.com/orgs/community/discussions/19865): comments minimized via GraphQL can look different in the web UI from ones hidden with the UI button.
  The UI uses an undocumented endpoint. Cosmetic only.
- Minimizing twice is harmless as far as known: the comment stays minimized. Unconfirmed; check `isMinimized` first to stay idempotent.

## Cheap live test

All of these use a throwaway issue in `nateinaction/factory` and the App's installation token.

1. **Read-only pre-check.** Have the writer post a comment. With the App token, query
   `node(id: "<IC_...>") { ... on IssueComment { viewerCanMinimize viewerCanDelete } }`.
   `true` answers the question without changing anything.
2. **Minimize and revert.** Call `minimizeComment(input: {subjectId: "<IC_...>", classifier: RESOLVED})`,
   check `isMinimized` and `minimizedReason`, then `unminimizeComment`.
3. **If it fails,** the error names the missing permission. Then try `DELETE /repos/nateinaction/factory/issues/comments/{id}`
   on a second throwaway comment and confirm `204`.
4. Repeat step 1 on a comment by `nateinaction` (the owner) to close the third unknown.

## Options for the retro dashboard

Not decisions; inputs to [How do retros feed back into skills?](https://github.com/nateinaction/factory/issues/22).

- **A. Hide as `RESOLVED`, delete as fallback.** The plan in the parent ticket. App needs Issues write.
  Check `viewerCanMinimize` and `isMinimized` before acting, so a retry is a no-op.
  On a permission error, fall back to `DELETE`, treating `404` as done.
- **B. Hide only.** Same permission. If hiding fails, leave the comment and mark it handled another way
  (a reaction, or a reply naming the comment). Keeps every command on record.
- **C. Delete only.** Confirmed to work with Issues write. Simplest. Loses the writer's record of the command.
- **D. Don't touch the writer's comment.** Mark it handled with a reaction or a reply, and filter handled commands when reading.
  Needs no permission beyond posting. The dashboard stays noisier.
- For every option: run the live test above before building on it.

## Sources

- Live GitHub GraphQL schema, introspected 2026-10-28: `Mutation.minimizeComment`, `Mutation.unminimizeComment`, `Mutation.deleteIssueComment`, `MinimizeCommentInput`, `ReportedContentClassifiers`, `Minimizable`.
- [GraphQL mutations reference](https://docs.github.com/en/graphql/reference/mutations#minimizecomment), [Minimizable interface](https://docs.github.com/en/graphql/reference/interfaces#minimizable)
- [Forming calls with GraphQL](https://docs.github.com/en/graphql/guides/forming-calls-with-graphql)
- [Permissions required for GitHub Apps](https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps)
- [REST: Delete an issue comment](https://docs.github.com/en/rest/issues/comments#delete-an-issue-comment)
- [Managing disruptive comments](https://docs.github.com/en/communities/moderating-comments-and-conversations/managing-disruptive-comments)
- [Repository roles for an organization](https://docs.github.com/en/organizations/managing-user-access-to-your-organizations-repositories/managing-repository-roles/repository-roles-for-an-organization)
- [Permission levels for a personal account repository](https://docs.github.com/en/account-and-profile/reference/permission-levels-for-a-personal-account-repository)
- [GITHUB_TOKEN](https://docs.github.com/en/actions/concepts/security/github_token)
- [github/gh-aw at 0bb0365](https://github.com/github/gh-aw/tree/0bb03657909120604882bf514c9d391277abd634): `docs/src/content/docs/reference/safe-outputs.md`, `pkg/workflow/safe_output_handlers.go`, `.github/workflows/ai-moderator.md`; [PR 5911](https://github.com/githubnext/gh-aw/pull/5911)
- [int128/hide-comment-action at 73ba7c2](https://github.com/int128/hide-comment-action/tree/73ba7c2058b1d12502b287bf38622d19cf3e6e03), [issue 224](https://github.com/int128/hide-comment-action/issues/224)
- [Community discussion 19865](https://github.com/orgs/community/discussions/19865)
