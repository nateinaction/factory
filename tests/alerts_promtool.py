#!/usr/bin/env python3
"""Write the promtool unit tests for the rendered Prometheus alert rules.

The rules arrive as JSON (tests/alerts.yml parses the rendered YAML for us). Each alert gets one
synthetic event whose series carry the label names foreman emits, so a rule that selects another
name fires nothing here. Each alert's expected labels are written out below, independently of the
rule under test, so a rule that changes its labels fails. Prometheus's {{ $labels.* }}
placeholders in the annotations are filled from the sample series. The output is JSON, which is
valid YAML, so promtool reads it without a YAML library here.
"""

import json
import re
import sys

# One synthetic event per alert: the series that makes it fire and the time to evaluate it. The
# label names are the ones foreman's metric attributes become in Prometheus (factory.event becomes
# factory_event), not the names a rule happens to select on.
SAMPLES = {
    "ForemanEgressNotRoutable": {
        "series": [
            'factory_security_events_total{factory_event="egress_refused",'
            'factory_outcome="refused_not_routable",factory_agent_role="implementer",'
            'factory_repo="o/r",factory_issue="5"}',
        ],
        "values": "0+1x10",
        "eval": "5m",
    },
    "ForemanProxyBadToken": {
        "series": [
            'factory_security_events_total{factory_event="key_proxy_refused",'
            'factory_outcome="refused_bad_token",factory_agent_role="implementer",'
            'factory_repo="o/r",factory_issue="5"}',
        ],
        "values": "0+1x10",
        "eval": "5m",
    },
    "ForemanOutputFactoryMarker": {
        "series": [
            'factory_security_events_total{factory_event="output_rejected",'
            'factory_outcome="factory_marker",factory_agent_role="implementer",'
            'factory_repo="o/r",factory_issue="5"}',
        ],
        "values": "0+1x10",
        "eval": "5m",
    },
    "ForemanAgentRoleFailureRate": {
        "series": [
            'factory_run_outcomes_total{factory_outcome="failed",factory_agent_role="implementer",factory_repo="o/r"}',
            'factory_run_outcomes_total{factory_outcome="completed",factory_agent_role="implementer",factory_repo="o/r"}',
        ],
        "values": "0+1x10",
        "eval": "10m",
    },
    "ForemanDailyModelCost": {
        "series": [
            'factory_run_cost_total{factory_agent_role="implementer",factory_repo="o/r",gen_ai_request_model="m"}',
        ],
        "values": "0+1x10",
        "eval": "10m",
    },
    "ForemanDeadRunReaped": {
        "series": [
            'factory_runs_reaped_total{factory_agent_role="implementer",factory_repo="o/r",factory_issue="5"}',
        ],
        "values": "0+1x10",
        "eval": "5m",
    },
}

# The labels each alert's series must carry once the rule has fired. These are written here, not
# copied from the rule: a rule that drops, renames or adds a label fails the test. A security alert
# aggregates away its event and outcome but keeps the issue; the reaped-run alert keeps the issue too;
# an aggregate alert keeps only the role and repo. No alert keeps a trace id.
EXPECTED_LABELS = {
    "ForemanEgressNotRoutable": {
        "severity": "critical",
        "factory_agent_role": "implementer",
        "factory_repo": "o/r",
        "factory_issue": "5",
    },
    "ForemanProxyBadToken": {
        "severity": "critical",
        "factory_agent_role": "implementer",
        "factory_repo": "o/r",
        "factory_issue": "5",
    },
    "ForemanOutputFactoryMarker": {
        "severity": "critical",
        "factory_agent_role": "implementer",
        "factory_repo": "o/r",
        "factory_issue": "5",
    },
    "ForemanAgentRoleFailureRate": {
        "severity": "warning",
        "factory_agent_role": "implementer",
        "factory_repo": "o/r",
    },
    "ForemanDailyModelCost": {
        "severity": "warning",
        "factory_agent_role": "implementer",
        "factory_repo": "o/r",
    },
    "ForemanDeadRunReaped": {
        "severity": "warning",
        "factory_agent_role": "implementer",
        "factory_repo": "o/r",
        "factory_issue": "5",
    },
}

# What the synthetic series' labels put where an annotation templates a label.
SAMPLE_LABELS = {
    "factory_agent_role": "implementer",
    "factory_repo": "o/r",
    "factory_issue": "5",
}

PLACEHOLDER = re.compile(r"\{\{\s*\$labels\.(\w+)\s*\}\}")


def expand(text):
    """Fill a rule's label placeholders the way Prometheus does for the sample labels."""
    return PLACEHOLDER.sub(lambda m: SAMPLE_LABELS[m.group(1)], text)


def main():
    source, dest = sys.argv[1], sys.argv[2]
    rendered = json.load(open(source))

    tests = []
    seen = set()

    for group in rendered["groups"]:
        for rule in group["rules"]:
            name = rule["alert"]
            seen.add(name)
            sample = SAMPLES[name]
            test = {
                "interval": "1m",
                "input_series": [
                    {"series": series, "values": sample["values"]} for series in sample["series"]
                ],
                "alert_rule_test": [
                    {
                        "eval_time": sample["eval"],
                        "alertname": name,
                        "exp_alerts": [
                            {
                                "exp_labels": EXPECTED_LABELS[name],
                                "exp_annotations": {
                                    key: expand(value)
                                    for key, value in rule.get("annotations", {}).items()
                                },
                            }
                        ],
                    }
                ],
            }
            tests.append(test)

    for expected, actual in ((SAMPLES, seen), (EXPECTED_LABELS, seen)):
        missing = set(expected) - actual
        if missing:
            raise SystemExit(f"alerts without a synthetic event or expected labels: {sorted(missing)}")

    with open(dest, "w") as out:
        json.dump(
            {"rule_files": ["alerts.yaml"], "evaluation_interval": "1m", "tests": tests},
            out,
            indent=2,
        )
        out.write("\n")


if __name__ == "__main__":
    main()
