package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// "What is happening right now" is not a screen of its own: a payment in
// progress belongs above the payments, and a card just added belongs above the
// cards. A separate live screen made an operator watch one place and act in
// another, so each list carries its own live panel instead.
//
// "Right now" is a window rather than a state, because the interesting rows are
// not only the ones still open: a payment that failed a minute ago is part of
// what just happened, and a screen that hid it would make the failure look like
// nothing at all.

// defaultWindowMinutes is how far back "right now" reaches by default.
const defaultWindowMinutes = 3

// windowChoices are the windows the panels offer. Three minutes is the default
// because that is about how long a rehearsed payment takes to play out; the
// longer ones are for reading back what happened while nobody was looking.
func windowChoices() []int { return []int{1, 3, 5, 15, 60} }

// liveWindow reads how far back the screen should look. Anything unrecognized
// falls back to the default rather than failing: a hand-edited URL is not
// worth an error screen.
func liveWindow(r *http.Request) int {
	raw := r.URL.Query().Get("window")
	if raw == "" {
		return defaultWindowMinutes
	}

	minutes, err := strconv.Atoi(raw)
	if err != nil || minutes <= 0 {
		return defaultWindowMinutes
	}

	// A window nobody can choose from the screen is still honoured, so a link
	// pasted between people keeps working, but an absurd one is capped.
	if minutes > 24*60 {
		return 24 * 60
	}

	return minutes
}

// liveCardRow is something that just happened to a card: it was added, or an
// OTP was sent to its owner.
type liveCardRow struct {
	cardRow
	// Ago is how long ago it happened, in words, since a panel that refreshes
	// itself is read as "how fresh" rather than "when".
	Ago string
	// Event says which of the two happened, because "a card appeared" and "a
	// code went out to a card that was already there" are different moments in
	// a rehearsal and the row would otherwise look the same.
	Event string
	// Waiting marks a card that has been sent a code and not yet used it,
	// which is the one an operator is about to type the OTP for.
	Waiting bool
}

// The two things that happen to a card while someone is watching.
const (
	eventCardAdded = "added"
	eventCodeSent  = "OTP sent"
)

// liveTransactionRow is a payment that moved inside the window, or one that is
// still open however long ago it started.
type liveTransactionRow struct {
	transactionRow
	Ago string
	// Open reports a payment that has neither settled nor been cancelled, which
	// is the one still holding an order.
	Open bool
}

// LiveTransactions returns the payments that moved inside the window, together
// with every payment still open whatever its age.
//
// The open ones are included regardless because they are the definition of
// "in progress": a payment created two minutes ago and never performed is
// exactly what someone watching this screen is looking for.
// The stand filter is honoured here as well as on the list below it: a screen
// narrowed to one stand that still showed another stand's payment in progress
// would be answering a question nobody asked.
func (s *store) LiveTransactions(ctx context.Context, sandbox string, minutes, limit int) ([]liveTransactionRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, s.slug, t.payme_id, t.order_id, t.account_id, t.account::text,
		       t.amount, t.state, t.reason, t.payme_time, t.create_time,
		       t.perform_time, t.cancel_time, coalesce(t.receivers::text, ''),
		       `+stamp("t.created_at")+`,
		       `+stamp("t.updated_at")+`,
		       extract(epoch FROM now() - t.updated_at)::bigint
		FROM merchant.transactions t
		JOIN control.sandboxes s ON s.id = t.sandbox_id
		WHERE (t.updated_at > now() - make_interval(mins => $1) OR t.state = 1)
		  AND ($3 = '' OR s.slug = $3)
		ORDER BY t.updated_at DESC
		LIMIT $2`, minutes, limit, sandbox)
	if err != nil {
		return nil, fmt.Errorf("select live transactions: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (liveTransactionRow, error) {
		var (
			out     liveTransactionRow
			orderID *int64
			reason  *int16
			ago     int64
		)

		if err := row.Scan(&out.ID, &out.Sandbox, &out.PaymeID, &orderID,
			&out.AccountID, &out.Account, &out.Amount, &out.State, &reason,
			&out.PaymeTime, &out.CreateTime, &out.PerformTime, &out.CancelTime,
			&out.Receivers, &out.CreatedAt, &out.UpdatedAt, &ago); err != nil {
			return liveTransactionRow{}, err
		}

		out.OrderID = "—"
		if orderID != nil {
			out.OrderID = fmt.Sprint(*orderID)
		}
		out.Reason = "—"
		if reason != nil {
			out.Reason = fmt.Sprint(*reason)
		}
		out.StateLabel = stateLabel(out.State)
		out.Failed = out.State < 0
		out.Open = out.State == 1
		out.Ago = humanAgo(ago)

		return out, nil
	})
}

// LiveCards returns what has just happened to the stand's cards: a card added,
// or a verification code sent to one.
//
// The OTP matters as much as the card: an integration that has asked for a
// code is waiting for someone to type it, and that wait is the most live thing
// on the stand.
func (s *store) LiveCards(ctx context.Context, sandbox string, minutes, limit int) ([]liveCardRow, error) {
	rows, err := s.pool.Query(ctx, cardSelect+`
		WHERE ($3 = '' OR s.slug = $3)
		  AND (c.created_at > now() - make_interval(mins => $1)
		   OR (c.verify_code_sent_at > 0
		       AND c.verify_code_sent_at >
		           extract(epoch FROM now() - make_interval(mins => $1))::bigint * 1000))
		ORDER BY greatest(c.verify_code_sent_at,
		                  extract(epoch FROM c.created_at)::bigint * 1000) DESC
		LIMIT $2`, minutes, limit, sandbox)
	if err != nil {
		return nil, fmt.Errorf("select live cards: %w", err)
	}

	// The card columns are scanned by the shared helper, so the moment is read
	// back from what it already returns rather than by widening the select
	// every list shares.
	cards, err := pgx.CollectRows(rows, scanCardRow)
	if err != nil {
		return nil, err
	}

	out := make([]liveCardRow, 0, len(cards))
	for _, card := range cards {
		out = append(out, liveCard(card, minutes))
	}

	return out, nil
}

// liveCard decides which moment a row stands for. A code sent after the card
// was added is the later moment and the one worth reporting; anything else is
// the card appearing.
func liveCard(card cardRow, minutes int) liveCardRow {
	out := liveCardRow{cardRow: card, Event: eventCardAdded, Ago: agoSince(card.Created)}

	if card.CodeSentAt <= 0 {
		return out
	}

	sent := time.UnixMilli(card.CodeSentAt)
	if sent.Before(time.Now().Add(-time.Duration(minutes) * time.Minute)) {
		return out
	}

	out.Event = eventCodeSent
	out.Ago = humanAgo(int64(time.Since(sent).Seconds()))
	// A card that took the code is no longer waiting for it, however recently
	// it was sent.
	out.Waiting = !card.Verify

	return out
}

// humanAgo says how long ago something happened in the shortest words that
// stay exact.
func humanAgo(seconds int64) string {
	switch {
	case seconds < 0:
		return "just now"
	case seconds < 60:
		return fmt.Sprintf("%ds ago", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm ago", seconds/60)
	default:
		return fmt.Sprintf("%dh ago", seconds/3600)
	}
}

// agoSince turns a stored timestamp into the same words. The column is already
// formatted for the screen, so it is parsed back rather than selected twice.
func agoSince(stamp string) string {
	at, err := time.Parse("2006-01-02 15:04:05", stamp)
	if err != nil {
		return ""
	}

	return humanAgo(int64(time.Since(at).Seconds()))
}
