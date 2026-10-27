# Factory

Hosts that take GitHub issues from assigned repositories and carry them to merged-ready pull requests with agents.

## Language

**Agent role**:
A named responsibility in an issue's lifecycle (for example Implementer or Reviewer), carried out by an agent run with its own skills, permissions and handoff.
_Avoid_: role (bare), persona, mode

**Ansible role**:
A unit of host provisioning under `roles/`.
_Avoid_: role (bare)

**Assigned repository**:
A repository the factory takes issues from; agent roles hold nothing specific to any one of them.
_Avoid_: target repo, finances repo
