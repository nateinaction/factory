"""Asserts the rendered opencode.json and config.json agree with the spec.

Usage: models_check.py <render dir>. Exits non-zero, naming the first mismatch. The expected values
come from the spec (issue #33), not from group_vars, so a wrong group_vars value fails here.
"""

import fnmatch
import json
import sys

ROLES = {"planner", "researcher", "implementer", "reviewer", "curator"}

# Skills and permissions: the spec's "Skills and permissions" tables.
SKILLS = {
    "planner": {
        "grilling",
        "domain-modeling",
        "wayfinder",
        "to-spec",
        "to-tickets",
        "writing-for-agents",
        "retro",
    },
    "researcher": {"research", "retro"},
    "implementer": {
        "implement",
        "tdd",
        "code-review",
        "diagnosing-bugs",
        "domain-modeling",
        "pr",
        "retro",
        "writing-for-agents",
    },
    "reviewer": {"code-review", "retro"},
    "curator": {"curate"},
}

# Step caps and deadlines: the spec's "Step caps and deadlines" table (minutes).
LIMITS = {
    "planner": (40, 20),
    "researcher": (80, 45),
    "implementer": (200, 90),
    "reviewer": (60, 30),
    "curator": (60, 30),
}
RETRO = (20, 15)
# The dead-run margin, in minutes, past an agent role's deadline (the spec's "Other limits").
DEAD_RUN_MARGIN = 10
# The review round limit, CI failures included, and the CI pending deadline in minutes.
REVIEW_ROUND_LIMIT = 3
CI_PENDING_DEADLINE_MINUTES = 60

OUTPUT_FILE = {
    "planner": "planner-output.json",
    "researcher": "researcher-output.json",
    "implementer": "implementer-output.json",
    "reviewer": "reviewer-output.json",
    "curator": "curator-output.json",
}


def permissions(role: str) -> dict:
    """The opencode agent's permission block."""
    return OC["agent"][role]["permission"]


def allowed(rule) -> set:
    """The patterns a granular rule allows, ignoring its catch-all."""
    if isinstance(rule, str):
        return {"*"} if rule == "allow" else set()
    return {
        pattern
        for pattern, action in rule.items()
        if action == "allow" and pattern != "*"
    }


def command_permission(rule, command: str) -> str:
    """The action opencode resolves for a command: the last matching rule wins."""
    if isinstance(rule, str):
        return rule

    action = None
    for pattern, effect in rule.items():
        if fnmatch.fnmatchcase(command, pattern):
            action = effect

    return action


def assert_common(role: str) -> None:
    """Every role denies by default, allows the clone's read tools and the web, and denies ask-only tools."""
    perm = permissions(role)
    assert perm["*"] == "deny", role + ": does not deny by default"
    for tool in ("read", "glob", "grep", "list", "webfetch", "websearch"):
        assert perm[tool] == "allow", role + ": " + tool + " is not allowed"
    for tool in ("question", "task", "doom_loop"):
        assert perm[tool] == "deny", role + ": " + tool + " is not denied"
    # Every external path is denied in the rendered config. foreman's per-run inline config is the
    # only place that names the run's own scratch directory, so no work-root wildcard may appear here.
    external = OC["permission"]["external_directory"]
    assert external == {"*": "deny"}, (
        role + ": external directories are not denied by default"
    )


def assert_skills(role: str) -> None:
    """The role loads exactly its skills; every other skill is denied."""
    skills = permissions(role)["skill"]
    assert skills["*"] == "deny", role + ": skills are not denied by default"
    assert allowed(skills) == SKILLS[role], (
        role + ": skills differ: " + str(sorted(allowed(skills)))
    )


def assert_edits(role: str) -> None:
    """The role edits exactly what the spec's edit column allows."""
    edit = permissions(role)["edit"]
    if role in ("implementer", "reviewer"):
        assert edit == "allow", role + ": the clone is not editable"
        return
    assert edit["*"] == "deny", role + ": edit does not deny by default"
    patterns = allowed(edit)
    if role == "planner":
        assert any(p.endswith("planner-output.json") for p in patterns), (
            "planner: no output file"
        )
    if role == "researcher":
        assert "docs/research/**" in patterns, (
            "researcher: docs/research is not editable"
        )
        assert any(p.endswith("researcher-output.json") for p in patterns), (
            "researcher: no output file"
        )
    if role == "curator":
        assert any(p.endswith("curator-output.json") for p in patterns), (
            "curator: no output file"
        )
    output = [p for p in patterns if p.endswith(".json")]
    assert output == sorted(output) and all(OUTPUT_FILE[role] in p for p in output), (
        role + ": extra output"
    )


READ_ONLY_GH = (
    "gh api --method GET",
    "gh api -X GET",
    "gh auth status",
    "gh issue list",
    "gh issue view",
    "gh label list",
    "gh pr checks",
    "gh pr diff",
    "gh pr list",
    "gh pr status",
    "gh pr view",
    "gh release list",
    "gh release view",
    "gh repo list",
    "gh repo view",
    "gh run list",
    "gh run view",
    "gh workflow list",
    "gh workflow view",
)

# Commands that must resolve to deny for every role: gh writes, and the api forms that mutate.
WRITE_COMMANDS = (
    "gh pr comment 5 --body fixed",
    "gh pr review 5 --approve",
    "gh pr merge 5 --squash",
    "gh pr create --title x --body y",
    "gh issue comment 5 --body hi",
    "gh api -f body=hi repos/o/r/issues/5/comments",
    "gh api repos/o/r/issues/5 -f body=hi",
    "gh api --field body=hi repos/o/r/issues/5/comments",
    "gh api --raw-field body=hi repos/o/r/issues/5/comments",
    "gh api --input payload.json repos/o/r/issues/5/comments",
    "gh api --method POST repos/o/r/issues/5/comments",
    "gh api -X POST repos/o/r/issues/5/comments",
    "git push origin HEAD",
)

READ_COMMANDS = (
    "git status",
    "git log --oneline",
    "gh pr view 5",
    "gh issue view 5",
    "gh pr list",
    "gh pr diff 5",
    "go test ./...",
)


def assert_shell(role: str) -> None:
    """Only the Implementer and Reviewer run arbitrary commands, and nobody pushes or writes via gh."""
    bash = permissions(role)["bash"]
    if role in ("implementer", "reviewer"):
        assert bash["*"] == "allow", role + ": bash is not allowed"
    else:
        assert bash["*"] == "deny", role + ": bash is not read-only"
        if role in ("planner", "curator"):
            assert allowed(bash), role + ": no read-only commands"

    for key, action in bash.items():
        if key.startswith("gh ") and action == "allow":
            assert key.removesuffix("*").rstrip() in READ_ONLY_GH, (
                role + ": gh write allowed: " + key
            )
    for command in WRITE_COMMANDS:
        assert command_permission(bash, command) == "deny", (
            role + ": write allowed: " + command
        )
    for command in READ_COMMANDS:
        effect = command_permission(bash, command)
        if effect == "deny" and role in ("implementer", "reviewer"):
            raise AssertionError(role + ": a read is denied: " + command)
        if (
            command.startswith("gh ")
            and role in ("planner", "curator")
            and any(
                word in command for word in (" view", " list", "--method GET", "-X GET")
            )
            and effect != "allow"
        ):
            raise AssertionError(
                role + ": a read-only gh command is denied: " + command
            )


def assert_steps(role: str) -> None:
    """The opencode agent carries the role's step cap, and foreman carries both limits."""
    steps, minutes = LIMITS[role]
    assert OC["agent"][role]["steps"] == steps, role + ": opencode step cap"
    assert CFG["agents"][role]["steps"] == steps, role + ": foreman step cap"
    assert CFG["agents"][role]["deadline_minutes"] == minutes, (
        role + ": foreman deadline"
    )


def assert_no_ask() -> None:
    """No permission anywhere asks: an unattended run can never answer a prompt."""

    def actions(rule) -> list:
        """Every action in a rule, including a granular rule's."""
        if isinstance(rule, dict):
            return [action for value in rule.values() for action in actions(value)]
        return [rule]

    for role in ROLES:
        for rule in permissions(role).values():
            assert "ask" not in actions(rule), role + ": a permission asks"


def main(out: str) -> None:
    """Check every agent role is a named opencode agent with its own capped model alias."""
    global OC, CFG

    with open(out + "/opencode.json") as f:
        OC = json.load(f)
    with open(out + "/config.json") as f:
        CFG = json.load(f)
    assert set(OC["agent"]) == ROLES == set(CFG["agents"]), "an agent role is missing"
    aliases = OC["provider"]["openrouter"]["models"]
    for role in ROLES:
        alias = OC["agent"][role]["model"].removeprefix("openrouter/")
        model = aliases[alias]
        assert model["id"] == CFG["agents"][role]["model"], role + ": model differs"
        assert (
            model["limit"]["input"] == CFG["agents"][role]["context_cap"] == 200000
        ), role + ": cap"
    family = lambda r: CFG["agents"][r]["model"].split("/")[0]
    assert family("reviewer") != family("implementer"), (
        "reviewer and implementer share a model family"
    )
    on = sorted(r for r in ROLES if CFG["agents"][r]["compaction"])
    assert on == ["implementer", "researcher"], "compaction on for " + str(on)
    assert "model" not in OC, "a global model would let an agent role fall back to it"

    for role in ROLES:
        assert_common(role)
        assert_skills(role)
        assert_edits(role)
        assert_shell(role)
        assert_steps(role)

    assert_no_ask()
    for role in ROLES:
        retro = {"steps": RETRO[0], "deadline_minutes": RETRO[1]}
        assert CFG["agents"][role]["retro"] == retro, role + ": retro budget"
    assert "retro" not in CFG and "retro_by_role" not in CFG, (
        "a retro budget outside the agent roles"
    )
    assert CFG["dead_run_margin_minutes"] == DEAD_RUN_MARGIN, "dead-run margin"
    assert CFG["review_round_limit"] == REVIEW_ROUND_LIMIT, "review round limit"
    assert CFG["ci_pending_deadline_minutes"] == CI_PENDING_DEADLINE_MINUTES, (
        "CI pending deadline"
    )
    for role in ROLES:
        egress = CFG["agents"][role]["egress"]["allowed_domains"]
        assert isinstance(egress, list), role + ": egress is not a list"


main(sys.argv[1])
