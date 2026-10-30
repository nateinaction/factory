# 0001. Run foreman's egress proxy in front of srt

- Status: Accepted
- Date: 2026-10-29

## Context

Every agent role run executes under srt (`@anthropic-ai/sandbox-runtime`), which owns the
filesystem boundary and blocks direct network egress. srt ships its own HTTP proxy and offers
`network.allowedDomains` to decide what a run may reach.

The factory needs two things srt's proxy cannot provide together:

- **Open egress by default.** An agent role's `factory_egress` allowlist is empty unless a human
  sets one, and an empty list means the run may reach any public host. srt 0.0.81 rejects a bare
  `*` in `allowedDomains`, and rejects broad entries such as `*.com`; only `deniedDomains` accepts
  `*`. srt's proxy therefore cannot express the default policy.
- **A domain log for the usage line, and a refuse-by-destination boundary.** The factory records
  every host a run contacts and must keep a run away from hosts that are not publicly routable.

srt's proxy can be skipped for HTTP by naming an external proxy: when `network.httpProxyPort` is set,
srt still blocks direct egress and routes HTTP(S) through that port.

## Decision

foreman runs its own loopback egress proxy (`foreman/internal/egress`) and points srt's
`network.httpProxyPort` at it. srt remains the sandbox: it blocks direct traffic, keeps the
filesystem boundary, and bridges only the configured proxy port into the run's network namespace.
foreman's proxy is the policy and logging point for everything the run reaches:

- It enforces the agent role's allowlist. An empty allowlist is open egress; a non-empty one permits
  exactly those hosts and their matching wildcard subdomains.
- It refuses a destination whose address is not publicly routable, whatever the allowlist says:
  loopback, RFC 1918 private space, RFC 6598 shared space (`100.64.0.0/10`, which is where a tailnet
  lives), link-local addresses including `169.254.169.254`, `0.0.0.0/8`, multicast and unspecified
  addresses. IPv4-mapped IPv6 addresses are checked as the IPv4 address they hold.
- It resolves each destination once, refuses a name when any address it resolves to is refused, and
  dials the resolved address. A second DNS answer cannot point the connection somewhere the check
  never saw.
- The check applies to both the `CONNECT` path and the plain-HTTP forward path, and it is
  independent of the allowlist: an open role is still refused a private destination.
- The same port serves the model's `/api/v1` path, forwarding it to the run's loopback OpenRouter
  key proxy. The key proxy is foreman's own listener rather than a model-chosen destination, so its
  transport dials without the public-address check. One port matters on Linux, where srt only
  bridges its configured proxy ports into the namespace.
- It records every host it is asked for, allowed and refused apart, for the usage line: `domains`
  lists the allowed hosts, `refused_domains` the refused ones. A host is recorded as allowed only
  after a connection to it was made, so a destination the run never reached is not listed, and a
  refusal never erases an earlier success. The refused list is bounded (`MaxRefusedHosts`), so a
  model that asks for endless names cannot grow the run's record without limit.

srt's own proxy still runs its SOCKS side with the empty allowlist srt requires, so non-HTTP TCP
egress is denied; opencode's traffic is HTTP(S).

### Amendment: how the host record is kept

The decision first said the proxy "records every host it is asked for, allowed or refused" in one
record. The usage line needs the two apart, and an unbounded refused list is a record a
prompt-injected run could grow, so the decision above is amended as implemented: `domains` and
`refused_domains` are separate, an allowed host is recorded only after a connection to it is made,
and the refused list is capped. A refusal is counted, measured and logged once, through the run's
one refusal tally, shared by both proxies; the log records are rate limited while the count is not.

## Consequences

- Open egress works without weakening srt: srt is still the only network path, and foreman's proxy
  is the only thing it may reach.
- The factory's policy and logging live in code it can test, instead of in a vendored proxy's
  configuration schema.
- A sandboxed model cannot reach dagu on the tailnet, another run's loopback proxy, a private
  service or a cloud metadata endpoint, even when its role's allowlist is open. This closes the
  path from a model to dagu, and through it to every App key.
- The proxy's dialect is HTTP proxy only. Non-HTTP protocols are already denied by srt; a future
  need for them would have to be met at srt's SOCKS proxy, which is out of the factory's control.
- The loopback key proxy must stay on loopback. Were it ever bound elsewhere, the API transport
  would still reach it unguarded by design, so that address must remain foreman's own.
- Two proxies run per agent role run (egress and key), both stopped when the run ends; a proxy that
  fails to bind fails the run, and there is no unsandboxed fallback.
- A refusal is one record: both proxies raise theirs through the run's one refusal tally, which
  counts it, measures it for the security alert and logs it. The count is exact; the log records are
  rate limited per outcome, so a model that floods refusals cannot fill the log.

## Alternatives considered

- **Configure srt's `allowedDomains` directly.** Rejected: srt 0.0.81 cannot express open egress, so
  the default policy would have to be narrowed to a fixed list of domains.
- **Add the destination check to srt.** Rejected: srt is a pinned third-party package, and the
  factory would depend on an unreleased change.
- **Check only the policy and dial the name as given.** Rejected: DNS rebinding would let a name
  answer publicly at the check and privately at the dial.
