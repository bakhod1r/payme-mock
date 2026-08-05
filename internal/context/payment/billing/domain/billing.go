// Package domain models the merchant's own billing: the accounts a payer is
// identified by and the orders a payment settles.
package domain

import (
	"context"

	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// OrderStatus tracks an order through payment.
type OrderStatus string

// The order lifecycle.
const (
	StatusNew        OrderStatus = "new"
	StatusProcessing OrderStatus = "processing"
	StatusPaid       OrderStatus = "paid"
	StatusCancelled  OrderStatus = "cancelled"
)

// Account is a payer known to the merchant, identified by the fields Payme
// sends in the `account` object.
type Account struct {
	ID        int64
	SandboxID int64
	Phone     string
	Login     string
	Name      string
	Balance   int64
	Blocked   bool
}

// Order is what a payment settles.
type Order struct {
	ID          int64
	SandboxID   int64
	AccountID   int64
	Amount      int64
	Status      OrderStatus
	Description string
}

// CheckAmount verifies that the amount Payme offers matches what the order
// expects. The protocol reports any mismatch as -31001.
func (o *Order) CheckAmount(amount int64) error {
	if amount != o.Amount {
		return payerr.ErrInvalidAmount
	}
	return nil
}

// Payable reports whether the order can still accept a payment.
func (o *Order) Payable() bool {
	return o.Status == StatusNew || o.Status == StatusProcessing
}

// MarkPaid records a successful payment.
func (o *Order) MarkPaid() { o.Status = StatusPaid }

// MarkProcessing records that a transaction has been created against the order.
func (o *Order) MarkProcessing() { o.Status = StatusProcessing }

// MarkCancelled returns the order to an unpaid state after a cancellation.
func (o *Order) MarkCancelled() { o.Status = StatusCancelled }

// AccountRepository resolves the `account` object into a known payer and
// stores what a performed payment did to their balance.
type AccountRepository interface {
	// ByField looks a payer up by one account field, for example "phone".
	// It returns ErrAccountNotFound naming the field when there is no match.
	ByField(ctx context.Context, field, value string) (*Account, error)
	// ByID loads a payer for a balance move, locking the row so two payments
	// settling at once cannot both read the same balance.
	ByID(ctx context.Context, id int64) (*Account, error)
	// Register loads the stand's own payer, locking the row.
	//
	// A register holds one balance. The payer a payment names is who it is
	// billed to — and on a stand that registers a payer per unknown order,
	// that is a different row every time — so a balance moved against it would
	// scatter the register's money across a list of one-payment accounts and
	// leave the figure an operator reads unchanged.
	Register(ctx context.Context) (*Account, error)
	// UpdateBalance stores a moved balance.
	UpdateBalance(ctx context.Context, id, balance int64) error
}

// OrderRepository loads and stores orders.
type OrderRepository interface {
	ByID(ctx context.Context, id int64) (*Order, error)
	ByAccount(ctx context.Context, accountID int64) ([]*Order, error)
	Update(ctx context.Context, o *Order) error
}

// WalkInRepository registers a payer the merchant has never seen, for stands
// configured to accept them.
type WalkInRepository interface {
	// Register returns the order identified by value for the given amount,
	// creating the payer and the order the first time either is asked for.
	//
	// The same call during CheckPerformTransaction and again during
	// CreateTransaction must yield the same order, or the check would approve
	// one order and the payment settle another.
	Register(ctx context.Context, value string, amount int64) (*Order, error)
}
