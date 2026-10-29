"""Asserts the rendered opencode.json and config.json agree with the spec.

Usage: models_check.py <render dir>. Exits non-zero, naming the first mismatch. The expected values
come from the spec (issue #33), not from group_vars, so a wrong group_vars value fails here.
"""

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
    return {pattern for pattern, action in rule.items() if action == "allow" and pattern != "*"}


def assert_common(role: str) -> None:
    """Every role denies by default, allows the clone's read tools and the web, and denies ask-only tools."""
    perm = permissions(role)
    assert perm["*"] == "deny", role + ": does not deny by default"
    for tool in ("read", "glob", "grep", "list", "webfetch", "websearch"):
        assert perm[tool] == "allow", role + ": " + tool + " is not allowed"
    for tool in ("question", "task", "doom_loop"):
        assert perm[tool] == "deny", role + ": " + tool + " is not denied"
    external = perm["external_directory"]
    assert external["*"] == "deny", role + ": external directories are not denied by default"
    assert allowed(external), role + ": no scratch directory is allowed"


def assert_skills(role: str) -> None:
    """The role loads exactly its skills; every other skill is denied."""
    skills = permissions(role)["skill"]
    assert skills["*"] == "deny", role + ": skills are not denied by default"
    assert allowed(skills) == SKILLS[role], role + ": skills differ: " + str(sorted(allowed(skills)))


def assert_edits(role: str) -> None:
    """The role edits exactly what the spec's edit column allows."""
    edit = permissions(role)["edit"]
    if role in ("implementer", "reviewer"):
        assert edit == "allow", role + ": the clone is not editable"
        return
    assert edit["*"] == "deny", role + ": edit does not deny by default"
    patterns = allowed(edit)
    if role == "planner":
        assert any(p.endswith("planner-output.json") for p in patterns), "planner: no output file"
    if role == "researcher":
        assert "docs/research/**" in patterns, "researcher: docs/research is not editable"
        assert any(p.endswith("researcher-output.json") for p in patterns), "researcher: no output file"
    if role == "curator":
        assert any(p.endswith("curator-output.json") for p in patterns), "curator: no output file"
    output = [p for p in patterns if p.endswith(".json")]
    assert output == sorted(output) and all(OUTPUT_FILE[role] in p for p in output), role + ": extra output"


def assert_shell(role: str) -> None:
    """Only the Implementer and Reviewer run arbitrary commands, and nobody pushes or writes via gh."""
    bash = permissions(role)["bash"]
    if role in ("implementer", "reviewer"):
        assert bash["*"] == "allow", role + ": bash is not allowed"
        assert bash["git push*"] == "deny", role + ": git push is not denied"
        for key, action in bash.items():
            if key.startswith("gh ") and action == "allow":
                assert any(w in key for w in (" view", " list")), role + ": gh write allowed"
        return
    assert bash["*"] == "deny", role + ": bash is not read-only"
    if role in ("planner", "curator"):
        assert allowed(bash), role + ": no read-only commands"
    for key in bash:
        assert not key.startswith("git push"), role + ": git push allowed"
        if key.startswith("gh "):
            assert "--method GET" in key or any(w in key for w in (" view", " list")), (
                role + ": gh write allowed"
            )


def assert_steps(role: str) -> None:
    """The opencode agent carries the role's step cap, and foreman carries both limits."""
    steps, minutes = LIMITS[role]
    assert OC["agent"][role]["steps"] == steps, role + ": opencode step cap"
    assert CFG["agents"][role]["steps"] == steps, role + ": foreman step cap"
    assert CFG["agents"][role]["deadline_minutes"] == minutes, role + ": foreman deadline"


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
        assert model["limit"]["input"] == CFG["agents"][role]["context_cap"] == 200000, role + ": cap"
    family = lambda r: CFG["agents"][r]["model"].split("/")[0]
    assert family("reviewer") != family("implementer"), "reviewer and implementer share a model family"
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
    assert CFG["retro"] == {"steps": RETRO[0], "deadline_minutes": RETRO[1]}, "retro budget"
    assert CFG["dead_run_margin_minutes"] == DEAD_RUN_MARGIN, "dead-run margin"
    assert CFG["review_round_limit"] == REVIEW_ROUND_LIMIT, "review round limit"
    assert CFG["ci_pending_deadline_minutes"] == CI_PENDING_DEADLINE_MINUTES, "CI pending deadline"
    for role in ROLES:
        egress = CFG["agents"][role]["egress"]["allowed_domains"]
        assert isinstance(egress, list), role + ": egress is not a list"


main(sys.argv[1])
