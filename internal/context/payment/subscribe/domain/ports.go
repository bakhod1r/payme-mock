package domain

import (
	"context"
	"errors"
)

// ErrNotFound is what repositories return when a lookup finds nothing. The
// application layer maps it to the protocol error each method expects.
var ErrNotFound = errors.New("not found")

// CardRepository stores tokenized cards.
type CardRepository interface {
	ByToken(ctx context.Context, token string) (*Card, error)
	ByID(ctx context.Context, id int64) (*Card, error)
	// ByNumber loads the card already on the stand for a number, whoever put
	// it there. Tokenizing a number an operator has rigged must produce a card
	// that behaves as rigged, so the behaviour is looked up before the new
	// token is stored.
	ByNumber(ctx context.Context, number string) (*Card, error)
	Create(ctx context.Context, c *Card) error
	Update(ctx context.Context, c *Card) error
	// TokenFor returns the token the calling register holds for a card,
	// issuing the offered one if it holds none. A card is one card, but each
	// register is handed its own string for it, so a token identifies both the
	// card and the till that asked for it.
	TokenFor(ctx context.Context, cardID int64, offered string) (string, error)
}

// ReceiptRepository stores receipts.
type ReceiptRepository interface {
	ByReceiptID(ctx context.Context, receiptID string) (*Receipt, error)
	Create(ctx context.Context, r *Receipt) error
	Update(ctx context.Context, r *Receipt) error
	// List returns receipts created within [from, to], newest first, limited
	// to count and skipping offset entries.
	List(ctx context.Context, from, to int64, count, offset int) ([]*Receipt, error)
}

// CashboxLedger records what a payout does to the register: the balance it
// moves, and the transaction the register's books show for it.
//
// A payment reaches the merchant's books through the Merchant API chain, which
// both writes the transaction and applies the balance at its end. A payout has
// no chain — the provider is paying a card, not asking a merchant for anything —
// so the stand has to write both itself, or a register that paid out all day
// would show neither a moved figure nor a single payment.
type CashboxLedger interface {
	// OpenPayout records a payout that has been asked for and not yet settled.
	// Nothing moves: the money leaves when the payout completes, and a payout
	// that never completes is exactly the row an operator goes looking for.
	OpenPayout(ctx context.Context, payout Payout) error
	// SettlePayout takes the money out of the register and marks the payout
	// done, as one act: a balance moved without a payment to explain it is
	// worse than no history at all.
	SettlePayout(ctx context.Context, payout Payout) error
	// Balance is what the register holds now. A payout register that runs dry
	// refuses every withdrawal, so an integration wants to watch the figure
	// rather than discover it through a refusal.
	Balance(ctx context.Context) (Cashbox, error)
}

// Cashbox is a register's own money, as the stand reports it.
type Cashbox struct {
	// Balance is held in tiyin, as every amount in the protocol is.
	Balance int64
	// Currency is the ISO code the register works in.
	Currency int
	// Kind says which way money moves on this register, because a figure with
	// no direction cannot be read: on a top-up register payments raise it, on a
	// payout register they lower it.
	Kind string
	// Blocked reports a register that has been stopped: it holds money and will
	// still pay nobody.
	Blocked bool
}

// Payout is one payout as the register's books need it: what it pays, who it
// names, and when each half of it happened.
type Payout struct {
	// TransactionID names the payout in the register's books, the way a
	// payment's Merchant API transaction id does. It is what links the receipt
	// to the row the books hold for it.
	TransactionID string
	Amount        int64
	Account       map[string]string
	CreateTime    int64
	PayTime       int64
}

// MerchantClient is how the Payme side reaches a merchant's Merchant API.
// The domain never learns whether it is the local mock or the real provider.
type MerchantClient interface {
	CheckPerformTransaction(ctx context.Context, amount int64, account map[string]string) error
	CreateTransaction(ctx context.Context, id string, timeMillis, amount int64, account map[string]string) (transaction string, err error)
	PerformTransaction(ctx context.Context, id string) error
	CancelTransaction(ctx context.Context, id string, reason int) error
}

// Scheduler queues the background step that advances a receipt to its next
// state. Real payments are not instantaneous, and neither are these.
type Scheduler interface {
	// ScheduleAdvance asks for the receipt to be advanced after a delay.
	ScheduleAdvance(ctx context.Context, receiptID string, delayMillis int) error
}

// TokenGenerator produces card tokens and receipt identifiers that look like
// the provider's: 24-character hex ids and long base64 tokens.
type TokenGenerator interface {
	CardToken() string
	ReceiptID() string
	TransactionID() string
}

// SMSSender delivers a receipt or verification code to a phone. The mock
// records the delivery instead of sending anything.
type SMSSender interface {
	Send(ctx context.Context, phone, message string) error
}
