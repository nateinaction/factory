// Package security is the shape of a security event: one structured log record for something the
// factory refused or detected. Every event has a stable event name, an outcome, and the host it
// concerned (empty where no destination was involved); the run it belongs to (agent role, repository,
// issue) comes from the logger, scoped once by Scope. An event never carries a secret, a token, a
// request header or a model's file text: callers pass a host and an outcome, nothing else.
package security

import "log/slog"

// Message is the log message every security event carries, so one filter finds them all.
const Message = "security event"

// EventEgressRefused is the event for a request the egress proxy did not allow.
const EventEgressRefused = "egress_refused"

// EventKeyProxyRefused is the event for a request the run's OpenRouter key proxy did not allow.
const EventKeyProxyRefused = "key_proxy_refused"

// EventOutputRejected is the event for a model output file foreman rejected.
const EventOutputRejected = "output_rejected"

// EventDashboardCommandRefused is the event for a retro dashboard command foreman did not carry out.
const EventDashboardCommandRefused = "dashboard_command_refused"

// EventRunCapped is the event for a run that reached its step cap.
const EventRunCapped = "run_capped"

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

// The dashboard command outcomes.
const (
	// CommandByNonWriter is a command from a login that may not write to the repository.
	CommandByNonWriter Outcome = "unauthorized_author"
	// CommandCheckUnreadable is a command settled with no action because the permission check failed
	// closed: a permission it could not read is never write access.
	CommandCheckUnreadable Outcome = "permission_unreadable"
)

// The run outcomes.
const (
	// RunStepCap is a run that reached its step cap, which fails it and is never retried.
	RunStepCap Outcome = "step_cap"
)

// A rejected output's outcome is workflow.OutputProblem's own value, so the two packages share one
// set of values and a rejection needs no bridge that could fall through.

// Scope returns log carrying the run every event it emits belongs to.
func Scope(log *slog.Logger, agentRole, repository string, issue int) *slog.Logger {
	return log.With("agent_role", agentRole, "repository", repository, "issue", issue)
}

// Emit writes one security event.
func Emit(log *slog.Logger, event string, outcome Outcome, host string) {
	log.Warn(Message, "event", event, "outcome", string(outcome), "host", host)
}
