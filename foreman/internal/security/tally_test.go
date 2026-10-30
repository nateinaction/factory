package security

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Tally", func() {
	It("counts every refusal by outcome and hands out a copy", func() {
		tally := NewTally(nil)
		log := slog.New(slog.DiscardHandler)

		tally.Refuse(context.Background(), log, EventEgressRefused, RefusedByPolicy, "blocked.example")
		tally.Refuse(context.Background(), log, EventEgressRefused, RefusedByPolicy, "other.example")
		tally.Refuse(context.Background(), log, EventEgressRefused, RefusedNotRoutable, "10.1.2.3")

		counts := tally.Counts()
		Expect(counts).To(Equal(map[Outcome]int{RefusedByPolicy: 2, RefusedNotRoutable: 1}))

		delete(counts, RefusedByPolicy)
		Expect(tally.Counts()).To(Equal(map[Outcome]int{RefusedByPolicy: 2, RefusedNotRoutable: 1}),
			"Counts handed out the tally's own map")
	})

	It("rate limits each outcome's events and still counts every refusal", func() {
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

		var logged bytes.Buffer

		log := slog.New(slog.NewJSONHandler(&logged, nil))
		tally := NewTally(nil)
		tally.now = func() time.Time { return now }

		events := func() int { return strings.Count(logged.String(), Message) }

		refuse := func(outcome Outcome) {
			tally.Refuse(context.Background(), log, EventEgressRefused, outcome, "blocked.example")
		}

		for range 100 {
			refuse(RefusedByPolicy)
		}

		Expect(events()).To(Equal(RefusalLogBurst), "a burst past the limit still reached the log")
		Expect(tally.Counts()[RefusedByPolicy]).To(Equal(100), "the limit dropped a refusal from the tally")

		now = now.Add(RefusalLogRefill)

		refuse(RefusedByPolicy)
		Expect(events()).To(Equal(RefusalLogBurst+1), "a refusal after a refill interval did not log")

		refuse(RefusedByPolicy)
		Expect(events()).To(Equal(RefusalLogBurst+1), "a refusal within the interval logged again")

		now = now.Add(RefusalLogBurst * RefusalLogRefill)

		for range RefusalLogBurst * 2 {
			refuse(RefusedByPolicy)
		}

		Expect(events()).To(Equal(RefusalLogBurst*2+1), "the refill let more through than one burst")

		// One outcome's flood leaves another outcome's first event alone.
		refuse(RefusedTunnelLimit)
		Expect(events()).To(Equal(RefusalLogBurst*2 + 2))
	})

	It("counts every refusal in the run's metric, even the ones its log limit drops", func() {
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

		var logged bytes.Buffer

		type measurement struct {
			event   Event
			outcome Outcome
		}

		var measured []measurement

		tally := NewTally(func(_ context.Context, event Event, outcome Outcome) {
			measured = append(measured, measurement{event: event, outcome: outcome})
		})
		tally.now = func() time.Time { return now }

		log := slog.New(slog.NewJSONHandler(&logged, nil))

		for range RefusalLogBurst * 3 {
			tally.Refuse(context.Background(), log, EventEgressRefused, RefusedByPolicy, "blocked.example")
		}

		Expect(measured).To(HaveLen(RefusalLogBurst*3), "the log limit dropped a count from the metric")
		Expect(measured[0]).To(Equal(measurement{event: EventEgressRefused, outcome: RefusedByPolicy}))
		Expect(strings.Count(logged.String(), Message)).To(Equal(RefusalLogBurst),
			"the metric path logged past the limit")
	})
})
