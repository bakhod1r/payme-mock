package main

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A screen that has never had a row on it is a screen nobody has read. These
// put one of everything on a stand and then read every list that shows them, so
// what is under test is the mapping — the labels, the dashes for what is not
// set, the "how long ago" — rather than an empty page rendering.

// busyStand is a stand with one of everything: an order, payments in each state
// worth showing, a card, receipts both ways, traffic that worked and traffic
// that did not, and a balance that has been moved.
func busyStand(t *testing.T) (*stand, sandboxRow) {
	t.Helper()

	s := newStand(t)
	sandbox := s.newSandbox(t, "busy", "topup", 100000)
	ctx := context.Background()

	// One order may hold one active payment at a time, so the three states are
	// spread over three orders — which is what a stand that has been used looks
	// like anyway.
	order := s.addOrder(t, sandbox, 250000)
	s.addTransaction(t, sandbox, order, "txn-created", 1)
	s.addTransaction(t, sandbox, s.addOrder(t, sandbox, 250000), "txn-performed", 2)
	s.addTransaction(t, sandbox, s.addOrder(t, sandbox, 250000), "txn-cancelled", -1)

	// A payout has no order and no reason, which is the pair of nulls every
	// mapping has to read as "not set" rather than fall over on.
	_, err := s.store.pool.Exec(ctx, `
		INSERT INTO merchant.transactions
			(sandbox_id, payme_id, order_id, account_id, account, amount, state,
			 payme_time, create_time)
		VALUES ($1, 'txn-payout', NULL, $2, '{"order_id":"none"}'::jsonb, 1000, 1,
		        1750000000000, 1750000000000)`, sandbox.ID, sandbox.AccountID)
	require.NoError(t, err)

	card := s.riggedCard(t, sandbox, uzcard, "success")
	s.addReceipt(t, sandbox, "receipt-paid", 120000, 4, false, &card)
	s.addReceipt(t, sandbox, "receipt-payout", 90000, 4, true, &card)
	s.addReceipt(t, sandbox, "receipt-open", 50000, 1, false, nil)

	failed := 500
	code := -31008
	s.addTrafficEntry(t, sandbox, "CheckPerformTransaction", &failed, &code)
	ok := 200
	s.addTrafficEntry(t, sandbox, "CreateTransaction", &ok, nil)

	_, err = s.store.ChangeBalance(ctx, sandbox.AccountID, "add", 50000)
	require.NoError(t, err)

	return s, sandbox
}

// Every list, read with rows on it. What is asserted is what an operator reads
// off the screen: which payment it was, what it did, and how it ended.
func TestE2EEveryListReadsItsRows(t *testing.T) {
	s, _ := busyStand(t)

	pages := map[string][]string{
		"/":             {"busy"},
		"/dashboard":    {"busy", "topup"},
		"/sandboxes":    {"busy"},
		"/payments":     {"receipt-paid", "txn-performed"},
		"/transactions": {"txn-performed"},
		"/traffic":      {"CheckPerformTransaction", "CreateTransaction"},
		"/cards":        {"860006******6311"},
		"/rules":        {"payme-mock"},
	}

	for path, wants := range pages {
		t.Run(path, func(t *testing.T) {
			w := s.get(t, path)
			require.Equal(t, http.StatusOK, w.Code)

			for _, want := range wants {
				assert.Contains(t, w.Body.String(), want)
			}
		})
	}
}

// The stand's own page is where an operator does most of their work, so it
// carries every list that belongs to one stand: its payer, its orders, its
// payments, what its balance has done and who may reach it.
func TestE2ETheStandsPageCarriesEverythingAboutIt(t *testing.T) {
	s, sandbox := busyStand(t)

	w := s.get(t, "/sandboxes/"+rowID(sandbox.ID))
	require.Equal(t, http.StatusOK, w.Code)

	body := w.Body.String()
	for _, want := range []string{
		"busy",     // the stand itself
		"2 500.00", // the order it holds, in the sum an operator reads
		"1 500.00", // the balance, after the move above
		"Reachable from",
	} {
		assert.Contains(t, body, want)
	}
}

// A payment and a receipt each have a page of their own, which is where the
// whole of one is read: both sides of it, and the raw bodies underneath.
func TestE2EEveryRowHasAPageOfItsOwn(t *testing.T) {
	s, sandbox := busyStand(t)
	ctx := context.Background()

	var (
		transaction int64
		receipt     int64
		traffic     int64
		card        int64
	)
	require.NoError(t, s.store.pool.QueryRow(ctx,
		`SELECT id FROM merchant.transactions WHERE payme_id = 'txn-performed'`).Scan(&transaction))
	require.NoError(t, s.store.pool.QueryRow(ctx,
		`SELECT id FROM mock.receipts WHERE receipt_id = 'receipt-paid'`).Scan(&receipt))
	require.NoError(t, s.store.pool.QueryRow(ctx,
		`SELECT id FROM control.request_log ORDER BY id LIMIT 1`).Scan(&traffic))
	require.NoError(t, s.store.pool.QueryRow(ctx,
		`SELECT id FROM mock.cards WHERE sandbox_id = $1`, sandbox.ID).Scan(&card))

	pages := map[string]string{
		"/transactions/" + rowID(transaction): "txn-performed",
		"/receipts/" + rowID(receipt):         "receipt-paid",
		"/traffic/" + rowID(traffic):          "CheckPerformTransaction",
		"/cards/" + rowID(card):               "860006******6311",
	}

	for path, want := range pages {
		t.Run(path, func(t *testing.T) {
			w := s.get(t, path)

			require.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, w.Body.String(), want)
		})
	}
}

// The live panel is the one screen that answers "what is happening right now",
// so it reads the same rows through a window of minutes rather than a page.
func TestE2ELivePanelsReadTheirRows(t *testing.T) {
	s, sandbox := busyStand(t)
	ctx := context.Background()

	payments, err := s.store.LiveTransactions(ctx, "", 60, 10)
	require.NoError(t, err)
	require.NotEmpty(t, payments)

	var payout, performed liveTransactionRow
	for _, row := range payments {
		switch row.PaymeID {
		case "txn-payout":
			payout = row
		case "txn-performed":
			performed = row
		}
	}

	assert.Equal(t, "—", payout.OrderID, "a payout settles no order")
	assert.Equal(t, "—", payout.Reason, "nothing cancelled it")
	assert.True(t, payout.Open)
	assert.NotEmpty(t, payout.Ago)

	assert.NotEqual(t, "—", performed.OrderID)
	assert.False(t, performed.Open)

	cards, err := s.store.LiveCards(ctx, sandbox.Slug, 60, 10)
	require.NoError(t, err)
	require.NotEmpty(t, cards)
	assert.Equal(t, "860006******6311", cards[0].Mask)

	// Narrowed to a stand that holds nothing, both answer nothing rather than
	// everything: a window is not a filter that fails open.
	empty, err := s.store.LiveTransactions(ctx, "nobody", 60, 10)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// The dashboard is arithmetic over the same rows, and the arithmetic is what
// makes it worth reading: money in, money out, and what is still in flight.
func TestE2EDashboardCountsWhatHappened(t *testing.T) {
	s, _ := busyStand(t)

	board, err := s.store.Dashboard(context.Background())
	require.NoError(t, err)

	assert.NotEmpty(t, board.Cashboxes)
	assert.NotZero(t, board.Endings.Total)
	assert.NotZero(t, board.Cards.Total)
	assert.NotZero(t, board.Traffic.Calls)
	assert.NotEmpty(t, board.Failures, "a call that failed is what the board is opened for")
}

// rowID keeps the addresses in these tests readable.
func rowID(id int64) string { return strconv.FormatInt(id, 10) }
