# GitHub identities for agent roles

Research for #9 (map #4), fact base for #2. Researched 2026-10-27 against
docs.github.com and GitHub's changelog. Where the docs are silent, that is said,
and any community claim is labelled as such.

## Question

What identity options exist for agent roles acting on GitHub: one GitHub App
for all agent roles, an App per agent role, or machine-user accounts? Can a PR
opened by one App be approved by another App, and does that approval satisfy
branch protection, required reviews and CODEOWNERS? What do token scoping and
per-repository installation look like for each option?

## Today

One GitHub App (`roles/github_app`): `app-id`, `installation-id`,
`private-key.pem` under `/etc/github-app`, and `github-app-token`, which mints a
one-hour installation token scoped to the repo. The Implementer opens PRs with
it, so the App is the PR author, and the Reviewer's `APPROVE` with the same
token gets HTTP 422 (#2).

## Facts

### Authors cannot approve their own PRs

- "Pull request authors cannot approve their own pull requests." [1]
- The author is the identity whose token created the PR. All installation
  tokens of one App act as that App's bot user, so **one App for all agent roles
  can never approve or request changes on its own PRs.** That is the 422 in #2.
  Two installations of the same App are still the same bot, so the second
  identity must be a different App (or a user).

### Which approvals count toward required reviews

- Branch protection and rulesets count approvals "from people with write
  permissions in the repository" [2][3]. The approving-a-PR page says approvals
  "from people with write or admin permissions" count toward merge
  requirements, and others "do not affect whether the pull request can be
  merged" [1].
- **The docs do not say whether a GitHub App's approval counts.** The pages
  describe reviewers as people and teams [2][3]. Community answers (not
  GitHub staff, unsourced) say approvals from a dedicated GitHub App or a
  machine-user PAT do count, unlike `github-actions[bot]` approvals [6].
  Treat it as **unverified until tested**: open a PR with App A, approve with
  App B (which has Pull requests write and, to be safe, Contents write), and
  check `mergeable_state` / the PR's merge box against a rule requiring 1
  approval.
- Precedent that GitHub treats bot approvals as a policy question, not a given:
  - `GITHUB_TOKEN` approvals: a repo/org setting "Allow GitHub Actions to create
    and approve pull requests", off by default for new personal repos [7]; the
    2022 changelog introduced it "to protect against a user using Actions to
    satisfy the 'Required approvals' branch protection requirement" [8].
  - Copilot approvals count toward required approvals only when an admin turns
    that on (changelog, 2026-09-01, public preview) [9].
- Optional rule: "Require approval of the most recent reviewable push": the
  latest push must be approved "by someone other than the person who pushed it"
  [2][3]. With separate Implementer and Reviewer identities this is satisfiable
  by the Reviewer; with one identity it is not.
- "Dismiss stale pull request approvals when commits are pushed" re-requires
  approval after each Implementer fix-up push [2].

### CODEOWNERS

- Code owners are `@username`, `@org/team-name`, or an email on a user account,
  and "must have write permissions for the repository" [4].
- With "Require review from Code Owners" on, approval from any one listed owner
  satisfies it [4].
- **A GitHub App cannot be a code owner.** The docs name only users and teams
  [4]; a GitHub staff reply in the old community forum says Apps are not
  supported in CODEOWNERS [10]. A machine user (a real user account) can be a
  code owner if it has explicit write access.
- Consequence: if CODEOWNERS review is required, an App-based Reviewer cannot
  satisfy it; only a human or a machine user can. This is also the mechanism
  for "keep a human in the merge path" (#2).

### Rulesets and bypass

- Rulesets' pull-request rule: 0-10 required approvals, code-owner review,
  last-push approval [3].
- Team-scoped "Required reviewers" by file pattern exist, but the team needs
  write access and **this rule is not available on user-owned repositories**
  [3]. `nateinaction/*` repos are user-owned.
- Bypass lists can include "users with certain roles, specific teams, or GitHub
  Apps" [3]. Bypass skips a rule; it does not satisfy it. For classic branch
  protection, "actors may only be added to bypass lists when the repository
  belongs to an organization" [2].

### Token scoping and installation

GitHub App (installation token) [5][11][12]:

- Token lasts 1 hour. Request body can narrow it with `repositories` /
  `repository_ids` (up to 500) and `permissions`; it "cannot be granted
  permissions that the app was not granted."
- The installing owner chooses which repositories the App can reach; each
  installation is per account (user or org), with repo selection inside it.
- A user or org can register up to 100 Apps. An App can be "Only on this
  account" (private) or "Any account" (public).
- No seat consumed; rate limits scale with repositories; actions are shown as
  the App.
- Endpoint permissions: create PR = Pull requests write; create review = Pull
  requests write; merge = Contents write [12].

Machine user (PAT) [13][14][15]:

- A real user account. ToS: "set up by an individual human who accepts the
  Terms on behalf of the Account, provides a valid email address, and is
  responsible for its actions"; at most one free machine account in addition
  to your free personal account [14].
- Is an org member/collaborator, so in an org on a paid plan it uses a seat
  (an App does not [11]).
- Fine-grained PAT: one resource owner, optionally selected repos, expiry 1-366
  days or none (unless an org policy blocks it), default 30 days [15]. GitHub
  recommends an App over a PAT "for long-lived integrations" [15].
- Long-lived secret on the host instead of a key that mints 1-hour tokens.
- Can be added as a collaborator with write, so its approvals are documented
  as counting, and it can be a code owner.

## Options compared

| | One App, all agent roles | App per agent role | Machine user per agent role |
|---|---|---|---|
| Reviewer can APPROVE / REQUEST_CHANGES Implementer PRs | No (author rule, 422) [1] | Yes (different bot) | Yes |
| Approval counts toward required approvals | n/a | Undocumented; community says yes; test it | Yes, with write access [1][2] |
| Satisfies last-push-approval rule | No | Likely, same caveat | Yes |
| Can be a code owner | No | No [4][10] | Yes |
| Credential | 1 App key, 1-hour tokens | N App keys, 1-hour tokens | N PATs, long-lived (up to none) |
| Per-token scoping | repos + permissions per mint [5] | same, per App ceiling differs | fixed at PAT creation |
| Seat cost / ToS | none | none; ≤100 Apps per owner | seat in paid orgs; 1 free machine account per human [14] |
| Per-repo onboarding | install (or add repo to install) | install each App on the repo | add each user as collaborator |
| Least privilege per agent role | only via per-mint `permissions` (same key can mint anything) | App-level ceiling per agent role | PAT-level per agent role |

## Trade-offs worth grilling

- An App per agent role gives each its own permission ceiling, so a leaked
  Reviewer key cannot push code. One App with per-mint `permissions` narrows
  tokens, but the key itself can still mint the full set.
- Bot approval satisfying required reviews is exactly what GitHub gates
  elsewhere (Actions, Copilot) [8][9]. If App approval does count, a person who
  can apply `ready-for-agent` can get code approved by bots alone. Requiring
  CODEOWNERS review by a human keeps a person in the merge path, and Apps
  cannot fill that slot.
- Machine users fill human-only slots (CODEOWNERS) but cost a long-lived secret,
  a ToS-limited free account, and seats in paid orgs.
- Assigned repositories may be user-owned (no team-required-reviewers, no
  classic bypass lists) or org-owned; the onboarding step differs per option.

## Sources

1. Approving a pull request with required reviews — https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/reviewing-changes-in-pull-requests/approving-a-pull-request-with-required-reviews
2. About protected branches — https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches
3. Available rules for rulesets — https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets
4. About code owners — https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/about-code-owners
5. Generating an installation access token — https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app
6. Community discussion #181487 (community answers, not authoritative) — https://github.com/orgs/community/discussions/181487
7. Managing GitHub Actions settings for a repository — https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/enabling-features-for-your-repository/managing-github-actions-settings-for-a-repository
8. Changelog 2022-01-13, prevent GitHub Actions from approving PRs — https://github.blog/changelog/2022-01-13-github-actions-prevent-github-actions-from-approving-pull-requests
9. Changelog 2026-09-01, Copilot code review can approve PRs — https://github.blog/changelog/2026-09-01-copilot-code-review-can-now-approve-pull-requests
10. Community thread "Is it possible to add GitHub Apps Bot to CODEOWNERS?" (staff reply; old) — https://github.community/t/is-it-possible-to-add-github-apps-bot-to-codeowners/3146
11. Deciding when to build a GitHub App — https://docs.github.com/en/apps/creating-github-apps/about-creating-github-apps/deciding-when-to-build-a-github-app ; Registering a GitHub App — https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app
12. Permissions required for GitHub Apps — https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps
13. Types of GitHub accounts — https://docs.github.com/en/get-started/learning-about-github/types-of-github-accounts
14. GitHub Terms of Service, B.3 Account Requirements — https://docs.github.com/en/site-policy/github-terms/github-terms-of-service
15. Managing your personal access tokens — https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens
