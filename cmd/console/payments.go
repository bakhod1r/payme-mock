package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	billing "github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
)

// A payment is one thing that happened, and the stand records it twice: as the
// receipt the Subscribe API works on, and as the transaction the merchant was
// driven through. Two screens made the reader join them by eye — and worse, made
// each screen lie by omission, because a payout has no transaction and a call
// made straight against the Merchant API has no receipt. This screen is the one
// list: a row per payment, whichever halves of it exist.

// The sides a row can come from, which is also what the screen filters by.
const (
	// sideReceipt is a payment the Subscribe API drove: a card was charged or
	// paid, and the receipt is the record that names the card.
	sideReceipt = "receipt"
	// sideMerchant is a payment that only ever existed on the merchant's books,
	// driven straight against the Merchant API with no card behind it.
	sideMerchant = "merchant"
)

// The ways money moved, in the words the integration itself uses: a top-up
// takes money off the card, a withdrawal puts it back. The protocol calls both
// a receipt and tells them apart by a flag, which is true and unreadable.
const (
	wayTopUp    = "top up"
	wayWithdraw = "withdraw"
	wayBilled   = "merchant only"
)

// paymentRow is one payment, from whichever side the stand recorded it.
type paymentRow struct {
	// Side says which record this row is: a receipt, or a merchant transaction
	// with no receipt behind it.
	Side string
	// ID is the row's own identifier on its side, and Href is where clicking it
	// goes — a receipt's page or a transaction's.
	ID   int64
	Href string
	// Ref is what the row is called: the receipt id, or the Payme transaction
	// id when there is no receipt.
	Ref     string
	Created string
	Sandbox string
	Kind    string
	// KindMeaning says in words what this register does with a payment.
	KindMeaning string
	// Card is the masked number, or a dash: a merchant-side payment never knew
	// one, and a receipt outlives the card it charged.
	Card   string
	CardID int64
	Amount int64
	// State and StateLabel are in the vocabulary of this row's own side: the
	// two sides number their states differently, and each row is read in its
	// own. Which side it is is already plain from the What column.
	State      int
	StateLabel string
	Way        string
	Payout     bool
	// Failed and Open are the same question in both vocabularies, worked out
	// per side so a template need not know either.
	Failed bool
	Open   bool
	// Merchant is the transaction a receipt drove, when it drove one. It is what
	// says the merchant's books agree with the provider's.
	MerchantRow   int64
	MerchantState int
	MerchantLabel string
	HasMerchant   bool
	// Order is what the payment settled on the merchant's side, when it settled
	// one. A payout settles nothing: nothing was bought.
	Order string
	// Account is the account object as it arrived. It is carried so the search
	// can reach into it, and shown on the row's own page rather than here.
	Account string
}

// paymentFilter is what the payments screen's filter form submits.
type paymentFilter struct {
	Sandbox string
	// Way narrows to charges, payouts, or the merchant-only rows.
	Way string
	// State narrows by how a payment ended, in terms both sides share.
	State string
	Query string
	Sort  string
}

// Narrowed reports a filter that leaves something out. The template asks it, so
// a screen can say when it is showing less than everything.
func (f paymentFilter) Narrowed() bool {
	return f.Sandbox != "" || f.Way != "" || f.State != "" || f.Query != ""
}

// The state groupings the screen offers. They are deliberately not state
// numbers: the two sides number their states differently, and "is it done" is
// the question worth asking across both.
const (
	paymentOpen = "open"
	paymentDone = "done"
	paymentBad  = "failed"
)

// paymentStates are the groupings the state filter offers.
func paymentStates() []stateOption {
	return []stateOption{
		{Value: paymentOpen, Label: "in progress · neither settled nor cancelled"},
		{Value: paymentDone, Label: "settled · the money moved"},
		{Value: paymentBad, Label: "cancelled · it never completed"},
	}
}

// paymentWays are the directions a row can be.
func paymentWays() []sortOption {
	return []sortOption{
		{Value: receiptPayments, Label: "top up · money off the card"},
		{Value: receiptPayouts, Label: "withdraw · money back onto the card"},
		{Value: sideMerchant, Label: "merchant only · no card behind it"},
	}
}

// paymentSelect is the one list: every receipt, and every merchant transaction
// no receipt accounts for.
//
// The second half is not noise. A transaction with no receipt is either a
// payment made straight against the Merchant API — which is how the merchant
// side is rehearsed on its own — or a payout, which never asks a merchant for
// anything and so is written by the stand itself. Leaving it out would make the
// screen claim payments that happened did not.
const paymentSelect = `
	SELECT '` + sideReceipt + `' AS side, r.id, r.receipt_id AS ref,
	       r.created_at, s.slug, s.kind, coalesce(c.number_full, ''),
	       coalesce(c.id, 0), r.amount, r.state, r.payout,
	       coalesce(t.id, 0), coalesce(t.state, 0), t.id IS NOT NULL,
	       coalesce(t.order_id::text, ''), r.account::text
	FROM mock.receipts r
	JOIN control.sandboxes s ON s.id = r.sandbox_id
	LEFT JOIN mock.cards c ON c.id = r.card_id
	LEFT JOIN merchant.transactions t
	       ON t.sandbox_id = r.sandbox_id AND t.payme_id = r.merchant_txn

	UNION ALL

	SELECT '` + sideMerchant + `', t.id, t.payme_id, t.created_at, s.slug, s.kind,
	       '', 0, t.amount, t.state, FALSE, t.id, t.state, TRUE,
	       coalesce(t.order_id::text, ''), t.account::text
	FROM merchant.transactions t
	JOIN control.sandboxes s ON s.id = t.sandbox_id
	WHERE NOT EXISTS (
		SELECT 1 FROM mock.receipts r
		WHERE r.sandbox_id = t.sandbox_id AND r.merchant_txn = t.payme_id)`

// Payments lists both sides as one, newest first, narrowed by the filter.
//
// The union is wrapped so the filter and the ordering are written once rather
// than twice, which is also what keeps the two halves from drifting into
// different answers to the same question.
func (s *store) Payments(ctx context.Context, f paymentFilter, page pageRequest) ([]paymentRow, error) {
	rows, err := s.pool.Query(ctx, `
		WITH everything AS (`+paymentSelect+`)
		SELECT side, id, ref, `+stamp("created_at")+`, slug, kind, number,
		       card_id, amount, state, payout, merchant_row, merchant_state,
		       has_merchant, order_ref, account
		FROM everything AS e(side, id, ref, created_at, slug, kind, number,
		                     card_id, amount, state, payout, merchant_row,
		                     merchant_state, has_merchant, order_ref, account)
		WHERE ($1 = '' OR slug = $1)
		  AND CASE
		        WHEN $2 = '' THEN TRUE
		        WHEN $2 = '`+receiptPayouts+`' THEN side = '`+sideReceipt+`' AND payout
		        WHEN $2 = '`+receiptPayments+`' THEN side = '`+sideReceipt+`' AND NOT payout
		        WHEN $2 = '`+sideMerchant+`' THEN side = '`+sideMerchant+`'
		        ELSE TRUE
		      END
		  -- Each side is asked in its own numbering: a receipt is paid at 4 and
		  -- cancelled at 50, a merchant payment is performed at 2 and cancelled
		  -- below zero. Asking one question of both is the point of the screen.
		  AND CASE
		        WHEN $3 = '' THEN TRUE
		        WHEN $3 = '`+paymentOpen+`' THEN
		             (side = '`+sideReceipt+`' AND state NOT IN (4, 50))
		          OR (side = '`+sideMerchant+`' AND state = 1)
		        WHEN $3 = '`+paymentDone+`' THEN
		             (side = '`+sideReceipt+`' AND state = 4)
		          OR (side = '`+sideMerchant+`' AND state = 2)
		        WHEN $3 = '`+paymentBad+`' THEN
		             (side = '`+sideReceipt+`' AND state = 50)
		          OR (side = '`+sideMerchant+`' AND state < 0)
		        ELSE TRUE
		      END
		  AND ($4 = '' OR ref ILIKE '%' || $4 || '%'
		       OR number ILIKE '%' || $4 || '%'
		       -- The order is searched too: on the merchant's side a payment is
		       -- remembered by what it settled, not by its own identifier.
		       OR order_ref = $4
		       -- And the account object: the identifier someone is chasing is
		       -- usually inside it, not in a column beside it. A backend's own
		       -- order id is often a UUID in there.
		       OR account ILIKE '%' || $4 || '%')
		ORDER BY
		  CASE WHEN $6 = '`+sortLargest+`' THEN amount END DESC,
		  CASE WHEN $6 = '`+sortOldest+`' THEN created_at END ASC,
		  created_at DESC, id DESC
		LIMIT $5 OFFSET $7`,
		f.Sandbox, f.Way, f.State, f.Query, page.Limit, f.Sort, page.Offset)
	if err != nil {
		return nil, fmt.Errorf("select payments: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (paymentRow, error) {
		var (
			out  paymentRow
			card string
		)

		if err := row.Scan(&out.Side, &out.ID, &out.Ref, &out.Created,
			&out.Sandbox, &out.Kind, &card, &out.CardID, &out.Amount,
			&out.State, &out.Payout, &out.MerchantRow, &out.MerchantState,
			&out.HasMerchant, &out.Order, &out.Account); err != nil {
			return paymentRow{}, err
		}

		out.KindMeaning = billing.Kind(out.Kind).Describe()

		// A receipt outlives the card it charged, so the number is only shown
		// while the card is there; masking a number that is gone would invent
		// one.
		out.Card = "—"
		if card != "" {
			out.Card = maskNumber(card)
		}

		if out.Side == sideReceipt {
			out.Href = fmt.Sprintf("/receipts/%d", out.ID)
			out.StateLabel = receiptStateLabel(out.State)
			out.Failed = out.State == 50
			out.Open = out.State != 4 && out.State != 50
			out.Way = wayTopUp
			if out.Payout {
				out.Way = wayWithdraw
			}
		} else {
			out.Href = fmt.Sprintf("/transactions/%d", out.ID)
			out.StateLabel = stateLabel(out.State)
			out.Failed = out.State < 0
			out.Open = out.State == 1
			out.Way = wayBilled
		}

		if out.HasMerchant {
			out.MerchantLabel = stateLabel(out.MerchantState)
		}

		return out, nil
	})
}

func (a *app) showPayments(w http.ResponseWriter, r *http.Request, user string) {
	a.renderPayments(w, r, user, "")
}

// renderPayments draws the screen with a message on it, which is how an edit
// that failed reports itself: the operator stays where they were, with the list
// still in front of them.
func (a *app) renderPayments(w http.ResponseWriter, r *http.Request, user, message string) {
	filter := paymentFilter{
		Sandbox: r.URL.Query().Get("sandbox"),
		Way:     r.URL.Query().Get("way"),
		State:   r.URL.Query().Get("state"),
		Query:   strings.TrimSpace(r.URL.Query().Get("q")),
		Sort:    sortOrder(r),
	}

	page := pageOf(r)

	payments, err := a.store.Payments(r.Context(), filter, page)
	if err != nil {
		a.fail(w, "list payments", err)
		return
	}

	payments, pager := paginate(payments, page, r)

	// There is no live panel here. A payment that is still moving is a state
	// like any other and the filter asks for it — state=open — so a second,
	// self-refreshing copy of the same rows above the list only made the screen
	// jump under the reader.

	sandboxes, err := a.store.Sandboxes(r.Context(), a.cfg.GatewayBaseURL)
	if err != nil {
		a.fail(w, "list sandboxes", err)
		return
	}

	a.render(w, "payments", view{
		Title: "Payments", Nav: "payments", User: user, Error: message,
		Notice:   notice(r),
		Payments: payments, Sandboxes: sandboxes, PaymentFilter: filter,
		States: paymentStates(), Results: paymentWays(), Sorts: listSorts(),
		Page: pager,
	})
}
