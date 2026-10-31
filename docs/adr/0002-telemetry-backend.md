# 0002. Run Prometheus, Tempo, Loki and Grafana as the telemetry backend

- Status: Accepted
- Date: 2026-10-29

## Context

Every host's collector forwards foreman's traces, metrics and diagnostics to one backend on the dagu
server, a Raspberry Pi with 2GB of memory that also runs dagu. A run's trace, metrics and
diagnostics must be findable from its trace id, the backend must be reachable only on the tailnet,
and every store must have a bound so the Pi's disk cannot fill.

## Decision

Prometheus (OTLP ingest) for metrics, Tempo for traces, Loki for diagnostics and Grafana in front,
all from the nixpkgs revision the factory already pins, each as a systemd unit that binds loopback.
`tailscale serve` is the only thing that exposes them. Grafana links the three by trace id: Tempo
to Loki and Prometheus, Loki's `trace_id` to Tempo, and Prometheus exemplars to Tempo.

Retention: Prometheus 15 days and 2GB, Tempo 7 days, Loki 7 days. Tempo has no size cap, so its
bound is age.

### Memory budget

The stack may use 800 MiB of the server's 2GB, enforced by `MemoryMax` on each unit (and
`GOMEMLIMIT` at 80% of it for the Go services):

| Service | MemoryMax |
| --- | --- |
| Prometheus | 256 MiB |
| Tempo | 192 MiB |
| Loki | 192 MiB |
| Grafana | 160 MiB |

These are limits chosen from the services' typical small-deployment footprints, not measurements on
the Pi. The caps make a service that outgrows its share restart rather than starve dagu.

## Consequences

- If the Pi's memory proves too tight, the lighter substitutes are VictoriaMetrics and
  VictoriaLogs. They are not chosen now because they replace two of the four services but not
  Tempo's role, so they save less than they cost in a second set of query languages.
- Four processes to keep up instead of one, in exchange for stock components and Grafana's native
  trace-to-log-to-metric links.
