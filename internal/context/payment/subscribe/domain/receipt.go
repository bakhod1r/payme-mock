// Package domain models the Subscribe API: card tokens and receipts, and the
// receipt state machine the real provider walks through step by step.
package domain

import (
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// ReceiptState is the documented state of a receipt.
type ReceiptState int

// The receipt lifecycle. A receipt walks 0 → 1 → 2 → 3 → 4 on payment and
// 21 → 50 on cancellation; it never jumps straight to a terminal state.
const (
	// StateCreated means the receipt exists and awaits payment confirmation.
	StateCreated ReceiptState = 0
	// StateChecking is the initial verification, creating the transaction in
	// the supplier's billing.
	StateChecking ReceiptState = 1
	// StateWithdrawn means the funds have left the card.
	StateWithdrawn ReceiptState = 2
	// StateClosing means the transaction is being closed in the supplier's billing.
	StateClosing ReceiptState = 3
	// StatePaid is the terminal success state.
	StatePaid ReceiptState = 4
	// StateHeld covers state 5, which the documentation describes as archived
	// in the state table and as held funds on the holding page. Which meaning
	// applies depends on whether holding is enabled, so the interpretation is
	// configurable rather than hard-coded.
	StateHeld ReceiptState = 5
	// StateHoldAccepted means a hold command was received.
	StateHoldAccepted ReceiptState = 6
	// StatePaused means the receipt awaits manual intervention.
	StatePaused ReceiptState = 20
	// StateCancelQueued means cancellation has been requested but not completed.
	StateCancelQueued ReceiptState = 21
	// StateCloseQueued means the receipt is queued for closing in billing.
	StateCloseQueued ReceiptState = 30
	// StateCancelled is the terminal cancellation state.
	StateCancelled ReceiptState = 50
)

// Valid reports whether s is a documented state.
func (s ReceiptState) Valid() bool {
	switch s {
	case StateCreated, StateChecking, StateWithdrawn, StateClosing, StatePaid,
		StateHeld, StateHoldAccepted, StatePaused, StateCancelQueued,
		StateCloseQueued, StateCancelled:
		return true
	default:
		return false
	}
}

// Terminal reports whether the receipt has finished moving.
func (s ReceiptState) Terminal() bool {
	return s == StatePaid || s == StateCancelled
}

// InProgress reports whether the receipt is mid-payment, which is when a
// background step is still expected to advance it.
func (s ReceiptState) InProgress() bool {
	switch s {
	case StateChecking, StateWithdrawn, StateClosing, StateCancelQueued, StateCloseQueued:
		return true
	default:
		return false
	}
}

// CardSystem names the processing network, whose holding rules differ.
type CardSystem string

// The supported networks.
const (
	CardUzcard CardSystem = "uzcard"
	CardHumo   CardSystem = "humo"
)

// Receipt is the Subscribe API aggregate.
type Receipt struct {
	ID         int64
	SandboxID  int64
	ReceiptID  string
	MerchantID string
	Amount     int64
	Currency   int
	Commission int64
	State      ReceiptState
	Type       int
	// Payout reports money handed to the card rather than taken from it.
	Payout      bool
	Hold        bool
	HoldExpire  int64
	CardSystem  CardSystem
	Account     map[string]string
	Detail      map[string]any
	Description string
	CardID      *int64
	Payer       map[string]any
	CreateTime  int64
	PayTime     int64
	CancelTime  int64
	// MerchantTxn is the transaction this receipt drove the merchant through.
	// The Payme side issues that identifier, so it is the only side that can
	// record which payment on the merchant's books this receipt settled.
	MerchantTxn string
}

// CurrencyUZS is the ISO code the provider reports for Uzbek som.
const CurrencyUZS = 860

// TypeCardPayment is the `type` the provider reports for a receipt paid by
// card, which is every receipt a stand issues. A payout carries the same type;
// what tells the two apart is the receipt's own Payout flag, not a type value
// no documented response uses.
const TypeCardPayment = 1

// NewReceipt creates an unpaid receipt.
func NewReceipt(sandboxID int64, receiptID, merchantID string, amount int64,
	account map[string]string, now int64,
) *Receipt {
	return &Receipt{
		SandboxID:  sandboxID,
		ReceiptID:  receiptID,
		MerchantID: merchantID,
		Amount:     amount,
		Currency:   CurrencyUZS,
		Type:       TypeCardPayment,
		State:      StateCreated,
		Account:    account,
		CreateTime: now,
	}
}

// NewPayout creates an unpaid payout: money the register owes the card.
//
// A payout carries no order in the merchant's billing — nothing was bought —
// so it never reaches the Merchant API, and completing one is a single step
// rather than the walk a payment makes.
func NewPayout(sandboxID int64, receiptID, merchantID string, amount int64,
	account map[string]string, now int64,
) *Receipt {
	receipt := NewReceipt(sandboxID, receiptID, merchantID, amount, account, now)
	receipt.Payout = true

	return receipt
}

// Complete settles a payout. It refuses anything that is not a payout still
// waiting to be settled, so a payment can never be closed through this door.
func (r *Receipt) Complete(amount, now int64) error {
	if !r.Payout || r.State != StateCreated {
		return payerr.ErrCannotPerform
	}
	if amount != r.Amount {
		return payerr.ErrInvalidAmount
	}

	r.State = StatePaid
	r.PayTime = now

	return nil
}

// payProgression is the ordered walk a paying receipt makes. The provider
// moves through each step, so the mock does too rather than jumping to paid.
var payProgression = []ReceiptState{
	StateChecking, StateWithdrawn, StateClosing, StatePaid,
}

// NextPayState returns the state that follows the current one while paying,
// and whether any step remains.
func (r *Receipt) NextPayState() (ReceiptState, bool) {
	if r.State == StateCreated {
		return payProgression[0], true
	}
	for i, s := range payProgression {
		if s == r.State && i+1 < len(payProgression) {
			return payProgression[i+1], true
		}
	}
	return r.State, false
}

// BeginPay starts payment against a card. It refuses a receipt that is not
// waiting to be paid, since only a fresh receipt can start the walk.
func (r *Receipt) BeginPay(cardID int64, system CardSystem, hold bool, now int64) error {
	if r.State != StateCreated {
		return payerr.ErrCannotPerform
	}

	r.CardID = &cardID
	r.CardSystem = system
	r.Hold = hold
	r.State = StateChecking
	r.PayTime = now
	return nil
}

// Advance moves the receipt one step along whichever walk it is on. It reports
// whether the state changed, so a scheduler knows when to stop.
func (r *Receipt) Advance(now int64) bool {
	if r.State == StateCancelQueued {
		r.State = StateCancelled
		r.CancelTime = now
		return true
	}

	next, ok := r.NextPayState()
	if !ok {
		return false
	}

	// A held receipt stops short of paid and waits for confirmation.
	if r.Hold && next == StatePaid {
		r.State = StateHeld
		return true
	}

	r.State = next
	return true
}

// ConfirmHold releases held funds, completing the payment.
func (r *Receipt) ConfirmHold(now int64) error {
	if r.State != StateHeld {
		return payerr.ErrCannotPerform
	}
	r.State = StatePaid
	r.PayTime = now
	r.Hold = false
	return nil
}

// Cancel queues a receipt for cancellation. The receipt reaches state 50 only
// once the background step runs, exactly as the provider behaves.
func (r *Receipt) Cancel(now int64) error {
	switch {
	case r.State == StateCancelled || r.State == StateCancelQueued:
		return nil // repeating a cancellation returns the stored response
	case r.State == StateCreated:
		// Nothing was ever charged, so it cancels outright.
		r.State = StateCancelled
		r.CancelTime = now
		return nil
	case r.State == StatePaid || r.State == StateHeld || r.State.InProgress():
		r.State = StateCancelQueued
		r.CancelTime = now
		return nil
	default:
		return payerr.ErrCannotPerform
	}
}

// HoldExpired reports whether a hold has outlived the network's window, after
// which the funds are released back to the payer.
func (r *Receipt) HoldExpired(now int64) bool {
	return r.Hold && r.HoldExpire > 0 && now > r.HoldExpire
}
