package domain

import (
	"fmt"

	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// Kind is the direction a cash register moves money in.
//
// Payme's registers are not all the same: one takes money from the payer, the
// others hand money back. Which one a stand is decides what a performed payment
// does to the balance, so it is a property of the register rather than of any
// single payment.
type Kind string

// The register kinds.
const (
	// KindTopup takes money from the payer, so a performed payment adds to the
	// balance the merchant holds.
	KindTopup Kind = "topup"
	// KindDividend pays earnings out to the payer, so a performed payment
	// takes from the balance.
	KindDividend Kind = "dividend"
	// KindDeposit returns deposited funds to the payer, so a performed payment
	// takes from the balance.
	KindDeposit Kind = "deposit"
)

// Kinds lists every register kind, in the order the console offers them.
var Kinds = []Kind{KindTopup, KindDividend, KindDeposit}

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	switch k {
	case KindTopup, KindDividend, KindDeposit:
		return true
	default:
		return false
	}
}

// Inbound reports whether a performed payment brings money in. Everything that
// is not inbound pays out, which is the case that can run short of funds.
func (k Kind) Inbound() bool { return k == KindTopup }

// Describe says in words what a payment does on this register, which is what
// the console shows next to the balance.
func (k Kind) Describe() string {
	switch k {
	case KindTopup:
		return "payments add to the balance"
	case KindDividend:
		return "dividend payouts take from the balance"
	case KindDeposit:
		return "deposit returns take from the balance"
	default:
		return "unknown register kind"
	}
}

// ErrInsufficientFunds reports a payout larger than the register holds.
//
// The protocol has no dedicated code for an empty register: the documented
// merchant errors cover a wrong amount, a missing transaction, a completed
// order and an operation that cannot be carried out. A payout with nothing
// behind it is the last of those, so -31008 is what the payer's side sees,
// with the reason spelled out in the message.
var ErrInsufficientFunds = payerr.ErrCannotPerform.WithMessage(payerr.NewMessage(
	"Недостаточно средств на кассе",
	"Kassada mablag' yetarli emas",
	"Insufficient funds in the cash register",
))

// Apply moves the balance for a performed payment of the given amount.
//
// A payout is refused rather than allowed to go negative: a register that owes
// more than it holds would report a balance no reconciliation could explain.
func (a *Account) Apply(kind Kind, amount int64) error {
	if !kind.Valid() {
		return fmt.Errorf("unknown register kind %q", kind)
	}

	// A blocked payer moves no money in either direction. The flag was stored
	// and shown and never once consulted, so a register an operator had stopped
	// went on answering every payment with a 200 — the one thing a stand exists
	// to let them rehearse.
	if err := a.Usable(); err != nil {
		return err
	}

	if kind.Inbound() {
		a.Balance += amount
		return nil
	}

	if a.Balance < amount {
		return ErrInsufficientFunds
	}

	a.Balance -= amount

	return nil
}

// Usable reports whether the account may take part in a payment at all.
//
// The provider answers -31008 for this: it is not the amount, not the order and
// not the card, it is that this payer is stopped. Blocking is the operator's
// switch for rehearsing exactly that, so it refuses where the money would move
// rather than where a screen happens to read the flag.
func (a *Account) Usable() error {
	if a.Blocked {
		return payerr.ErrCannotPerform
	}

	return nil
}

// Reverse undoes what Apply did, which is what a cancellation after a
// performed payment has to do.
//
// A reversal is never refused: the money moved once already, so putting it back
// cannot leave the register in a state it was not in a moment ago.
func (a *Account) Reverse(kind Kind, amount int64) {
	if kind.Inbound() {
		a.Balance -= amount
		return
	}

	a.Balance += amount
}
