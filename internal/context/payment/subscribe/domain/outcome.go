package domain

import (
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// Outcome is what a card is rigged to do. A stand needs a card that fails on
// purpose as much as it needs one that works: an integration cannot rehearse
// its refusal path against a card that always pays.
//
// The protocol has one code for a refused card, -31008, so every failing
// outcome answers with that code and differs only in the text — which is what
// the real provider does too.
type Outcome string

const (
	// OutcomeSuccess is the ordinary card: it verifies and it pays.
	OutcomeSuccess Outcome = "success"
	// OutcomeInsufficientFunds refuses payment however much the card holds, so
	// the case can be rehearsed without arranging a balance to match an amount.
	OutcomeInsufficientFunds Outcome = "insufficient_funds"
	// OutcomeBlocked refuses everything, the way a card the bank stopped does.
	OutcomeBlocked Outcome = "blocked"
	// OutcomeExpired refuses everything, the way a card past its date does.
	OutcomeExpired Outcome = "expired"
	// OutcomeVerifyFailed takes the OTP and rejects it, so a card can never
	// reach the state where it is allowed to pay.
	OutcomeVerifyFailed Outcome = "verify_failed"
	// OutcomeSystemError answers with a failure that is not about the card at
	// all, which is the one an integration is least likely to have handled.
	OutcomeSystemError Outcome = "system_error"
)

// Outcomes lists every rigged behaviour, in the order the console offers them.
var Outcomes = []Outcome{
	OutcomeSuccess,
	OutcomeInsufficientFunds,
	OutcomeBlocked,
	OutcomeExpired,
	OutcomeVerifyFailed,
	OutcomeSystemError,
}

// Valid reports whether o is one of the known outcomes.
func (o Outcome) Valid() bool {
	for _, known := range Outcomes {
		if known == o {
			return true
		}
	}
	return false
}

// Label is what the outcome is called on screen.
func (o Outcome) Label() string {
	switch o {
	case OutcomeInsufficientFunds:
		return "insufficient funds"
	case OutcomeBlocked:
		return "blocked"
	case OutcomeExpired:
		return "expired"
	case OutcomeVerifyFailed:
		return "verification fails"
	case OutcomeSystemError:
		return "system error"
	default:
		return "success"
	}
}

// Err returns the protocol error a rigged card answers with, or nil when the
// card is meant to work.
func (o Outcome) Err() error {
	switch o {
	case OutcomeInsufficientFunds:
		return payerr.ErrCannotPerform.WithMessage(payerr.NewMessage(
			"На карте недостаточно средств",
			"Kartada mablag' yetarli emas",
			"Insufficient funds on the card",
		))
	case OutcomeBlocked:
		return payerr.ErrCannotPerform.WithMessage(payerr.NewMessage(
			"Карта заблокирована",
			"Karta bloklangan",
			"The card is blocked",
		))
	case OutcomeExpired:
		return payerr.ErrCannotPerform.WithMessage(payerr.NewMessage(
			"Срок действия карты истёк",
			"Kartaning amal qilish muddati tugagan",
			"The card has expired",
		))
	case OutcomeVerifyFailed:
		return payerr.ErrCannotPerform.WithMessage(payerr.NewMessage(
			"Не удалось подтвердить карту",
			"Kartani tasdiqlab bo'lmadi",
			"The card could not be verified",
		))
	case OutcomeSystemError:
		// Not -31008: the provider is not saying the card was refused, it is
		// saying it never got far enough to look.
		return payerr.ErrTransport.WithMessage(payerr.NewMessage(
			"Неизвестная системная ошибка",
			"Noma'lum tizim xatosi",
			"Unknown system error",
		))
	default:
		return nil
	}
}

// blocksEverything reports an outcome that refuses every operation rather than
// only the payment: a stopped or expired card cannot even be verified.
func (o Outcome) blocksEverything() bool {
	return o == OutcomeBlocked || o == OutcomeExpired || o == OutcomeSystemError
}

// Source is how a card reached the stand.
type Source string

const (
	// SourceAPI is a card an integration tokenized for itself. It is the
	// register's own card, and it behaves as the documentation says.
	SourceAPI Source = "api"
	// SourceConsole is a card an operator added to rehearse something. Being
	// rigged is the point of it: it is told outright what to answer.
	SourceConsole Source = "console"
)

// FromConsole reports a card an operator added.
func (s Source) FromConsole() bool { return s == SourceConsole }
