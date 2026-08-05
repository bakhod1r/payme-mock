package infrastructure

import (
	"context"
	"fmt"

	"github.com/bakhod1r/payme-mock/internal/context/payment/merchant/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
)

// EventRecorder writes the audit trail the console reads back as a
// transaction's history.
type EventRecorder struct {
	pool *postgres.Pool
}

// NewEventRecorder wires the recorder to a pool.
func NewEventRecorder(pool *postgres.Pool) *EventRecorder {
	return &EventRecorder{pool: pool}
}

// Record appends one state machine step.
//
// The row carries no sandbox of its own: it hangs off a transaction that is
// already scoped to one, and the foreign key deletes it with its parent.
func (r *EventRecorder) Record(ctx context.Context, e domain.Event) error {
	_, err := postgres.From(ctx, r.pool).Exec(ctx, `
		INSERT INTO merchant.transaction_events
			(transaction_id, method, from_state, to_state, idempotent_hit, error_code)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		e.TransactionID, e.Method, nullableState(e.FromState), nullableState(e.ToState),
		e.IdempotentHit, e.ErrorCode)
	if err != nil {
		return fmt.Errorf("insert transaction event: %w", err)
	}

	return nil
}

func nullableState(s *domain.State) *int16 {
	if s == nil {
		return nil
	}
	v := int16(*s)
	return &v
}
