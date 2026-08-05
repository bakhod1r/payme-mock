package domain

import (
	"strings"

	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// Card is a tokenized payment card. The mock keeps the full number only
// because it emulates the processing side; a real integration stores nothing
// but the token.
type Card struct {
	ID         int64
	SandboxID  int64
	Token      string
	NumberFull string
	Expire     string // MM/YY as returned to callers
	Recurrent  bool
	Verify     bool
	VerifyCode string
	Phone      string
	Balance    int64
	Removed    bool
	// Outcome is what the card is rigged to do. A card created through the API
	// works; one an operator added to rehearse a refusal need not.
	Outcome Outcome
	// Source is how the card reached the stand: an operator added it, or an
	// integration tokenized it for itself.
	Source Source
	// SMSEnabled reports a card whose owner can be sent an OTP at all. The
	// provider's own sandbox ships one that cannot, and an integration that
	// never handles that refusal stalls on a code the payer will never get.
	SMSEnabled bool
	// Frozen keeps the balance still: the card pays and is paid without the
	// figure moving, so one card can drive a run of payments untended.
	Frozen bool
	// DelayMillis is how long the card stalls before answering, which is what
	// a timeout is written against.
	DelayMillis int64
	// Account and Customer are what cards.create was told the card is for: the
	// account it intends to pay, and the application's own identifier for the
	// payer. The provider keeps both against the token and acts on neither.
	Account  map[string]string
	Customer string
	// VerifyCodeSentAt and VerifyWaitMillis bound how long an OTP stays usable.
	VerifyCodeSentAt int64
	VerifyWaitMillis int64
	// RegisteredAt is when an integration last tokenized this card for itself,
	// zero if none ever has. It is not the same fact as Source: an operator's
	// card that cards.create later asked for is handed back rather than copied,
	// so the row keeps saying where it came from while this says who has it.
	RegisteredAt int64
}

// Register records that an integration tokenized this card for itself.
func (c *Card) Register(now int64) { c.RegisteredAt = now }

// DefaultVerifyWaitMillis is the OTP validity window the documentation shows.
const DefaultVerifyWaitMillis int64 = 60000

// MaskNumber renders a card number the way the provider returns it: the first
// six and last four digits, with the middle hidden.
func MaskNumber(number string) string {
	if len(number) < 10 {
		return number
	}
	return number[:6] + strings.Repeat("*", len(number)-10) + number[len(number)-4:]
}

// PlainExpire turns the MM/YY a card carries back into the MMYY a receipt
// reports. The provider prints the expiry both ways: slashed on a card object,
// plain inside a receipt.
func PlainExpire(expire string) string {
	return strings.ReplaceAll(expire, "/", "")
}

// FormatExpire turns the MMYY a caller sends into the MM/YY the provider returns.
func FormatExpire(expire string) string {
	if len(expire) != 4 {
		return expire
	}
	return expire[:2] + "/" + expire[2:]
}

// DetectSystem infers the processing network from the card prefix. Uzcard
// numbers begin with 8600 and Humo with 9860.
func DetectSystem(number string) CardSystem {
	if strings.HasPrefix(number, "9860") {
		return CardHumo
	}
	return CardUzcard
}

// NumberMask returns the masked number for display.
func (c *Card) NumberMask() string { return MaskNumber(c.NumberFull) }

// System reports the card's processing network.
func (c *Card) System() CardSystem { return DetectSystem(c.NumberFull) }

// DefaultVerifyCode is the stand's shared OTP, and the one every card an
// operator adds through the console takes. It is a constant rather than a
// setting read back from the profile because a screen showing the code has to
// agree with the service issuing it, and an operator types it from memory.
const DefaultVerifyCode = "666666"

// ExpiryCode is the OTP a card expects: its expiry, with the year repeated to
// fill the six digits an OTP has. A card expiring 12/26 takes 122626.
//
// A stand holds many cards and one shared code cannot tell them apart;
// deriving the code from the card means anyone holding the card already knows
// what to type, with nothing to look up.
func ExpiryCode(expire string) string {
	digits := strings.ReplaceAll(expire, "/", "")
	if len(digits) != 4 {
		return ""
	}
	return digits + digits[2:]
}

// ExpectedVerifyCode returns the OTP this card takes.
//
// A card an operator put on the stand takes the stand's shared code: the
// operator already knows it, it is the same for every card they add, and it is
// what makes a card typed into the console usable without reading anything
// back. A card an integration tokenized for itself has no operator behind it,
// so it keeps the code its own expiry spells out — the caller is holding the
// card and can work the code out from the number they just sent.
func (c *Card) ExpectedVerifyCode(shared string) string {
	if c.Source.FromConsole() && shared != "" {
		return shared
	}

	if code := ExpiryCode(c.Expire); code != "" {
		return code
	}

	// An expiry the mock cannot read leaves the card on the shared code rather
	// than on no code at all, which would make it impossible to verify.
	return shared
}

// SendVerifyCode records that an OTP was issued, starting its validity window.
func (c *Card) SendVerifyCode(code string, now, waitMillis int64) {
	c.VerifyCode = code
	c.VerifyCodeSentAt = now
	c.VerifyWaitMillis = waitMillis
}

// Operable reports whether the card may be touched at all. A card the bank
// stopped or one past its date refuses every operation, not only the payment,
// so the check belongs before the OTP as well as before the charge.
func (c *Card) Operable() error {
	if c.Removed {
		return payerr.ErrCannotPerform
	}
	if c.Outcome.blocksEverything() {
		return c.Outcome.Err()
	}
	return nil
}

// VerifyWith checks an OTP. A code that is wrong, or right but past its
// window, leaves the card unverified.
//
// The stand's shared code is taken as well as the card's own. A stand is not a
// bank: nobody reads the SMS, and the operator driving it should not have to
// work out which of two rules a particular card falls under before they can
// confirm it. So 666666 confirms anything, and a card's own code — its expiry,
// which is what an integration holding the card can derive — confirms it too.
func (c *Card) VerifyWith(code string, now int64) error {
	if err := c.Operable(); err != nil {
		return err
	}
	// A card rigged to fail verification takes the right code and rejects it,
	// which is the only way to reach the branch an integration handles when the
	// payer's bank refuses the confirmation.
	if c.Outcome == OutcomeVerifyFailed {
		return c.Outcome.Err()
	}
	if c.VerifyCode == "" || (code != c.VerifyCode && code != DefaultVerifyCode) {
		return payerr.ErrCannotPerform
	}
	if c.verifyExpired(now) {
		return payerr.ErrCannotPerform
	}

	c.Verify = true
	c.VerifyCode = ""
	return nil
}

// verifyExpired reports whether the issued OTP is past its validity window.
func (c *Card) verifyExpired(now int64) bool {
	if c.VerifyWaitMillis <= 0 {
		return false
	}
	return now-c.VerifyCodeSentAt > c.VerifyWaitMillis
}

// Usable reports whether the card can pay: it must exist, be verified, and
// hold enough balance.
func (c *Card) Usable(amount int64) error {
	if err := c.Operable(); err != nil {
		return err
	}
	if err := c.Outcome.Err(); err != nil {
		return err
	}
	if !c.Verify {
		return payerr.ErrCannotPerform
	}
	if c.Balance < amount {
		return payerr.ErrCannotPerform
	}
	return nil
}

// SMSReachable reports whether an OTP can be sent to this card's owner. A card
// without SMS is refused before a code is issued rather than after a wrong one
// is typed.
func (c *Card) SMSReachable() error {
	if c.SMSEnabled {
		return nil
	}

	return payerr.ErrCannotPerform.WithMessage(payerr.NewMessage(
		"СМС-информирование не подключено",
		"SMS-xabarnoma ulanmagan",
		"SMS notification is not enabled",
	))
}

// Charge deducts an amount. Callers check Usable first — payment refuses a
// card before the merchant is contacted, so by the time money moves the card
// has already been cleared and re-checking here would be dead code.
//
// A frozen card is charged without its balance moving: the payment is real,
// the figure simply stays where an operator put it.
func (c *Card) Charge(amount int64) {
	if c.Frozen {
		return
	}
	c.Balance -= amount
}

// Refund returns a charged amount to the card.
func (c *Card) Refund(amount int64) { c.credit(amount) }

// Receive credits a payout to the card.
func (c *Card) Receive(amount int64) { c.credit(amount) }

// credit adds to the balance unless the card is frozen, in which case nothing
// moves in either direction — a balance that only held still one way would
// drift on the first refund.
func (c *Card) credit(amount int64) {
	if c.Frozen {
		return
	}
	c.Balance += amount
}
