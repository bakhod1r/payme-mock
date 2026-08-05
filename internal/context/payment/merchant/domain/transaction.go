// Package domain holds the Merchant API transaction aggregate and the state
// machine the protocol prescribes. It depends on nothing outside the kernel.
package domain

import (
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// State is the transaction state as the protocol defines it.
type State int

// The four states a transaction can occupy.
const (
	StateCreated          State = 1  // created, awaiting confirmation
	StatePerformed        State = 2  // performed, funds credited
	StateCancelled        State = -1 // cancelled before being performed
	StateCancelledAfterDo State = -2 // cancelled after being performed (refund)
)

// Valid reports whether s is one of the four protocol states.
func (s State) Valid() bool {
	switch s {
	case StateCreated, StatePerformed, StateCancelled, StateCancelledAfterDo:
		return true
	default:
		return false
	}
}

// IsFinal reports whether no further transition is possible from s.
func (s State) IsFinal() bool {
	return s == StateCancelled || s == StateCancelledAfterDo
}

// IsActive reports whether the transaction still occupies its order. Only one
// active transaction may exist per order at a time.
func (s State) IsActive() bool {
	return s == StateCreated || s == StatePerformed
}

// Reason is the documented cancellation reason. The public documentation does
// not publish the full table, so values are carried through rather than
// interpreted: the mock sends them and the merchant stores and returns them.
type Reason int

// ReasonTimeout is the reason recorded when a transaction is auto-cancelled
// for exceeding the confirmation window.
const ReasonTimeout Reason = 4

// Transaction is the aggregate root. Times are Payme protocol timestamps:
// milliseconds since the Unix epoch, zero meaning "not yet".
type Transaction struct {
	ID          int64
	SandboxID   int64
	PaymeID     string
	OrderID     int64
	AccountID   int64
	Account     map[string]string
	Amount      int64
	State       State
	Reason      *Reason
	PaymeTime   int64
	CreateTime  int64
	PerformTime int64
	CancelTime  int64
	Receivers   []Receiver
}

// Receiver is an optional payment split target.
type Receiver struct {
	ID     string `json:"id"`
	Amount int64  `json:"amount"`
}

// NewTransaction creates a transaction in the created state.
func NewTransaction(sandboxID int64, paymeID string, orderID, accountID int64,
	account map[string]string, amount, paymeTime, now int64,
) *Transaction {
	return &Transaction{
		SandboxID:  sandboxID,
		PaymeID:    paymeID,
		OrderID:    orderID,
		AccountID:  accountID,
		Account:    account,
		Amount:     amount,
		State:      StateCreated,
		PaymeTime:  paymeTime,
		CreateTime: now,
	}
}

// Expired reports whether an unconfirmed transaction has outlived the
// confirmation window. Performed and cancelled transactions never expire.
func (t *Transaction) Expired(now, timeoutMillis int64) bool {
	if t.State != StateCreated {
		return false
	}
	return now-t.CreateTime > timeoutMillis
}

// Perform moves a created transaction to performed.
//
// A transaction that is already performed is not an error: the protocol
// requires a repeated PerformTransaction to return the original response, so
// the caller can reply with the stored result.
func (t *Transaction) Perform(now, timeoutMillis int64) error {
	switch t.State {
	case StatePerformed:
		return nil // idempotent replay
	case StateCreated:
		if t.Expired(now, timeoutMillis) {
			t.cancel(ReasonTimeout, now)
			return payerr.ErrCannotPerform
		}
		t.State = StatePerformed
		t.PerformTime = now
		return nil
	default:
		return payerr.ErrCannotPerform
	}
}

// Cancel moves a transaction to the cancelled state matching its current one:
// a created transaction becomes -1, a performed transaction becomes -2.
//
// Repeating a cancellation is not an error, for the same idempotency reason as
// Perform: the stored response must be returned unchanged.
func (t *Transaction) Cancel(reason Reason, now int64) error {
	switch t.State {
	case StateCancelled, StateCancelledAfterDo:
		return nil // idempotent replay
	case StateCreated, StatePerformed:
		t.cancel(reason, now)
		return nil
	default:
		return payerr.ErrCannotPerform
	}
}

// cancel applies the cancellation without checking whether it is allowed.
func (t *Transaction) cancel(reason Reason, now int64) {
	if t.State == StatePerformed {
		t.State = StateCancelledAfterDo
	} else {
		t.State = StateCancelled
	}
	t.Reason = &reason
	t.CancelTime = now
}
