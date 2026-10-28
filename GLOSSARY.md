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
A step only a human may take; the one between planning and building is applying `ready-for-agent`.
_Avoid_: approval, sign-off

**Implementer**:
The agent role that turns an issue a human has made ready into a pull request that resolves it.
_Avoid_: worker, builder, coder

**Reviewer**:
The agent role that judges a pull request from a fresh context it shares with no author, and returns a verdict.
_Avoid_: QA, tester, checker

**Ansible role**:
A unit of host provisioning under `roles/`.
_Avoid_: role (bare)

**Assigned repository**:
A repository the factory takes issues from; agent roles hold nothing specific to any one of them.
_Avoid_: target repo, finances repo
