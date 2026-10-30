#!/usr/bin/env python3
"""Run the collector role's rendered config through the real collector and check what leaves it.

The rendered config is a host's collector: it receives OTLP on loopback and forwards to the telemetry
backend. This test renders it with a second collector as its backend, so the second collector's debug
exporter shows exactly what the first let through: a non-allowlisted attribute is dropped from spans,
metric data points and log records, host health's generic labels are renamed, foreman's dagu stderr
records are picked up with their trace ids, and no value of a dropped attribute survives.

Run by tests/collector.test; requires the pinned otelcol-contrib on PATH (the flake's dev shell).
"""

import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request

TRACE_ID = "4bf92f3577b34da6a3ce929d0e0e4736"
SPAN_ID = "00f067aa0ba902b7"

# The security events a run's stderr carries. Every field of each must reach the sink, so one query
# on the record's body returns all of them with the run and host they name.
SECURITY_EVENTS = [
    {
        "event": "output_rejected",
        "outcome": "factory_marker",
        "host": "blocked.example",
        "agent_role": "implementer",
        "repository": "o/r",
        "issue": 5,
    },
    {
        "event": "egress_refused",
        "outcome": "refused_not_routable",
        "host": "metadata.example",
        "agent_role": "implementer",
        "repository": "o/r",
        "issue": 5,
    },
]


def free_port():
    """Return a port nothing is listening on."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_for_port(port, timeout=20):
    """Wait until something accepts a connection on the port."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.5):
                return
        except OSError:
            time.sleep(0.1)

    raise SystemExit(f"collector did not listen on 127.0.0.1:{port}")


def post(url, payload):
    """POST one OTLP/JSON document."""
    body = json.dumps(payload).encode()
    req = urllib.request.Request(
        url, data=body, headers={"Content-Type": "application/json"}, method="POST"
    )
    with urllib.request.urlopen(req, timeout=10) as resp:
        if resp.status not in (200, 202):
            raise SystemExit(f"POST {url}: {resp.status}")


def attr(key, value):
    """One OTLP/JSON attribute, typed from the Python value."""
    if isinstance(value, bool):
        return {"key": key, "value": {"boolValue": value}}
    if isinstance(value, int):
        return {"key": key, "value": {"intValue": str(value)}}
    if isinstance(value, float):
        return {"key": key, "value": {"doubleValue": value}}
    return {"key": key, "value": {"stringValue": value}}


def span_payload(now):
    """A run span with an allowed attribute, an allowed issue, and one that must be dropped."""
    return {
        "resourceSpans": [
            {
                "resource": {"attributes": [attr("service.name", "foreman")]},
                "scopeSpans": [
                    {
                        "spans": [
                            {
                                "traceId": TRACE_ID,
                                "spanId": SPAN_ID,
                                "name": "foreman.run",
                                "kind": 1,
                                "startTimeUnixNano": str(now),
                                "endTimeUnixNano": str(now + 1_000_000),
                                "attributes": [
                                    attr("factory.repo", "o/r"),
                                    attr("factory.issue", 5),
                                    attr("factory.run_id", "run-old"),
                                    # What a poll and a process report about themselves.
                                    attr("factory.skip_reason", "capacity_full"),
                                    attr("factory.attempt", 2),
                                    attr("factory.verb", "start"),
                                    attr("factory.exit_code", 1),
                                    # Never allowlisted: neither the key nor its value may leave.
                                    attr("not.allowed.attr", "secret-span-value"),
                                ],
                            }
                        ]
                    }
                ],
            }
        ]
    }


def metric_payloads(now):
    """A poll metric with an allowed state and a host-health metric with generic labels."""
    poll = {
        "resourceMetrics": [
            {
                "scopeMetrics": [
                    {
                        "metrics": [
                            {
                                "name": "factory.poll.issues",
                                "gauge": {
                                    "dataPoints": [
                                        {
                                            "asInt": "3",
                                            "startTimeUnixNano": str(now),
                                            "timeUnixNano": str(now),
                                            "attributes": [
                                                attr("factory.repo", "o/r"),
                                                attr("factory.state", "factory:review"),
                                                attr("not.allowed.metric", "secret-metric-value"),
                                            ],
                                        }
                                    ],
                                },
                            }
                        ]
                    }
                ]
            }
        ]
    }

    health = {
        "resourceMetrics": [
            {
                "scopeMetrics": [
                    {
                        "metrics": [
                            {
                                "name": "factory_host_throttled",
                                "gauge": {
                                    "dataPoints": [
                                        {
                                            "asInt": "1",
                                            "timeUnixNano": str(now),
                                            "attributes": [
                                                attr("kind", "throttled"),
                                                attr("state", "now"),
                                            ],
                                        }
                                    ]
                                },
                            }
                        ]
                    }
                ]
            }
        ]
    }

    return poll, health


def log_payload(now):
    """One diagnostic record, with a trace and a value that must be dropped."""
    return {
        "resourceLogs": [
            {
                "scopeLogs": [
                    {
                        "logRecords": [
                            {
                                "timeUnixNano": str(now),
                                "body": {"stringValue": "hello diagnostic"},
                                "attributes": [
                                    attr("factory.repo", "o/r"),
                                    attr("not.allowed.log", "secret-log-value"),
                                ],
                                "traceId": TRACE_ID,
                                "spanId": SPAN_ID,
                            }
                        ]
                    }
                ]
            }
        ]
    }


def main():
    root = sys.argv[1]
    # Real path, because the collector compares a file's resolved path with the log directory: a
    # temporary tree reached through a symlink (/tmp on macOS is /private/tmp) would differ.
    work = os.path.realpath(tempfile.mkdtemp(prefix="collector-otlp-"))
    logs_dir = os.path.join(work, "dagu-logs")
    state_dir = os.path.join(work, "state")
    render_dir = os.path.join(work, "render")
    os.makedirs(render_dir)
    os.makedirs(logs_dir)

    front_grpc, front_http = free_port(), free_port()
    sink_grpc, sink_http = free_port(), free_port()

    env = dict(os.environ)
    env.update(
        {
            "RENDER_DIR": render_dir,
            "ANSIBLE_CONFIG": os.path.join(root, "ansible.cfg"),
            "COLLECTOR_BACKEND": "127.0.0.1",
            "COLLECTOR_GRPC_PORT": str(front_grpc),
            "COLLECTOR_HTTP_PORT": str(front_http),
            "COLLECTOR_TEMPO_PORT": str(sink_grpc),
            "COLLECTOR_PROM_PORT": str(sink_http),
            "COLLECTOR_LOKI_PORT": str(sink_http),
            "COLLECTOR_STATE_DIR": state_dir,
            "COLLECTOR_LOGS_DIR": logs_dir,
        }
    )

    rendered = subprocess.run(
        ["ansible-playbook", os.path.join(root, "tests/collector.yml")],
        env=env,
        capture_output=True,
        text=True,
    )
    if rendered.returncode != 0:
        sys.stderr.write(rendered.stdout + rendered.stderr)
        return 1

    # The sink accepts what the role's exporters send: OTLP/gRPC for traces, and the Prometheus and
    # Loki OTLP HTTP paths for metrics and diagnostics.
    sink_config = os.path.join(work, "sink.yaml")
    with open(sink_config, "w") as f:
        f.write(
            f"""receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 127.0.0.1:{sink_grpc}
      http:
        endpoint: 127.0.0.1:{sink_http}
        metrics_url_path: /api/v1/otlp/v1/metrics
        logs_url_path: /otlp/v1/logs
exporters:
  debug:
    verbosity: detailed
service:
  telemetry:
    metrics:
      level: none
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [debug]
    metrics:
      receivers: [otlp]
      exporters: [debug]
    logs:
      receivers: [otlp]
      exporters: [debug]
"""
        )

    # The run's stderr file exists before the collector starts (a dagu run creates it as it runs), so
    # the receiver's start_at: end picks up what is appended afterwards.
    # dagu keeps a step's stderr at <dag>/<run id>/<attempt id>/<step>.stderr.log.
    run_dir = os.path.join(logs_dir, "factory-implementer", "run-old", "a1b2c3")
    os.makedirs(run_dir)
    run_err = os.path.join(run_dir, "implementer.stderr.log")
    open(run_err, "w").close()

    # A root-only file the dagu user must not be able to publish to Loki, reached the ways a symlink
    # in the log directory can: the link is the log file, or it is a whole run directory. The
    # collector's CAP_DAC_READ_SEARCH can read the target, so the receiver has to drop it.
    root_only = os.path.join(work, "root-only")
    with open(root_only, "w") as f:
        f.write(json.dumps({"time": "2026-01-01T00:00:00.123456789Z", "level": "INFO",
                            "msg": "root-only secret"}) + "\n")
    os.chmod(root_only, 0o600)
    os.symlink(root_only, os.path.join(run_dir, "leak.stderr.log"))

    outside_run = os.path.join(work, "outside-run")
    os.makedirs(os.path.join(outside_run, "a1b2c3"))
    outside_err = os.path.join(outside_run, "a1b2c3", "sneak.stderr.log")
    open(outside_err, "w").close()
    os.makedirs(os.path.join(logs_dir, "factory-leak"))
    os.symlink(outside_run, os.path.join(logs_dir, "factory-leak", "run-sneak"))

    # A directory whose name starts with the log directory's name is still outside it.
    sibling_run = os.path.join(work, "dagu-logs-outside")
    os.makedirs(os.path.join(sibling_run, "a1b2c3"))
    sibling_err = os.path.join(sibling_run, "a1b2c3", "sneak.stderr.log")
    open(sibling_err, "w").close()
    os.makedirs(os.path.join(logs_dir, "factory-sibling"))
    os.symlink(sibling_run, os.path.join(logs_dir, "factory-sibling", "run-sibling"))

    sink_log = open(os.path.join(work, "sink.log"), "w")
    front_log = open(os.path.join(work, "front.log"), "w")

    sink = subprocess.Popen(
        ["otelcol-contrib", "--config", sink_config], stdout=sink_log, stderr=subprocess.STDOUT
    )
    front = subprocess.Popen(
        ["otelcol-contrib", "--config", os.path.join(render_dir, "config.yaml")],
        stdout=front_log,
        stderr=subprocess.STDOUT,
    )

    try:
        wait_for_port(sink_http)
        wait_for_port(front_http)

        now = time.time_ns()
        post(f"http://127.0.0.1:{front_http}/v1/traces", span_payload(now))
        poll, health = metric_payloads(now)
        post(f"http://127.0.0.1:{front_http}/v1/metrics", poll)
        post(f"http://127.0.0.1:{front_http}/v1/metrics", health)
        post(f"http://127.0.0.1:{front_http}/v1/logs", log_payload(now))

        # A run's stderr as dagu writes it, appended after the receiver started (start_at: end).
        time.sleep(1)
        with open(run_err, "a") as f:
            for event in SECURITY_EVENTS:
                f.write(
                    json.dumps(
                        {
                            "time": "2026-01-01T00:00:00.123456789Z",
                            "level": "WARN",
                            "msg": "security event",
                            **event,
                            "trace_id": TRACE_ID,
                            "span_id": SPAN_ID,
                            "not.allowed.filelog": "secret-filelog-value",
                        }
                    )
                    + "\n"
                )
            # A line that is not a JSON record, as a tool writing to fd 2 could leave behind: it must
            # be dropped rather than shipped as raw text to Loki.
            f.write("private raw tool output\n")

        # The symlink targets, written after the receiver started. A record that came back through a
        # symlink must never reach Loki, even though the collector can read the target.
        for path, message in ((root_only, "root-only secret"),
                              (outside_err, "symlinked directory secret"),
                              (sibling_err, "similarly named directory secret")):
            with open(path, "a") as f:
                f.write(json.dumps({"time": "2026-01-01T00:00:00.223456789Z", "level": "INFO",
                                    "msg": message}) + "\n")

        # The role's batch timeout is 5s, so give the exports well over one batch interval.
        wanted = [
            "factory.repo",
            "factory.issue",
            "factory.state",
            "factory.host_health.kind",
            "factory.host_health.state",
            "factory.skip_reason",
            "factory.attempt",
            "factory.verb",
            "factory.exit_code",
            "hello diagnostic",
            "security event",
            "egress_refused",
        ]
        deadline = time.time() + 40
        while time.time() < deadline:
            with open(os.path.join(work, "sink.log")) as f:
                out = f.read()
            if all(marker in out for marker in wanted):
                break
            time.sleep(0.5)
        else:
            with open(os.path.join(work, "sink.log")) as f:
                out = f.read()
            with open(os.path.join(work, "front.log")) as f:
                sys.stderr.write("front collector:\n" + f.read())
            sys.stderr.write("sink collector:\n" + out)
            return 1
    finally:
        for proc in (front, sink):
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
        sink_log.close()
        front_log.close()

    with open(os.path.join(work, "sink.log")) as f:
        out = f.read()

    dropped = [
        "not.allowed.attr",
        "secret-span-value",
        "not.allowed.metric",
        "secret-metric-value",
        "not.allowed.log",
        "secret-log-value",
        "not.allowed.filelog",
        "secret-filelog-value",
        "-> kind:",
        "-> state:",
    ]
    leaked = [marker for marker in dropped if marker in out]
    if leaked:
        sys.stderr.write("the collector let a non-allowlisted attribute through: " + repr(leaked) + "\n")
        sys.stderr.write(out)
        return 1

    if "private raw tool output" in out:
        sys.stderr.write("the collector shipped a line it could not parse as JSON\n")
        sys.stderr.write(out)
        return 1

    smuggled = [m for m in ("root-only secret", "symlinked directory secret",
                            "similarly named directory secret") if m in out]
    if smuggled:
        sys.stderr.write("the collector shipped a file reached through a symlink: " + repr(smuggled) + "\n")
        sys.stderr.write(out)
        return 1

    # A poll's skip reason and a process's verb, exit code and attempt are what #126 and #122 read,
    # so each must survive the allowlist on the span that carries it.
    run_attributes = [
        "factory.skip_reason: Str(capacity_full)",
        "factory.attempt: Int(2)",
        "factory.verb: Str(start)",
        "factory.exit_code: Int(1)",
    ]
    lost = [marker for marker in run_attributes if marker not in out]
    if lost:
        sys.stderr.write("the collector dropped a run attribute: " + repr(lost) + "\n")
        sys.stderr.write(out)
        return 1

    # Every security event ships with the whole record. They share one body, so one query returns all
    # of them; the record itself names the event, outcome, host and run it belongs to.
    records = out.split("Body: Str(security event)")[1:]
    shipped = {
        event["event"]: record
        for record in records
        for event in SECURITY_EVENTS
        if "event: Str(" + event["event"] + ")" in record
    }
    if set(shipped) != {event["event"] for event in SECURITY_EVENTS}:
        sys.stderr.write("not every security event reached the sink: " + repr(sorted(shipped)) + "\n")
        sys.stderr.write(out)
        return 1

    for event in SECURITY_EVENTS:
        fields = [
            "outcome: Str(" + event["outcome"] + ")",
            "host: Str(" + event["host"] + ")",
            "agent_role: Str(" + event["agent_role"] + ")",
            "repository: Str(" + event["repository"] + ")",
            "issue: Double(" + str(event["issue"]) + ")",
        ]
        missing = [field for field in fields if field not in shipped[event["event"]]]
        if missing:
            sys.stderr.write(
                "the collector dropped a security event field: " + repr(missing) + "\n"
            )
            sys.stderr.write(out)
            return 1

    # The filelog record's trace id survives as the record's trace context and as an attribute Loki's
    # derived field matches, so a trace can be followed to its diagnostics.
    record = out.find("Body: Str(security event)")
    if record < 0 or TRACE_ID not in out[record : record + 600]:
        sys.stderr.write("the diagnostics lost their trace id\n")
        sys.stderr.write(out)
        return 1

    if "trace_id: Str(" + TRACE_ID not in out:
        sys.stderr.write("the diagnostics lost the trace_id attribute Loki matches\n")
        sys.stderr.write(out)
        return 1

    shutil.rmtree(work, ignore_errors=True)

    return 0


if __name__ == "__main__":
    sys.exit(main())
