package domain

import "context"

// TransactionRepository persists the transaction aggregate. Implementations
// scope every query to the sandbox carried in the context.
type TransactionRepository interface {
	// ByPaymeID loads a transaction by the identifier Payme assigned it.
	// It returns ErrNotFound when no such transaction exists.
	ByPaymeID(ctx context.Context, paymeID string) (*Transaction, error)
	// Create stores a new transaction, assigning its ID.
	Create(ctx context.Context, tx *Transaction) error
	// Update persists state changes of an existing transaction.
	Update(ctx context.Context, tx *Transaction) error
	// Statement lists transactions created within [from, to] inclusive,
	// ordered by creation time ascending.
	Statement(ctx context.Context, from, to int64) ([]*Transaction, error)
	// ActiveByOrder returns the order's current active transaction, if any.
	ActiveByOrder(ctx context.Context, orderID int64) (*Transaction, error)
}

// EventRecorder writes the audit trail the console reads back as a
// transaction's history.
type EventRecorder interface {
	Record(ctx context.Context, e Event) error
}

// Event is one state machine step, including replays that changed nothing.
type Event struct {
	TransactionID int64
	Method        string
	FromState     *State
	ToState       *State
	IdempotentHit bool
	ErrorCode     *int
}
