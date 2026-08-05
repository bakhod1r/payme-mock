package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// A receipt is what the Subscribe API works on, and it is a different record
// from a merchant transaction: a client charging a saved card never reaches the
// merchant side of the stand at all, so the transactions screen stays empty
// while receipts pile up. Without a screen of their own that traffic is only
// visible in the request log, one call at a time.

// receiptRow is one receipt with everything the protocol carried, so the
// details view needs no second query.
type receiptRow struct {
	ID        int64
	Sandbox   string
	ReceiptID string
	Merchant  string
	// MerchantName is the business the payer sees on this receipt.
	MerchantName string
	// From and To are where the money moved: a card and a register, in the
	// order it travelled. It is the question a receipt is opened with, and
	// answering it means reading three fields and knowing which way a payout
	// goes.
	From   string
	To     string
	Amount int64
	// Currency and Commission are the money fields beside the amount, kept
	// because a receipt that charged a commission is a different receipt.
	Currency   int
	Commission int64
	State      int
	// StateLabel is what the state means, since the protocol's numbers say
	// nothing on their own.
	StateLabel string
	// Kind is "payout" or "payment", which is the first thing anyone asks of a
	// receipt: did money leave the card or arrive on it.
	Kind string
	// Payout is the same fact for the template, which colours the two apart.
	Payout bool
	Type   int
	Hold   bool
	// Card is the masked number the receipt charged, or a dash when the card
	// has since been deleted: the row survives its card by design. CardID is
	// zero in that case, which is what decides whether the number is a link.
	Card       string
	CardID     int64
	CardSystem string
	// Way is the direction in the words the integration uses: a top-up takes
	// money off the card, a withdrawal puts it back. Kind is the same fact in
	// the protocol's terms, which the filter speaks.
	Way string
	// Account is the `account` object as it arrived, and AccountFields is that
	// object read out field by field, because a payment is identified by what
	// the account carried and a screen printing one line of JSON makes the
	// reader do the parsing.
	Account       string
	AccountFields []accountField
	Detail        string
	Payer         string
	Meta          string
	Description   string
	ProcessingID  string
	CreateTime    int64
	PayTime       int64
	CancelTime    int64
	HoldExpire    int64
	CreatedAt     string
	UpdatedAt     string
	// MerchantTxn is the transaction this receipt drove the merchant through,
	// and TransactionRow is that transaction's own row when the stand still
	// holds it. A payout has neither: nothing is asked of a merchant.
	MerchantTxn    string
	TransactionRow int64
	// Failed reports a receipt that ended cancelled, which is the row worth
	// finding.
	Failed bool
	// Open reports a receipt still on its way, neither paid nor cancelled.
	Open bool
}

// The kinds of receipt the filter offers, which is the payment/payout split.
const (
	receiptPayments = "payment"
	receiptPayouts  = "payout"
)

// receiptFilter is what the receipts screen's filter form submits.
type receiptFilter struct {
	Sandbox string
	State   string
	Kind    string
	Query   string
	Sort    string
}

// receiptStateLabel names one receipt state. An unknown number is printed
// rather than hidden: the state column comes from the database, and a state the
// console has not heard of is exactly what someone needs to see.
func receiptStateLabel(state int) string {
	labels := map[int]string{
		0:  "created",
		1:  "checking",
		2:  "withdrawn",
		3:  "closing",
		4:  "paid",
		5:  "archived",
		6:  "hold accepted",
		20: "paused",
		21: "cancellation queued",
		30: "closing queued",
		50: "cancelled",
	}

	if label, ok := labels[state]; ok {
		return label
	}

	return "unknown state"
}

// The columns every receipt read selects, written once so the list and one
// row's own page can never drift into showing different records.
var receiptColumns = `
	r.id, s.slug, r.receipt_id, r.merchant_id, coalesce(s.merchant_name, ''),
	r.amount, r.currency,
	r.commission, r.state, r.type, r.hold, r.hold_expire,
	coalesce(c.number_full, ''), coalesce(c.id, 0),
	coalesce(r.card_system, ''), r.account::text,
	coalesce(r.detail::text, ''), coalesce(r.payer::text, ''),
	coalesce(r.meta::text, ''), r.description, coalesce(r.processing_id, ''),
	r.create_time, r.pay_time, r.cancel_time, r.payout,
	` + stamp("r.created_at") + `, ` + stamp("r.updated_at") + `,
	coalesce(r.merchant_txn, ''), coalesce(t.id, 0)`

// The join a receipt read needs: its stand, and the card it charged if that
// card still exists.
const receiptSource = `
	FROM mock.receipts r
	JOIN control.sandboxes s ON s.id = r.sandbox_id
	LEFT JOIN mock.cards c ON c.id = r.card_id
	LEFT JOIN merchant.transactions t
	       ON t.sandbox_id = r.sandbox_id AND t.payme_id = r.merchant_txn`

// scanReceipt reads one row of receiptColumns.
func scanReceipt(row pgx.Row) (receiptRow, error) {
	var (
		out  receiptRow
		card string
	)

	if err := row.Scan(&out.ID, &out.Sandbox, &out.ReceiptID, &out.Merchant,
		&out.MerchantName, &out.Amount, &out.Currency, &out.Commission, &out.State, &out.Type,
		&out.Hold, &out.HoldExpire, &card, &out.CardID, &out.CardSystem, &out.Account,
		&out.Detail, &out.Payer, &out.Meta, &out.Description, &out.ProcessingID,
		&out.CreateTime, &out.PayTime, &out.CancelTime, &out.Payout,
		&out.CreatedAt, &out.UpdatedAt, &out.MerchantTxn,
		&out.TransactionRow); err != nil {
		return receiptRow{}, err
	}

	out.StateLabel = receiptStateLabel(out.State)
	out.AccountFields = readAccount(out.Account)
	out.Failed = out.State == 50
	out.Open = out.State != 4 && out.State != 50

	out.Kind = receiptPayments
	out.Way = wayTopUp
	if out.Payout {
		out.Kind = receiptPayouts
		out.Way = wayWithdraw
	}

	// A receipt outlives the card it charged, so the number is only shown when
	// the card is still there; masking a number that is gone would invent one.
	out.Card = "—"
	if card != "" {
		out.Card = maskNumber(card)
	}

	// Where the money went, in the order it travelled. A register is named by
	// the business behind it when it has one, because that is what the payer
	// sees; the slug is the operator's word for the same thing.
	register := out.Sandbox
	if out.MerchantName != "" {
		register = out.MerchantName + " · " + out.Sandbox
	}

	out.From, out.To = out.Card, register
	if out.Payout {
		out.From, out.To = register, out.Card
	}

	return out, nil
}

// maskNumber shows a card the way the protocol does: the issuer and the last
// four, nothing in between.
func maskNumber(number string) string {
	if len(number) < 10 {
		return number
	}

	return number[:6] + strings.Repeat("*", len(number)-10) + number[len(number)-4:]
}

// ReceiptByID returns one receipt with everything the protocol carried.
func (s *store) ReceiptByID(ctx context.Context, id int64) (receiptRow, error) {
	out, err := scanReceipt(s.pool.QueryRow(ctx,
		`SELECT `+receiptColumns+receiptSource+` WHERE r.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return receiptRow{}, errNoRow
	}
	if err != nil {
		return receiptRow{}, fmt.Errorf("select receipt: %w", err)
	}

	return out, nil
}

func (a *app) showReceipt(w http.ResponseWriter, r *http.Request, user string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	receipt, err := a.store.ReceiptByID(r.Context(), id)
	if errors.Is(err, errNoRow) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, "load receipt", err)
		return
	}

	a.render(w, "receipt", view{
		Title: receipt.ReceiptID, Nav: "receipts", User: user,
		Notice: notice(r), Receipt: receipt,
	})
}
