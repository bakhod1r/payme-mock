package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The screens that read payments and traffic need rows the console never
// writes: the merchant binary does. These insert them directly, which is the
// state those screens exist to show.

// addOrder writes an order and returns its identifier.
func (s *stand) addOrder(t *testing.T, sandbox sandboxRow, amount int64) int64 {
	t.Helper()

	var id int64
	err := s.store.pool.QueryRow(context.Background(), `
		INSERT INTO merchant.orders (sandbox_id, account_id, amount, status, description)
		VALUES ($1, $2, $3, 'new', 'rehearsal')
		RETURNING id`, sandbox.ID, sandbox.AccountID, amount).Scan(&id)
	require.NoError(t, err)

	return id
}

// addTransaction writes a payment in the state a merchant call would leave it.
func (s *stand) addTransaction(t *testing.T, sandbox sandboxRow, orderID int64, paymeID string, state int) int64 {
	t.Helper()

	var id int64
	err := s.store.pool.QueryRow(context.Background(), `
		INSERT INTO merchant.transactions
			(sandbox_id, payme_id, order_id, account_id, account, amount, state,
			 payme_time, create_time, perform_time, receivers)
		VALUES ($1, $2, $3, $4, $5::jsonb, 500000, $6::smallint,
		        1750000000000, 1750000000000, 0, '[]'::jsonb)
		RETURNING id`, sandbox.ID, paymeID, orderID, sandbox.AccountID,
		`{"order_id": "`+strconv.FormatInt(orderID, 10)+`"}`, state).Scan(&id)
	require.NoError(t, err)

	return id
}

// addTrafficEntry writes one line of the request log.
func (s *stand) addTrafficEntry(t *testing.T, sandbox sandboxRow, method string, status, errorCode *int) int64 {
	t.Helper()

	var id int64
	err := s.store.pool.QueryRow(context.Background(), `
		INSERT INTO control.request_log
			(sandbox_id, service, direction, method, http_status, request_body,
			 response_body, duration_ms, error_code, remote_addr)
		VALUES ($1, 'merchant', 'in', $2, $3, $4::jsonb, $5::jsonb, 12, $6, '127.0.0.1:5000')
		RETURNING id`, sandbox.ID, method, status,
		`{"method":"`+method+`"}`, `{"result":{"allow":true}}`, errorCode).Scan(&id)
	require.NoError(t, err)

	return id
}

func TestE2ETransactionPageShowsEverythingTheProtocolCarried(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "payments", "topup", 100000)
	order := s.addOrder(t, sandbox, 500000)
	id := s.addTransaction(t, sandbox, order, "5305e3ba1f0b2c3d4e5f6071", 2)

	body := s.get(t, "/transactions/"+strconv.FormatInt(id, 10)).Body.String()

	assert.Contains(t, body, "5305e3ba1f0b2c3d4e5f6071")
	assert.Contains(t, body, "5 000.00")
	assert.Contains(t, body, "performed")
}

func TestE2ETransactionListShowsThePayment(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "listed", "topup", 0)
	order := s.addOrder(t, sandbox, 500000)
	s.addTransaction(t, sandbox, order, "aaaa1111bbbb2222cccc3333", 1)

	body := s.get(t, "/transactions").Body.String()

	assert.Contains(t, body, "aaaa1111bbbb2222cccc3333")
	assert.Contains(t, body, "created")
}

// The filter is SQL, so each way of narrowing is checked against real rows.
func TestE2ETransactionFilterNarrowsTheList(t *testing.T) {
	s := newStand(t)
	first := s.newSandbox(t, "one", "topup", 0)
	second := s.newSandbox(t, "two", "topup", 0)

	s.addTransaction(t, first, s.addOrder(t, first, 500000), "1111111111111111aaaaaaaa", 1)
	s.addTransaction(t, second, s.addOrder(t, second, 500000), "2222222222222222bbbbbbbb", 2)

	tests := []struct {
		name     string
		query    string
		contains string
		absent   string
	}{
		{"by sandbox", "?sandbox=one", "1111111111111111aaaaaaaa", "2222222222222222bbbbbbbb"},
		// The merged screen asks how a payment ended rather than for a state
		// number: the two sides it lists number their states differently.
		{"by how it ended", "?state=done", "2222222222222222bbbbbbbb", "1111111111111111aaaaaaaa"},
		{"still in progress", "?state=open", "1111111111111111aaaaaaaa", "2222222222222222bbbbbbbb"},
		{"by payme id", "?q=1111111111111111", "1111111111111111aaaaaaaa", "2222222222222222bbbbbbbb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := s.get(t, "/payments"+tt.query).Body.String()

			assert.Contains(t, body, tt.contains)
			assert.NotContains(t, body, tt.absent)
		})
	}
}

func TestE2ETransactionCanBeFilteredByOrder(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "by-order", "topup", 0)
	order := s.addOrder(t, sandbox, 500000)
	s.addTransaction(t, sandbox, order, "dddd4444eeee5555ffff6666", 1)

	body := s.get(t, "/payments?q="+strconv.FormatInt(order, 10)).Body.String()

	assert.Contains(t, body, "dddd4444eeee5555ffff6666")
}

// Forcing a state rehearses what happens after a payment was settled or
// reversed upstream; the balance is deliberately left where it is.
func TestE2EForceTransactionState(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "forced", "topup", 0)
	order := s.addOrder(t, sandbox, 500000)
	id := strconv.FormatInt(s.addTransaction(t, sandbox, order, "9999888877776666aaaa5555", 1), 10)

	_, message := location(t, s.post(t, "/transactions/"+id, url.Values{
		"state": {"-2"},
		"back":  {"/transactions/" + id},
	}))
	assert.Contains(t, message, "state changed")

	body := s.get(t, "/transactions/"+id).Body.String()
	assert.Contains(t, body, "cancelled after perform")
}

func TestE2EForceTransactionStateRejectsAnUnknownState(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "bad-state", "topup", 0)
	order := s.addOrder(t, sandbox, 500000)
	id := strconv.FormatInt(s.addTransaction(t, sandbox, order, "5555444433332222aaaa1111", 1), 10)

	w := s.post(t, "/transactions/"+id, url.Values{"state": {"7"}})

	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"the schema refuses a state the protocol does not define")
}

func TestE2EDeleteTransaction(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "deletable", "topup", 0)
	order := s.addOrder(t, sandbox, 500000)
	id := strconv.FormatInt(s.addTransaction(t, sandbox, order, "7777666655554444aaaa3333", 1), 10)

	_, message := location(t, s.post(t, "/transactions/"+id+"/delete", nil))
	assert.Contains(t, message, "Transaction deleted")

	assert.Equal(t, http.StatusNotFound, s.get(t, "/transactions/"+id).Code)
}

func TestE2EForcingAVanishedTransactionReportsIt(t *testing.T) {
	s := newStand(t)

	assert.Contains(t, s.post(t, "/transactions/999999", url.Values{"state": {"1"}}).Body.String(), "gone")
	assert.Contains(t, s.post(t, "/transactions/999999/delete", nil).Body.String(), "gone")
}

// An order a payment in flight holds may not be deleted: the payment would be left
// pointing at nothing Payme was ever told about.
func TestE2EDeletingAHeldOrderIsRefused(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "held", "topup", 0)
	order := s.addOrder(t, sandbox, 500000)
	s.addTransaction(t, sandbox, order, "6666555544443333aaaa2222", 1)

	// The order is read on the stand that holds it, and a payment holding it
	// is marked there.
	body := s.get(t, "/sandboxes/"+strconv.FormatInt(sandbox.ID, 10)).Body.String()
	assert.Contains(t, body, "payment in flight")
}

func TestE2ETrafficEntryPageShowsBothBodies(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "logged", "topup", 0)
	status := 200
	id := s.addTrafficEntry(t, sandbox, "CheckPerformTransaction", &status, nil)

	body := s.get(t, "/traffic/"+strconv.FormatInt(id, 10)).Body.String()

	assert.Contains(t, body, "CheckPerformTransaction")
	assert.Contains(t, body, "allow")
	assert.Contains(t, body, "127.0.0.1:5000")
}

// A protocol error is reported inside a 200 response, so the row is a failure
// even when the status says otherwise.
func TestE2ETrafficEntryWithAProtocolErrorReadsAsFailed(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "failed", "topup", 0)
	status := 200
	code := -31008
	id := s.addTrafficEntry(t, sandbox, "PerformTransaction", &status, &code)

	body := s.get(t, "/traffic/"+strconv.FormatInt(id, 10)).Body.String()

	assert.Contains(t, body, "-31008")
	assert.Contains(t, body, "tag bad")
}

// A dropped connection never reached a status, and the log says so rather than
// inventing one.
func TestE2ETrafficEntryWithNoStatusReadsAsADash(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "dropped", "topup", 0)
	id := s.addTrafficEntry(t, sandbox, "CheckTransaction", nil, nil)

	assert.Contains(t, s.get(t, "/traffic/"+strconv.FormatInt(id, 10)).Body.String(), "—")
}

func TestE2ETrafficListShowsTheEntryAndClearRemovesIt(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "tailed", "topup", 0)
	status := 500
	id := s.addTrafficEntry(t, sandbox, "CreateTransaction", &status, nil)

	assert.Contains(t, s.get(t, "/traffic").Body.String(), "CreateTransaction")

	_, message := location(t, s.post(t, "/traffic/"+strconv.FormatInt(id, 10)+"/delete", nil))
	assert.Contains(t, message, "Entry deleted")

	assert.NotContains(t, s.get(t, "/traffic").Body.String(), "CreateTransaction")
}

// A reset clears what a stand did without touching how it is reached.
func TestE2EResetClearsPaymentsOrdersAndTraffic(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "used", "topup", 100000)
	order := s.addOrder(t, sandbox, 500000)
	s.addTransaction(t, sandbox, order, "3333222211110000aaaa9999", 1)
	status := 200
	s.addTrafficEntry(t, sandbox, "CheckTransaction", &status, nil)

	location(t, s.post(t, "/sandboxes/"+strconv.FormatInt(sandbox.ID, 10)+"/reset", nil))

	ctx := context.Background()

	orders, err := s.store.Orders(ctx, "", 10)
	require.NoError(t, err)
	assert.Empty(t, orders)

	payments, err := s.store.Transactions(ctx, transactionFilter{}, pageRequest{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, payments)

	entries, err := s.store.TrafficDetails(ctx, trafficFilter{}, pageRequest{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, entries)

	// The payer survives with its balance zeroed: a stand with no payer cannot
	// answer a single call.
	account, err := s.store.AccountByID(ctx, sandbox.AccountID)
	require.NoError(t, err)
	assert.Zero(t, account.Balance)
}

func TestE2EResetOfAVanishedSandboxReportsIt(t *testing.T) {
	s := newStand(t)

	assert.Contains(t, s.post(t, "/sandboxes/999999/reset", nil).Body.String(), "gone")
}

func TestE2EDeleteOfAVanishedRowReportsIt(t *testing.T) {
	s := newStand(t)

	assert.Contains(t, s.post(t, "/traffic/999999/delete", nil).Body.String(), "gone")

	assert.Contains(t, s.post(t, "/orders/999999/delete", nil).Body.String(), "gone")
	assert.Contains(t, s.post(t, "/accounts/999999/delete", nil).Body.String(), "gone")
	assert.Contains(t, s.post(t, "/accounts/999999", url.Values{"name": {"Ghost"}}).Body.String(), "gone")
	assert.Contains(t, s.post(t, "/rules/999999", url.Values{
		"outcome": {outcomeSuccess},
	}).Body.String(), "gone")
}

// Removing something already gone is not an error: the operator asked for a
// state the stand is already in, and a refusal would only send them looking
// for a row nobody can see.
func TestE2ERemovingAnAlreadyGoneRowIsAccepted(t *testing.T) {
	s := newStand(t)

	location(t, s.post(t, "/rules/999999/toggle", nil))
	location(t, s.post(t, "/rules/999999/delete", nil))
}

// A profile is what a stand runs; switching it is a single field on the edit
// form.
func TestE2ESandboxCanBeCreatedWithAndMovedBetweenProfiles(t *testing.T) {
	s := newStand(t)

	profiles, err := s.store.Profiles(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, profiles)
	first := strconv.FormatInt(profiles[0].ID, 10)

	w := s.post(t, "/sandboxes", url.Values{
		"slug":      {"profiled"},
		"kind":      {"topup"},
		"balance":   {"0"},
		"config_id": {first},
	})
	location(t, w)

	sandboxes, err := s.store.Sandboxes(context.Background(), "https://x")
	require.NoError(t, err)

	var created sandboxRow
	for _, sandbox := range sandboxes {
		if sandbox.Slug == "profiled" {
			created = sandbox
		}
	}
	require.NotZero(t, created.ID)
	assert.Equal(t, profiles[0].Name, created.ConfigName)

	// And back to none, which is what a stand runs when nothing is chosen.
	location(t, s.post(t, "/sandboxes/"+strconv.FormatInt(created.ID, 10), url.Values{
		"name":      {"Profiled"},
		"kind":      {"topup"},
		"config_id": {""},
	}))

	body := s.get(t, "/sandboxes/"+strconv.FormatInt(created.ID, 10)).Body.String()
	assert.Contains(t, body, "none")
}

func TestE2ECreateSandboxRejectsABadBalanceOrKind(t *testing.T) {
	s := newStand(t)

	assert.Contains(t, s.post(t, "/sandboxes", url.Values{
		"slug": {"bad-kind"}, "kind": {"nonsense"},
	}).Body.String(), "which way the register moves money")

	assert.Contains(t, s.post(t, "/sandboxes", url.Values{
		"slug": {"bad-balance"}, "kind": {"topup"}, "balance": {"-1"},
	}).Body.String(), "zero or more")

	assert.Contains(t, s.post(t, "/sandboxes", url.Values{
		"slug": {"bad-profile"}, "kind": {"topup"}, "config_id": {"nonsense"},
	}).Body.String(), "Unknown profile")
}

// A rule may be aimed at one stand, which is how one integration is broken
// while the others keep working.
func TestE2ERuleCanBeAimedAtOneSandbox(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "targeted", "topup", 0)

	location(t, s.post(t, "/rules", url.Values{
		"method":     {"CheckTransaction"},
		"service":    {"merchant"},
		"outcome":    {"-31003"},
		"sandbox_id": {strconv.FormatInt(sandbox.ID, 10)},
	}))

	rules, err := s.store.Rules(context.Background(), "")
	require.NoError(t, err)

	var found bool
	for _, rule := range rules {
		if rule.Sandbox == "targeted" {
			found = true
		}
	}

	assert.True(t, found)
}

func TestE2ERuleWithAUseCountAndProbabilityIsDescribed(t *testing.T) {
	s := newStand(t)

	location(t, s.post(t, "/rules", url.Values{
		"method":        {"GetStatement"},
		"service":       {"merchant"},
		"outcome":       {outcomeTimeout},
		"delay_seconds": {"5"},
		"probability":   {"50"},
		"times_left":    {"3"},
	}))

	body := s.get(t, "/rules").Body.String()
	assert.Contains(t, body, "50% of the time")
	assert.Contains(t, body, "3 use(s) left")
}

// addCardAndReceipt writes a card and a receipt paid with it, driving the given
// merchant transaction. It is the shape the Payme side leaves behind, which is
// what ties a payment on the merchant's books to the card that made it.
func (s *stand) addCardAndReceipt(t *testing.T, sandbox sandboxRow, number, merchantTxn string, payout bool, state int) (cardID, receiptID int64) {
	t.Helper()

	ctx := context.Background()

	err := s.store.pool.QueryRow(ctx, `
		INSERT INTO mock.cards (sandbox_id, token, number_full, expire, verify, balance, source)
		VALUES ($1, $2, $3, '03/99', TRUE, 100000000, 'console')
		RETURNING id`, sandbox.ID, "token-"+number, number).Scan(&cardID)
	require.NoError(t, err)

	err = s.store.pool.QueryRow(ctx, `
		INSERT INTO mock.receipts
			(sandbox_id, receipt_id, merchant_id, amount, state, type, payout,
			 account, card_id, create_time, pay_time, merchant_txn)
		VALUES ($1, $2, $3, 500000, $4::smallint, 1, $5, '{"order_id": "197"}'::jsonb,
		        $6, 1750000000000, 1750000000000, $7)
		RETURNING id`, sandbox.ID, "receipt-"+number, sandbox.MerchantID, state,
		payout, cardID, merchantTxn).Scan(&receiptID)
	require.NoError(t, err)

	return cardID, receiptID
}

// A merchant transaction carries no card of its own: the provider never tells a
// merchant which card paid. The screen resolves it through the receipt that
// drove the payment, so both have to be there for the link to appear.
func TestE2ETransactionShowsTheCardAndTheCashbox(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "linked", "topup", 0)
	order := s.addOrder(t, sandbox, 500000)
	id := s.addTransaction(t, sandbox, order, "dddd4444eeee5555ffff6666", 2)
	cardID, receiptID := s.addCardAndReceipt(t, sandbox, "8600069195406311",
		"dddd4444eeee5555ffff6666", false, 4)

	page := s.get(t, "/transactions/"+strconv.FormatInt(id, 10)).Body.String()

	assert.Contains(t, page, "860006******6311", "the card that paid")
	assert.Contains(t, page, "/cards/"+strconv.FormatInt(cardID, 10))
	assert.Contains(t, page, "/receipts/"+strconv.FormatInt(receiptID, 10))
	assert.Contains(t, page, "payments add to the balance", "which way this register moves money")

	list := s.get(t, "/transactions").Body.String()
	assert.Contains(t, list, "860006******6311", "the list carries the card too")
}

// A payment made straight against the Merchant API never had a receipt, so the
// screen has to say so rather than leave a blank nobody can read.
func TestE2ETransactionWithoutAReceiptSaysSo(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "unlinked", "topup", 0)
	order := s.addOrder(t, sandbox, 500000)
	id := s.addTransaction(t, sandbox, order, "1111aaaa2222bbbb3333cccc", 2)

	page := s.get(t, "/transactions/"+strconv.FormatInt(id, 10)).Body.String()

	assert.Contains(t, page, "did not come through a saved card")
}

// The first question a card is opened with is whether anything reached it. A
// card nobody called and a card whose calls all failed look identical on the
// card itself, so the counts are what tell them apart.
func TestE2ECardPageCountsWhatReachedIt(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "counted", "topup", 0)
	cardID, _ := s.addCardAndReceipt(t, sandbox, "8600495473316478",
		"9999aaaa8888bbbb7777cccc", false, 4)

	page := s.get(t, "/cards/"+strconv.FormatInt(cardID, 10)).Body.String()

	assert.Contains(t, page, "1 charged the card")
	assert.Contains(t, page, "5 000.00", "what was taken off it")
	assert.Contains(t, page, "receipt-8600495473316478")
}

func TestE2ECardPageSaysWhenNothingReachedIt(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "untouched", "topup", 0)

	created := s.post(t, "/cards", url.Values{
		"sandbox_id": {strconv.FormatInt(sandbox.ID, 10)},
		"number":     {"8600123456789012"},
		"expire":     {"1226"},
		"outcome":    {"success"},
		"balance":    {"100000000"},
	})
	require.Equal(t, http.StatusSeeOther, created.Code)

	cards, err := s.store.Cards(context.Background(), sourceConsole, cardFilter{},
		pageOf(httptest.NewRequest(http.MethodGet, "/cards", nil)))
	require.NoError(t, err)
	require.NotEmpty(t, cards)

	page := s.get(t, "/cards/"+strconv.FormatInt(cards[0].ID, 10)).Body.String()

	assert.Contains(t, page, "Nothing has reached this card")
}

// The front screen answers the one question the stand is opened with — is the
// integration behaving — so the figures behind it are checked against rows
// rather than trusted.
func TestE2EDashboardCountsWhatMoved(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "counted-up", "topup", 500000)
	cardID, _ := s.addCardAndReceipt(t, sandbox, "8600069195406311",
		"aaaa0000bbbb1111cccc2222", false, 4)
	require.NotZero(t, cardID)

	body := s.get(t, "/").Body.String()

	assert.Contains(t, body, "Dashboard")
	assert.Contains(t, body, "counted-up", "every cashbox is listed with what went through it")
	assert.Contains(t, body, "5 000.00", "the settled top-up is counted as money that moved")
}

// A stand nobody has called yet must say so rather than draw empty panels that
// read as a failure.
func TestE2EDashboardOnAStandWithNothingOnIt(t *testing.T) {
	s := newStand(t)

	body := s.get(t, "/").Body.String()

	assert.Contains(t, body, "Nothing has failed")
}
