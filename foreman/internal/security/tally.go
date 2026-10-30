package security

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"
)

// A tally's log limit: one outcome may emit RefusalLogBurst events back to back, then one more for
// every RefusalLogRefill that passes. A model that floods refusals cannot fill the log, while the count
// itself still records every request.
const (
	// RefusalLogBurst is how many events one outcome may emit at once.
	RefusalLogBurst = 5
	// RefusalLogRefill is how long one spent event's place takes to return.
	RefusalLogRefill = time.Second
)

// CounterFunc records one refusal in the run's security metric. The tally calls it for every refusal,
// whatever the log limit drops, so the metric stays exact.
type CounterFunc func(ctx context.Context, event Event, outcome Outcome)

// Tally counts refusals by outcome, emits their security events and records each one in the run's
// metric. Both proxies keep one, so a refusal is counted and reported one way however it arrived.
type Tally struct {
	mu       sync.Mutex
	counts   map[Outcome]int
	budget   map[Outcome]int
	refilled map[Outcome]time.Time
	now      func() time.Time
	// counter records a refusal in the run's metric; nil records no metric, as a spec that ignores
	// telemetry does.
	counter CounterFunc
}

// NewTally returns an empty tally. counter records each refusal in the run's metric; nil counts
// without measuring.
func NewTally(counter CounterFunc) *Tally {
	return &Tally{
		counts: map[Outcome]int{}, budget: map[Outcome]int{}, refilled: map[Outcome]time.Time{},
		now: time.Now, counter: counter,
	}
}

// TallyOr returns t, or a tally that counts refusals and records no metric, when t is nil.
func TallyOr(t *Tally) *Tally {
	if t != nil {
		return t
	}

	return NewTally(nil)
}

// Refuse counts one refused request, records it in the run's metric and emits its security event, at
// most within the tally's log limit. Counting and measuring never drop a refusal; only the log record
// is limited. The host is the event's whole payload: no header or token can reach it.
func (t *Tally) Refuse(
	ctx context.Context, log *slog.Logger, event Event, outcome Outcome, host string,
) {
	t.mu.Lock()
	t.counts[outcome]++
	emit := t.take(outcome)
	t.mu.Unlock()

	if t.counter != nil {
		t.counter(ctx, event, outcome)
	}

	if emit {
		Emit(log, event, outcome, host)
	}
}

// take spends one of outcome's event slots, reporting whether it had one.
func (t *Tally) take(outcome Outcome) bool {
	now := t.now()

	budget, seen := t.budget[outcome]
	if !seen {
		t.budget[outcome] = RefusalLogBurst - 1
		t.refilled[outcome] = now

		return true
	}

	if refills := int(now.Sub(t.refilled[outcome]) / RefusalLogRefill); refills > 0 {
		budget = min(budget+refills, RefusalLogBurst)
		t.refilled[outcome] = t.refilled[outcome].Add(time.Duration(refills) * RefusalLogRefill)
	}

	if budget <= 0 {
		t.budget[outcome] = 0

		return false
	}

	t.budget[outcome] = budget - 1

	return true
}

// Counts is what the tally counted so far, by outcome.
func (t *Tally) Counts() map[Outcome]int {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make(map[Outcome]int, len(t.counts))
	maps.Copy(out, t.counts)

	return out
}
