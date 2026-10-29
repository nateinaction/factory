"""Asserts the rendered opencode.json and config.json agree with factory_models.

Usage: models_check.py <render dir>. Exits non-zero, naming the first mismatch.
"""

import json
import sys

ROLES = {"planner", "researcher", "implementer", "reviewer", "curator"}


def main(out: str) -> None:
    """Check every agent role is a named opencode agent with its own capped model alias."""
    with open(out + "/opencode.json") as f:
        oc = json.load(f)
    with open(out + "/config.json") as f:
        cfg = json.load(f)
    assert set(oc["agent"]) == ROLES == set(cfg["agents"]), "an agent role is missing"
    aliases = oc["provider"]["openrouter"]["models"]
    for role in ROLES:
        alias = oc["agent"][role]["model"].removeprefix("openrouter/")
        model = aliases[alias]
        assert model["id"] == cfg["agents"][role]["model"], role + ": model differs"
        assert model["limit"]["input"] == cfg["agents"][role]["context_cap"] == 200000, role + ": cap"
    family = lambda r: cfg["agents"][r]["model"].split("/")[0]
    assert family("reviewer") != family("implementer"), "reviewer and implementer share a model family"
    on = sorted(r for r in ROLES if cfg["agents"][r]["compaction"])
    assert on == ["implementer", "researcher"], "compaction on for " + str(on)
    for role in ROLES:
        egress = cfg["agents"][role]["egress"]["allowed_domains"]
        assert isinstance(egress, list), role + ": egress is not a list"
    assert "model" not in oc, "a global model would let an agent role fall back to it"


main(sys.argv[1])
