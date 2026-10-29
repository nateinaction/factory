# Factory

Hosts that take GitHub issues from assigned repositories and carry them to merged-ready pull requests with agents.

## Language

**Agent role**:
A named responsibility in an issue's lifecycle (for example Implementer or Reviewer), carried out by an agent run with its own skills, permissions and handoff.
_Avoid_: role (bare), persona, mode

**Planner**:
The agent role that works with a human, through the issue tracker, to take an idea to a spec or a wayfinder map ready for implementation.
_Avoid_: Designer, Triager, Wayfinder, Architect

**Researcher**:
The agent role that answers a research ticket from sources outside the repository and records its findings.
_Avoid_: investigator, scout

**Human gate**:
A step only a human may take; the one between planning and building is a writer confirming, in a comment, that the Planner's plan is ready to build.
_Avoid_: approval, sign-off

**Writer**:
A user with write access to an assigned repository; only a writer's issues and comments move the factory.
_Avoid_: maintainer, collaborator, trusted user

**Paused**:
An issue a human has taken out of the factory's hands; every other open issue in an assigned repository is the factory's.
_Avoid_: not-ready, opted out, blocked

**Implementer**:
The agent role that turns an issue a human has made ready into a pull request that resolves it.
_Avoid_: worker, builder, coder

**Reviewer**:
The agent role that judges a pull request from a fresh context it shares with no author, and returns a verdict.
_Avoid_: QA, tester, checker

**Curator**:
The agent role that looks after a repository's retro dashboard: it proposes merging groups, and writes the issue each promoted group becomes.
_Avoid_: librarian, triager, gardener

**Retro**:
An agent role's look back at its own run, giving findings about a skill, a repository's agent docs or the factory.
_Avoid_: postmortem, lessons learned

**Retro dashboard**:
The one issue per assigned repository that groups retro findings, counts repeat reports, and shows a writer which groups are ready to promote.
_Avoid_: retro ticket, findings log

**Foreman**:
The factory's program: it claims issues, starts each agent role's run and makes every change on GitHub, while the model only writes files.
_Avoid_: factory script, orchestrator, finances-agent

**Scheduler**:
The program that decides when foreman runs and where: cron, per-agent-role queues, run caps and dispatch to worker hosts. Dagu today.
_Avoid_: orchestrator, coordinator, workflow engine

**Worker host**:
A host that runs agent role runs for the factory; it takes the agent roles it lists, or all of them.
_Avoid_: runner, node, slot

**Security event**:
One structured log record for something the factory refused or detected, carrying a stable event name,
an outcome and the host concerned, and never a token, a key or a request header. The egress proxy and
the key proxy emit one for every request they refuse, and a run emits one for a forged marker, a
rejected output, a refused dashboard command and its step cap; docs are in `foreman/docs/sandbox.md`.
_Avoid_: security log, alert, audit line

**Ansible role**:
A unit of host provisioning under `roles/`.
_Avoid_: role (bare)

**Assigned repository**:
A repository the factory takes issues from; agent roles hold nothing specific to any one of them.
_Avoid_: target repo, finances repo
