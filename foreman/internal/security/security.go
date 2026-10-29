// Package security is the shape of a security event: one structured log record for something the
// factory refused or detected. Every event has a stable event name, an outcome, and the host it
// concerned; the run it belongs to (agent role, repository, issue) comes from the logger, scoped once
// by Scope. An event never carries a secret, a token or a request header: callers pass a host and
// an outcome, nothing else.
package security

import "log/slog"

// Message is the log message every security event carries, so one filter finds them all.
const Message = "security event"

// EventEgressRefused is the event for a request the egress proxy did not allow.
const EventEgressRefused = "egress_refused"

// Outcome is why the request was refused. The values are stable: alerts match on them.
type Outcome string

// The egress outcomes.
const (
	// RefusedByPolicy is a destination the agent role's allowlist leaves out, or a path the proxy
	// does not serve.
	RefusedByPolicy Outcome = "refused_by_policy"
	// RefusedNotRoutable is a destination whose address is not publicly routable.
	RefusedNotRoutable Outcome = "refused_not_routable"
	// RefusedTunnelLimit is a tunnel refused because the run already has the most it may open.
	RefusedTunnelLimit Outcome = "refused_tunnel_limit"
	// RefusedBadToken is a request with a missing or wrong run token.
	RefusedBadToken Outcome = "refused_bad_token"
)

// Scope returns log carrying the run every event it emits belongs to.
func Scope(log *slog.Logger, agentRole, repository string, issue int) *slog.Logger {
	return log.With("agent_role", agentRole, "repository", repository, "issue", issue)
}

// Emit writes one security event.
func Emit(log *slog.Logger, event string, outcome Outcome, host string) {
	log.Warn(Message, "event", event, "outcome", string(outcome), "host", host)
}
