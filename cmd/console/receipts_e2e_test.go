package main

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The receipts screen reads rows the console never writes: the paymemock binary
// does, from the Subscribe API. These insert them directly, which is the state
// the screen exists to show.

// addCard writes a saved card and returns its identifier.
func (s *stand) addCard(t *testing.T, sandbox sandboxRow, number string) int64 {
	t.Helper()

	var id int64
	err := s.store.pool.QueryRow(context.Background(), `
		INSERT INTO mock.cards (sandbox_id, token, number_full, expire, verify)
		VALUES ($1, $2, $3, '0399', TRUE)
		RETURNING id`, sandbox.ID, "token-"+number, number).Scan(&id)
	require.NoError(t, err)

	return id
}

// addReceipt writes one receipt in the state a Subscribe call would leave it.
func (s *stand) addReceipt(t *testing.T, sandbox sandboxRow, receiptID string,
	amount int64, state int, payout bool, cardID *int64,
) int64 {
	t.Helper()

	var id int64
	err := s.store.pool.QueryRow(context.Background(), `
		INSERT INTO mock.receipts
			(sandbox_id, receipt_id, merchant_id, amount, state, type, account,
			 detail, payer, meta, description, processing_id, card_id, card_system,
			 commission, create_time, pay_time, payout)
		VALUES ($1, $2, $3, $4, $5::smallint, 1, $6::jsonb, '{"discount":0}'::jsonb,
		        '{"phone":"998901112233"}'::jsonb, '{"source":"subscribe"}'::jsonb,
		        'rehearsal', 'proc-1', $7, 'uzcard', 700, 1750000000000,
		        1750000000500, $8)
		RETURNING id`, sandbox.ID, receiptID, sandbox.MerchantID, amount, state,
		`{"order_id": "`+receiptID+`-order", "login": "998901112233"}`,
		cardID, payout).Scan(&id)
	require.NoError(t, err)

	return id
}

// A charge against a saved card never reaches the merchant side of the stand,
// so the transactions screen stays empty while receipts pile up. The receipts
// screen is the only place that traffic is a list rather than a request log.
func TestE2EReceiptListShowsWhatTheSubscribeAPIWrote(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "receipts-list", "topup", 100000)
	card := s.addCard(t, sandbox, "8600490000006478")
	s.addReceipt(t, sandbox, "receipt-paid", 500000, 4, false, &card)

	// The list is the merged one: both sides of a payment on one screen.
	w := s.get(t, "/payments")

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "receipt-paid")
	assert.Contains(t, body, "860049******6478")
	assert.Contains(t, body, "4 · paid")
	assert.Contains(t, body, "top up", "a charged card is a top-up in the words the integration uses")
	assert.Contains(t, body, "1 payment(s)")
}

func TestE2EReceiptPageShowsEverythingTheProtocolCarried(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "receipts-page", "topup", 100000)
	card := s.addCard(t, sandbox, "8600490000006478")
	id := s.addReceipt(t, sandbox, "receipt-detail", 500000, 4, false, &card)

	w := s.get(t, "/receipts/"+strconv.FormatInt(id, 10))

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "receipt-detail")
	assert.Contains(t, body, sandbox.MerchantID)
	assert.Contains(t, body, "receipt-detail-order")
	assert.Contains(t, body, "998901112233")
	assert.Contains(t, body, "proc-1")
	assert.Contains(t, body, "uzcard")
}

// A payout and a payment are the same table and different questions, so the
// screen names the direction rather than leaving the reader to infer it.
func TestE2EReceiptDirectionSeparatesPayoutsFromPayments(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "receipts-kind", "topup", 100000)
	card := s.addCard(t, sandbox, "8600490000006478")
	s.addReceipt(t, sandbox, "receipt-charge", 500000, 4, false, &card)
	s.addReceipt(t, sandbox, "receipt-payout", 300000, 4, true, &card)

	payouts := s.get(t, "/payments?way=payout").Body.String()
	assert.Contains(t, payouts, "receipt-payout")
	assert.Contains(t, payouts, "withdraw")
	assert.NotContains(t, payouts, "receipt-charge")

	payments := s.get(t, "/payments?way=payment").Body.String()
	assert.Contains(t, payments, "receipt-charge")
	assert.NotContains(t, payments, "receipt-payout")
}

func TestE2EReceiptFilterNarrowsTheList(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "receipts-filter", "topup", 100000)
	other := s.newSandbox(t, "receipts-other", "topup", 100000)
	card := s.addCard(t, sandbox, "8600490000006478")
	s.addReceipt(t, sandbox, "receipt-paid", 500000, 4, false, &card)
	s.addReceipt(t, sandbox, "receipt-open", 100000, 1, false, &card)
	s.addReceipt(t, sandbox, "receipt-gone", 200000, 50, false, &card)
	s.addReceipt(t, other, "receipt-elsewhere", 900000, 4, false, nil)

	for _, tt := range []struct {
		name    string
		query   string
		present []string
		absent  []string
	}{
		{
			name:    "by stand",
			query:   "?sandbox=receipts-filter",
			present: []string{"receipt-paid", "receipt-open"},
			absent:  []string{"receipt-elsewhere"},
		},
		{
			name:    "open, whatever state that is",
			query:   "?state=open",
			present: []string{"receipt-open"},
			absent:  []string{"receipt-paid", "receipt-gone"},
		},
		{
			name:    "cancelled",
			query:   "?state=failed",
			present: []string{"receipt-gone"},
			absent:  []string{"receipt-paid", "receipt-open"},
		},
		{
			name:    "by the card it charged",
			query:   "?q=6478",
			present: []string{"receipt-paid"},
			absent:  []string{"receipt-elsewhere"},
		},
		{
			name:    "by what the account carried",
			query:   "?q=receipt-open-order",
			present: []string{"receipt-open"},
			absent:  []string{"receipt-paid"},
		},
		{
			name:    "largest first",
			query:   "?sort=largest",
			present: []string{"receipt-elsewhere"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := s.get(t, "/payments"+tt.query)

			require.Equal(t, http.StatusOK, w.Code)
			body := w.Body.String()
			for _, want := range tt.present {
				assert.Contains(t, body, want)
			}
			for _, unwanted := range tt.absent {
				assert.NotContains(t, body, unwanted)
			}
		})
	}
}

// A receipt outlives the card it charged, and the row is what a support
// question is about, so it is still readable with the card gone.
func TestE2EReceiptSurvivesTheCardItCharged(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "receipts-orphan", "topup", 100000)
	card := s.addCard(t, sandbox, "8600490000006478")
	id := s.addReceipt(t, sandbox, "receipt-orphan", 500000, 4, false, &card)

	_, err := s.store.pool.Exec(context.Background(),
		`DELETE FROM mock.cards WHERE id = $1`, card)
	require.NoError(t, err)

	w := s.get(t, "/receipts/"+strconv.FormatInt(id, 10))

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "receipt-orphan")
	assert.NotContains(t, w.Body.String(), "860049")
}

func TestE2EReceiptPageIsNotFoundForAnUnknownID(t *testing.T) {
	s := newStand(t)

	assert.Equal(t, http.StatusNotFound, s.get(t, "/receipts/909090").Code)
	assert.Equal(t, http.StatusNotFound, s.get(t, "/receipts/not-a-number").Code)
}

func TestE2EReceiptListIsEmptyUntilAClientCalls(t *testing.T) {
	s := newStand(t)
	s.newSandbox(t, "receipts-empty", "topup", 100000)

	w := s.get(t, "/receipts")

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Nothing matches")
}
