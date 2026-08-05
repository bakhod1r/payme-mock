package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// cardActivity is what a card has been through: how much of the traffic named
// it, and what came of the payments made with it.
//
// The first question anyone opens a card with is whether the integration reached
// it at all — a run that failed and a run that never called look identical on
// the card itself, since neither moves the balance. Counting the calls that
// named its token separates the two.
type cardActivity struct {
	// Requests is how many logged calls carried this card's token, and Last is
	// when the newest of them arrived.
	Requests int
	Last     string
	// Payments and Payouts are receipts made with the card, whichever way the
	// money went.
	Payments int
	Payouts  int
	// Paid and Cancelled are how those receipts ended. A receipt still on its
	// way is counted in neither, which is why they need not add up.
	Paid      int
	Cancelled int
	// Charged is what was taken off the card by settled payments, and PaidOut
	// what settled payouts put on it. Only settled receipts count: an amount
	// asked for and never paid moved nothing.
	Charged int64
	PaidOut int64
}

// Any reports a card the stand has seen used, which is what decides whether the
// panel is worth drawing at all.
func (a cardActivity) Any() bool { return a.Requests > 0 || a.Payments > 0 || a.Payouts > 0 }

// cardReceiptLimit caps the payment list on a card's page. It is a card's
// recent history, not its archive; the receipts screen holds the rest.
const cardReceiptLimit = 20

// ReceiptsForCard lists the receipts made with one card, newest first.
func (s *store) ReceiptsForCard(ctx context.Context, id int64, limit int) ([]receiptRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+receiptColumns+receiptSource+`
		WHERE r.card_id = $1
		ORDER BY r.id DESC
		LIMIT $2`, id, limit)
	if err != nil {
		return nil, fmt.Errorf("select card receipts: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (receiptRow, error) {
		return scanReceipt(row)
	})
}

// cardCashbox is one of the merchant's registers as a card's page shows it:
// whether this card may be charged through it, and how much of the card's
// traffic went through it.
type cardCashbox struct {
	SandboxID int64
	Slug      string
	Kind      string
	// Blocked reports a register the merchant stopped taking this card at.
	Blocked bool
	// Home marks the register the card was added to, which is only where it
	// came from: a merchant's card is chargeable through all of them.
	Home bool
	// Payments counts the receipts made with this card through that register.
	Payments int
	// Token is the string this register was handed for the card. Each register
	// gets its own, so the page shows them per row rather than once at the top:
	// the token an integration stored is the token of the till it asked at.
	Token string
}

// CardCashboxes lists the registers a card can be used through: every one of
// its merchant's, with the ones it has been taken off marked.
func (s *store) CardCashboxes(ctx context.Context, id int64) ([]cardCashbox, error) {
	rows, err := s.pool.Query(ctx, `
		WITH card AS (
			SELECT c.id, c.sandbox_id, c.merchant_key FROM mock.cards c WHERE c.id = $1
		)
		SELECT s.id, s.slug, s.kind,
		       EXISTS (SELECT 1 FROM mock.card_cashbox_blocks b
		                WHERE b.card_id = card.id AND b.sandbox_id = s.id),
		       s.id = card.sandbox_id,
		       (SELECT count(*) FROM mock.receipts r
		         WHERE r.card_id = card.id AND r.sandbox_id = s.id),
		       coalesce((SELECT t.token FROM mock.card_tokens t
		                  WHERE t.card_id = card.id AND t.sandbox_id = s.id), '')
		FROM control.sandboxes s, card
		WHERE NOT s.archived
		  AND coalesce(s.merchant_group, 'sandbox:' || s.id) = card.merchant_key
		ORDER BY s.id`, id)
	if err != nil {
		return nil, fmt.Errorf("select card cashboxes: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (cardCashbox, error) {
		var out cardCashbox
		err := row.Scan(&out.SandboxID, &out.Slug, &out.Kind, &out.Blocked,
			&out.Home, &out.Payments, &out.Token)
		return out, err
	})
}

// SetCardCashbox takes a card off one register, or puts it back.
//
// It is not the same as blocking the card: a blocked card refuses everywhere,
// the way a bank stops it, while this is the merchant declining it at one till
// and taking it at the others.
func (s *store) SetCardCashbox(ctx context.Context, cardID, sandboxID int64, blocked bool) error {
	if blocked {
		_, err := s.pool.Exec(ctx, `
			INSERT INTO mock.card_cashbox_blocks (card_id, sandbox_id)
			VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, cardID, sandboxID)
		if err != nil {
			return fmt.Errorf("take card off cashbox: %w", err)
		}
		return nil
	}

	if _, err := s.pool.Exec(ctx, `
		DELETE FROM mock.card_cashbox_blocks
		WHERE card_id = $1 AND sandbox_id = $2`, cardID, sandboxID); err != nil {
		return fmt.Errorf("put card back on cashbox: %w", err)
	}

	return nil
}

// CardActivity counts what has happened to one card.
//
// The traffic count matches on the token inside the request body, because the
// log keeps no card column: a call names a card by the token it sends, and that
// token is unique enough that a substring match cannot collect another card's
// calls. A card whose token never appears has been reached by nobody, which is
// the finding.
func (s *store) CardActivity(ctx context.Context, id int64) (cardActivity, error) {
	var out cardActivity

	err := s.pool.QueryRow(ctx, `
		WITH card AS (
			SELECT id, sandbox_id, token FROM mock.cards WHERE id = $1
		), calls AS (
			SELECT count(*) AS seen, max(l.at) AS last
			FROM control.request_log l, card
			WHERE l.sandbox_id = card.sandbox_id
			  AND l.request_body::text LIKE '%' || card.token || '%'
		), paid AS (
			SELECT
				count(*) FILTER (WHERE NOT r.payout)                      AS payments,
				count(*) FILTER (WHERE r.payout)                          AS payouts,
				count(*) FILTER (WHERE r.state = 4)                       AS settled,
				count(*) FILTER (WHERE r.state = 50)                      AS cancelled,
				coalesce(sum(r.amount) FILTER (
					WHERE r.state = 4 AND NOT r.payout), 0)               AS charged,
				coalesce(sum(r.amount) FILTER (
					WHERE r.state = 4 AND r.payout), 0)                   AS paid_out
			FROM mock.receipts r, card
			WHERE r.card_id = card.id
		)
		SELECT calls.seen, coalesce(`+stamp("calls.last")+`, ''),
		       paid.payments, paid.payouts, paid.settled, paid.cancelled,
		       paid.charged, paid.paid_out
		FROM calls, paid`, id).
		Scan(&out.Requests, &out.Last, &out.Payments, &out.Payouts, &out.Paid,
			&out.Cancelled, &out.Charged, &out.PaidOut)
	if err != nil {
		return cardActivity{}, fmt.Errorf("count card activity: %w", err)
	}

	if out.Last == "" {
		out.Last = "—"
	}

	return out, nil
}
