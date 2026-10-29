#!/usr/bin/env python3
"""Write the promtool unit tests for the rendered Prometheus alert rules.

The rules arrive as JSON (tests/alerts.yml parses the rendered YAML for us). Each alert gets one
synthetic event, and its expected labels and annotations are read back from the rule itself, with
Prometheus's {{ $labels.* }} placeholders filled from the sample series. The output is JSON, which is
valid YAML, so promtool reads it without a YAML library here.
"""

import json
import re
import sys

# One synthetic event per alert: the series that makes it fire and the time to evaluate it.
SAMPLES = {
    "ForemanEgressNotRoutable": {
        "series": [
            'factory_security_events_total{event="egress_refused",outcome="refused_not_routable",'
            'factory_agent_role="implementer",factory_repo="o/r"}',
        ],
        "values": "0+1x10",
        "eval": "5m",
    },
    "ForemanProxyBadToken": {
        "series": [
            'factory_security_events_total{event="key_proxy_refused",outcome="refused_bad_token",'
            'factory_agent_role="implementer",factory_repo="o/r"}',
        ],
        "values": "0+1x10",
        "eval": "5m",
    },
    "ForemanOutputFactoryMarker": {
        "series": [
            'factory_security_events_total{event="output_rejected",outcome="factory_marker",'
            'factory_agent_role="implementer",factory_repo="o/r"}',
        ],
        "values": "0+1x10",
        "eval": "5m",
    },
    "ForemanAgentRoleFailureRate": {
        "series": [
            'factory_run_outcomes_total{outcome="failed",factory_agent_role="implementer",factory_repo="o/r"}',
            'factory_run_outcomes_total{outcome="completed",factory_agent_role="implementer",factory_repo="o/r"}',
        ],
        "values": "0+1x10",
        "eval": "10m",
    },
    "ForemanDailyModelCost": {
        "series": [
            'factory_run_cost_total{factory_agent_role="implementer",factory_repo="o/r",factory_model="m"}',
        ],
        "values": "0+1x10",
        "eval": "10m",
    },
    "ForemanDeadRunReaped": {
        "series": [
            'factory_runs_reaped_total{factory_agent_role="implementer",factory_repo="o/r"}',
        ],
        "values": "0+1x10",
        "eval": "5m",
    },
}

# What the synthetic series' labels put where a rule templates a label.
SAMPLE_LABELS = {"factory_agent_role": "implementer", "factory_repo": "o/r"}

PLACEHOLDER = re.compile(r"\{\{\s*\$labels\.(\w+)\s*\}\}")


def expand(text):
    """Fill a rule's label placeholders the way Prometheus does for the sample labels."""
    return PLACEHOLDER.sub(lambda m: SAMPLE_LABELS[m.group(1)], text)


def series_labels(series):
    """The labels a synthetic series carries, without __name__."""
    spec = series[series.index("{") + 1 : -1]
    labels = {}
    for pair in spec.split(","):
        key, value = pair.split("=", 1)
        labels[key] = value.strip('"')
    return labels


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
                                "exp_labels": {
                                    **rule.get("labels", {}),
                                    **{
                                        key: value
                                        for key, value in series_labels(sample["series"][0]).items()
                                        if key in SAMPLE_LABELS
                                    },
                                },
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

    missing = set(SAMPLES) - seen
    if missing:
        raise SystemExit(f"alerts without a synthetic event: {sorted(missing)}")

    with open(dest, "w") as out:
        json.dump(
            {"rule_files": ["alerts.yaml"], "evaluation_interval": "1m", "tests": tests},
            out,
            indent=2,
        )
        out.write("\n")


if __name__ == "__main__":
    main()
