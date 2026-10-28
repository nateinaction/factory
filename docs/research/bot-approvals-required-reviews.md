# Do a second App's approvals count toward required reviews?

Research for [Do a second App's approvals count toward required reviews?](https://github.com/nateinaction/factory/issues/18).
Context: [What identity does each agent role act as?](https://github.com/nateinaction/factory/issues/14)
gives each agent role its own GitHub App, and the Reviewer App posts native PR reviews on PRs authored by another App.

Researched 2026-10-27.

Evidence labels:

- **Documented**: GitHub (or the vendor) states it in official docs or the changelog.
- **Reported**: observed in practice and written up by users (issues, discussions, blog posts), with no official statement.
- **Unconfirmed**: inferred, or the sources are silent or contradict each other.

## Short answer

Yes for the approval count, as long as the Reviewer App has **write access to repository contents**. No for code owner review, because an App cannot be a code owner. The "most recent reviewable push" rule is satisfied by any approver other than the last pusher, so an App approval satisfies it. That means a human is only forced into the loop if a rule needs one: code owner review on owned paths, or a required-reviewer team.

GitHub's docs never say outright that App approvals count. Before relying on it, run a canary test on a throwaway repo (see the end of this file).

## Findings

### 1. Does an App's approval satisfy "required approving reviews"?

| Claim | Status | Source |
|---|---|---|
| Required approvals count reviews "from people with write permissions in the repository" or a code owner. Apps are not mentioned. | Documented (silent on Apps) | [About protected branches](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches), [Available rules for rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets) |
| An approval made with an App installation token does count. With only `pull_requests: write`, the review is created but excluded from the merge decision (`REVIEW_REQUIRED`). Granting `contents: write` makes the same review count (`APPROVED`). | Reported (controlled test, 2026-09-14) | [giantswarm/roadmap: marge sweep engine: verify a GitHub App approving review satisfies required approvals](https://github.com/giantswarm/roadmap/issues/4349) |
| A canary repo using classic protection (`required_approving_review_count: 1`, `enforce_admins: true`) went from `REVIEW_REQUIRED` to `APPROVED` after an App approval. The App held `contents: write` and `pull_requests: write`. | Reported (2026-10) | [One machine, ten coding agents: giving each one its own identity](https://dev.to/tiaforge/one-machine-ten-coding-agents-giving-each-one-its-own-identity-e20) |
| An App's `APPROVE` counts toward Required approvals "the same as a human reviewer's". The vendor confirmed it in code and fixed it as a P1 issue. | Reported (2026-10) | [mergewatch: MergeWatch's APPROVE reviews count toward required approvals and get past "most recent push" protection](https://github.com/mergewatch/mergewatch.ai/issues/706) |
| `github-actions[bot]` approvals made with `GITHUB_TOKEN` do **not** count. The answers there are from community members, not GitHub staff. The docs describe only the setting that lets `GITHUB_TOKEN` approve, not whether its approvals count. | Reported; behaviour Unconfirmed in docs | [community discussion: GHA bot review is not affecting merge requirements](https://github.com/orgs/community/discussions/181487), [Managing GitHub Actions settings](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/enabling-features-for-your-repository/managing-github-actions-settings-for-a-repository) |
| An App cannot approve a PR it authored. The API returns 422. "Pull request authors cannot approve their own pull requests." | Documented (for authors); Reported (for Apps) | [Approving a pull request with required reviews](https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/reviewing-changes-in-pull-requests/approving-a-pull-request-with-required-reviews), [giantswarm/roadmap issue](https://github.com/giantswarm/roadmap/issues/4349) |
| Classic branch protection and rulesets behave the same way here. | Unconfirmed. The giantswarm test does not name its protection type, and the dev.to test used classic protection. The docs use identical wording for both. | as above |

Implication for the factory: the Reviewer App needs `contents: write`, not just `pull_requests: write`. A least-privilege pass that removes `contents: write` would make its approvals stop counting, with no error. Separate Apps for Coder and Reviewer are what make the approval possible at all, because an App cannot approve its own PR.

### 2. Does it satisfy "require review from code owners"? Can an App be a code owner?

| Claim | Status | Source |
|---|---|---|
| CODEOWNERS entries are users (`@user` or email) and teams (`@org/team`), all with write access. Apps are not listed. | Documented | [About code owners](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/about-code-owners) |
| "Currently, GitHub Apps can't be used in CODEOWNERS – that's not supported." This was a GitHub staff answer in 2020. Users were still asking for it in July 2026, with no newer staff reply. | Reported (staff answer, community forum) | [community discussion: Is it possible to add GitHub Apps Bot to CODEOWNERS?](https://github.com/orgs/community/discussions/23064) |
| An App approval counts toward the approval total, but the PR stays `BLOCKED` when code owner review is required on a path the App touches. | Reported (controlled test) | [giantswarm/roadmap issue](https://github.com/giantswarm/roadmap/issues/4349) |
| The usual workaround is a machine user (a paid seat) added to a code-owner team. One vendor built a bot that reads CODEOWNERS and approves as a team member. | Reported | [community discussion 23064](https://github.com/orgs/community/discussions/23064), [FullStory: Taming GitHub CODEOWNERS with bots](https://fullstory.com/blog/taming-github-codeowners-with-bots) |

Implication: code owner review is the lever for "a human reviews only risky PRs". Put risky paths in CODEOWNERS with a human owner. The Reviewer App's approval then clears everything else but cannot clear those paths.

### 3. Does it satisfy "require approval of the most recent reviewable push"?

| Claim | Status | Source |
|---|---|---|
| The rule requires approval "from someone other than the last person to push to a branch". | Documented | [Available rules for rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets), [About protected branches](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches) |
| An App approval satisfies it when a different identity pushed. A bot that re-approves after each push lets new commits merge without a human. | Reported | [mergewatch issue](https://github.com/mergewatch/mergewatch.ai/issues/706) |
| When a different App (the Coder) is the last pusher and the Reviewer App approves, the rule is satisfied. | Unconfirmed (follows from the documented rule and the report above, not tested directly) | — |
| The rule does nothing to keep a human in the loop when the author and the reviewer are both Apps. | Unconfirmed (inference from the above) | — |

### 4. Can a ruleset bypass list or required-reviewer setting name an App?

| Claim | Status | Source |
|---|---|---|
| Ruleset bypass lists can include GitHub Apps, as well as roles, teams and Dependabot. The modes are "Always allow" and "For pull requests only". | Documented | [Creating rulesets for a repository](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/creating-rulesets-for-a-repository) |
| Classic protection has "Allow specified actors to bypass required pull requests". Renovate documents using it for its own PRs. | Documented | [Renovate: Automerge](https://docs.renovatebot.com/key-concepts/automerge/) |
| The ruleset "required reviewers" setting (review by path) takes **teams only**, up to 15. Apps cannot be named. It is not available on user-owned repositories. | Documented | [Available rules for rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets) |
| GitHub Apps can be named as actors allowed to dismiss reviews. | Documented | [Available rules for rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets) |

Caveat: `nateinaction/factory` is user-owned. Both bypass lists and required-reviewer teams need an organization repo, so neither is available here as things stand.

### 5. What do other bot reviewers report?

| Bot | What it does | Status | Source |
|---|---|---|---|
| **Copilot code review** | Its approvals do not count by default. Since 2026-09-01 (public preview), admins can enable approvals that "satisfy your repository's required-approval rule", limited by file globs, and new pushes dismiss them. One docs page still says Copilot reviews never count, so the docs are inconsistent. | Documented | [Changelog: Copilot code review can now approve pull requests](https://github.blog/changelog/2026-09-01-copilot-code-review-can-now-approve-pull-requests/), [Copilot code review](https://docs.github.com/en/copilot/concepts/agents/code-review), [Approving a PR with required reviews](https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/reviewing-changes-in-pull-requests/approving-a-pull-request-with-required-reviews) |
| **Copilot coding agent (authoring)** | When Copilot opens a PR under its own App identity, rulesets require **one more** approval than configured. The person who assigned Copilot cannot approve it. This is a special case for Copilot, and nothing in the docs says it applies to other Apps. | Documented | [Available rules for rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets), [Approving a PR with required reviews](https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/reviewing-changes-in-pull-requests/approving-a-pull-request-with-required-reviews) |
| **CodeRabbit** | "CodeRabbit's approval counts toward your Git provider's required approvals." The vendor recommends branch rules to require a human. | Documented (vendor) | [CodeRabbit: Automatic approvals](https://docs.coderabbit.ai/pr-reviews/automatic-approvals) |
| **Renovate (renovate-approve apps)** | Separate helper Apps that approve Renovate's automerge PRs, so the approver is not the author. Renovate also documents bypass as an alternative. A giantswarm audit found that its own renovate-approve approvals all landed on rulesets that already bypassed review, so they proved nothing. | Documented (vendor); Reported (caveat) | [Renovate: Automerge](https://docs.renovatebot.com/key-concepts/automerge/), [giantswarm/roadmap issue](https://github.com/giantswarm/roadmap/issues/4349) |
| **Mergify** | The `review` action posts `APPROVE`, and its Dependabot example is aimed at meeting required-review counts. Its docs do not say whether GitHub counts the review. It can also review as an impersonated user through `bot_account`. | Documented (silent on counting) | [Mergify: Review action](https://docs.mergify.com/workflow/actions/review/) |
| **MergeWatch** | Its App approvals counted and satisfied the most-recent-push rule. The vendor treated this as a P1 risk and changed the behaviour. | Reported | [mergewatch issue](https://github.com/mergewatch/mergewatch.ai/issues/706) |

## Open questions

- Ruleset-based protection has not been tested side by side with classic protection for App approvals.
- The exact permission behind the `contents: write` requirement is not in the docs. It is probably the documented "write permissions in the repository" rule applied to Apps.
- It is unknown whether GitHub will add more Copilot-style author rules (an extra required approval) for other App-authored PRs.

## Recommended canary test

Before building on this, verify it in a throwaway repo owned the same way as this one:

1. The Coder App opens a PR, and the Reviewer App approves it with `pull_requests: write` only. Expect `REVIEW_REQUIRED`.
2. Grant the Reviewer App `contents: write`. Expect `APPROVED`.
3. Enable "most recent reviewable push", have the Coder App push again, and have the Reviewer App approve. Expect `APPROVED`.
4. Add a CODEOWNERS entry with a human owner on one path and touch that path. Expect `BLOCKED` until the human approves.
5. Repeat 1–4 with a ruleset in place of classic protection.
