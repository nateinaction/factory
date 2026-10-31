package foreman

import (
	"context"
	"errors"
	"fmt"

	"github.com/nateinaction/factory/foreman/internal/dagu"
	"github.com/nateinaction/factory/foreman/internal/proc"
)

// DrainCheck asks dagu for the latest run of every named DAG and returns 0 only when each has
// finished. It fails closed: a dagu error, an active run and a status foreman does not recognise
// all block with exit 1, and every DAG is checked so the log names each one that blocks. Active
// is the run model's own definition (dagu.Classify), shared with the poll.
func DrainCheck(ctx context.Context, d Deps, dags []string) (int, error) {
	blocked := 0

	for _, dag := range dags {
		out, err := d.run(ctx, proc.Cmd{
			Argv: []string{d.Settings.Dagu, "status", dag}, Capture: true, Timeout: DaguTimeout,
		})
		if err != nil {
			d.log().ErrorContext(ctx, "drain-check: dagu could not report on the DAG", "dag", dag, "error", err)

			blocked++

			continue
		}

		switch dagu.Classify(out) {
		case dagu.PhaseActive:
			d.log().ErrorContext(ctx, "drain-check: the DAG has an active run", "dag", dag)

			blocked++
		case dagu.PhaseUnknown:
			d.log().ErrorContext(ctx, "drain-check: unrecognised status", "dag", dag, "status", out)

			blocked++
		case dagu.PhaseFinished:
			// Drained: nothing to block on.
		}
	}

	if blocked > 0 {
		return 1, errors.New(blockedMessage(blocked, len(dags)))
	}

	return 0, nil
}

// blockedMessage is the one failure DrainCheck returns.
func blockedMessage(blocked, total int) string {
	return fmt.Sprintf("drain-check: %d of %d DAGs are not drained", blocked, total)
}
