// Package security is the shape of a security event: one structured log record for something the
// factory refused or detected. Every event has a stable event name from Event, an outcome, and the host
// it concerned (empty where no destination was involved); the run it belongs to (agent role,
// repository, issue) comes from the logger, scoped once by Scope from the run's own value. An event
// never carries a secret, a token, a request header or a model's file text: callers pass a host and an
// outcome, nothing else.
package security

import "log/slog"

// Message is the log message every security event carries, so one filter finds them all.
const Message = "security event"

// Event is a security event's name. The values are stable: alerts and queries match on them, and a
// caller raises an event from this set rather than naming one itself.
type Event string

// The events. Each is raised through the one call that logs and counts it, so a new event is one
// constant and one call.
const (
	// EventEgressRefused is the event for a request the egress proxy did not allow.
	EventEgressRefused Event = "egress_refused"
	// EventKeyProxyRefused is the event for a request the run's OpenRouter key proxy did not allow.
	EventKeyProxyRefused Event = "key_proxy_refused"
	// EventOutputRejected is the event for a model output file foreman rejected.
	EventOutputRejected Event = "output_rejected"
	// EventDashboardCommandRefused is the event for a retro dashboard command foreman did not carry out.
	EventDashboardCommandRefused Event = "dashboard_command_refused"
	// EventRunCapped is the event for a run that reached its step cap.
	EventRunCapped Event = "run_capped"
)

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

// Run is the run a security event belongs to: the agent role that ran, its repository and its issue.
// It travels as one value, so a caller hands a run's context over once instead of three ways.
type Run struct {
	// Role is the agent role that ran.
	Role string
	// Repository is the repository the run works, as owner/name.
	Repository string
	// Issue is the issue the run works, or zero when no issue was involved.
	Issue int
}

// Scope returns log carrying the run every event it emits belongs to.
func Scope(log *slog.Logger, run Run) *slog.Logger {
	return log.With("agent_role", run.Role, "repository", run.Repository, "issue", run.Issue)
}

// Emit writes one security event.
func Emit(log *slog.Logger, event Event, outcome Outcome, host string) {
	log.Warn(Message, "event", string(event), "outcome", string(outcome), "host", host)
}
