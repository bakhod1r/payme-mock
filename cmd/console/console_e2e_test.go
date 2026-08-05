package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/kernel/postgres/testdb"
)

// The console is the only writer of the control schema and it writes through
// SQL rather than a domain, so these run against a real PostgreSQL: what is
// under test is the statements themselves.

// stand is a console wired to a fresh database, plus the handler under test.
type stand struct {
	app   *app
	store *store
	mux   http.Handler
}

func newStand(t *testing.T) *stand {
	t.Helper()

	pool := testdb.New(t)
	st := &store{pool: pool}

	ctx := context.Background()
	_, err := st.SeedProfiles(ctx)
	require.NoError(t, err)
	_, err = st.SeedErrorCatalog(ctx)
	require.NoError(t, err)

	cfg := config{
		Username: "admin", Password: "s3cret",
		OpenOnLoopback: true,
		GatewayBaseURL: "https://merchant.localhost:8443",
	}

	application, err := newApp(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)

	return &stand{app: application, store: st, mux: application.routes()}
}

// get fetches a page as the operator at the keyboard.
func (s *stand) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = "127.0.0.1:50000"
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)

	return w
}

// post submits a form as the operator at the keyboard.
func (s *stand) post(t *testing.T, path string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "127.0.0.1:50000"
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)

	return w
}

// postRaw submits a body the form parser cannot read, which is what a broken
// client sends and what the screens have to answer without falling over.
func (s *stand) postRaw(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "127.0.0.1:50000"
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)

	return w
}

// newSandbox creates a stand through the screen that creates one and returns
// its row, which is how every later test gets something to act on.
func (s *stand) newSandbox(t *testing.T, slug, kind string, balance int64) sandboxRow {
	t.Helper()

	w := s.post(t, "/sandboxes", url.Values{
		"slug":    {slug},
		"name":    {slug + " stand"},
		"kind":    {kind},
		"balance": {strconv.FormatInt(balance, 10)},
	})
	require.Equal(t, http.StatusSeeOther, w.Code)

	sandboxes, err := s.store.Sandboxes(context.Background(), "https://merchant.localhost:8443")
	require.NoError(t, err)

	for _, sandbox := range sandboxes {
		if sandbox.Slug == slug {
			return sandbox
		}
	}

	t.Fatalf("sandbox %q was not created", slug)
	return sandboxRow{}
}

// location is where a redirect pointed, with its message decoded.
func location(t *testing.T, w *httptest.ResponseRecorder) (string, string) {
	t.Helper()

	require.Equal(t, http.StatusSeeOther, w.Code, w.Body.String())

	target, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)

	return target.Path, target.Query().Get("ok")
}

func TestE2EEveryScreenRenders(t *testing.T) {
	s := newStand(t)
	s.newSandbox(t, "screens", "topup", 100000)

	for _, path := range []string{"/", "/rules", "/cards", "/transactions", "/traffic"} {
		t.Run(path, func(t *testing.T) {
			w := s.get(t, path)

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, w.Body.String(), "payme-mock")
		})
	}
}

func TestE2ECreateSandboxShowsItInTheList(t *testing.T) {
	s := newStand(t)

	w := s.post(t, "/sandboxes", url.Values{
		"slug":    {"acme"},
		"name":    {"Acme staging"},
		"kind":    {"deposit"},
		"balance": {"250000"},
	})

	path, message := location(t, w)
	// The front screen is the dashboard now; the cashbox list has its own address.
	assert.Equal(t, "/sandboxes", path)
	assert.Contains(t, message, "acme")

	body := s.get(t, "/sandboxes").Body.String()
	assert.Contains(t, body, "acme")
	assert.Contains(t, body, "2 500.00", "balances are shown in so'm")
}

// A slug is how a stand is addressed, so two stands cannot share one.
func TestE2ECreateSandboxRefusesADuplicateSlug(t *testing.T) {
	s := newStand(t)
	s.newSandbox(t, "taken", "topup", 0)

	w := s.post(t, "/sandboxes", url.Values{
		"slug": {"taken"}, "kind": {"topup"}, "balance": {"0"},
	})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "already taken")
}

func TestE2ECreateSandboxRejectsAMissingSlug(t *testing.T) {
	s := newStand(t)

	w := s.post(t, "/sandboxes", url.Values{"kind": {"topup"}, "balance": {"0"}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "slug")
}

func TestE2ESandboxPageShowsCredentialsAndHistory(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "detail", "topup", 500000)

	body := s.get(t, "/sandboxes/"+strconv.FormatInt(sandbox.ID, 10)).Body.String()

	assert.Contains(t, body, sandbox.MerchantID)
	assert.Contains(t, body, sandbox.Key)
	assert.Contains(t, body, "5 000.00")
}

func TestE2ESandboxPageIsNotFoundForAnUnknownID(t *testing.T) {
	s := newStand(t)

	assert.Equal(t, http.StatusNotFound, s.get(t, "/sandboxes/424242").Code)
	assert.Equal(t, http.StatusNotFound, s.get(t, "/sandboxes/not-a-number").Code)
}

func TestE2EEditSandboxRenamesItAndReturnsToItsPage(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "renamed", "topup", 0)
	id := strconv.FormatInt(sandbox.ID, 10)

	w := s.post(t, "/sandboxes/"+id, url.Values{
		"name": {"After the rename"},
		"kind": {"dividend"},
		"back": {"/sandboxes/" + id},
	})

	path, message := location(t, w)
	assert.Equal(t, "/sandboxes/"+id, path, "an edit made on a page returns to it")
	assert.Contains(t, message, "updated")

	body := s.get(t, "/sandboxes/"+id).Body.String()
	assert.Contains(t, body, "After the rename")
	assert.Contains(t, body, "dividend")
}

func TestE2EEditSandboxRejectsAnEmptyName(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "unnamed", "topup", 0)

	w := s.post(t, "/sandboxes/"+strconv.FormatInt(sandbox.ID, 10),
		url.Values{"name": {""}, "kind": {"topup"}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "needs a name")
}

func TestE2EEditSandboxRejectsAnUnreadableProfile(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "profileless", "topup", 0)

	w := s.post(t, "/sandboxes/"+strconv.FormatInt(sandbox.ID, 10), url.Values{
		"name":      {"Fine"},
		"kind":      {"topup"},
		"config_id": {"not-a-number"},
	})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Unknown profile")
}

func TestE2EEditSandboxReportsAVanishedRow(t *testing.T) {
	s := newStand(t)

	w := s.post(t, "/sandboxes/999999", url.Values{"name": {"Ghost"}, "kind": {"topup"}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "gone")
}

func TestE2EResetSandboxKeepsTheCredentials(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "reset-me", "topup", 100000)
	id := strconv.FormatInt(sandbox.ID, 10)

	_, message := location(t, s.post(t, "/sandboxes/"+id+"/reset", nil))
	assert.Contains(t, message, "cleared")

	body := s.get(t, "/sandboxes/"+id).Body.String()
	assert.Contains(t, body, sandbox.MerchantID, "the keys survive a reset")
}

func TestE2EDeleteSandboxRemovesItFromTheList(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "temporary", "topup", 0)

	path, message := location(t, s.post(t, "/sandboxes/"+strconv.FormatInt(sandbox.ID, 10)+"/delete", nil))
	assert.Equal(t, "/sandboxes", path)
	assert.Contains(t, message, "deleted")

	assert.NotContains(t, s.get(t, "/sandboxes").Body.String(), "temporary")
}

func TestE2EDeleteSandboxRejectsAnUnreadableID(t *testing.T) {
	s := newStand(t)

	w := s.post(t, "/sandboxes/not-a-number/delete", nil)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Unknown sandbox")
}

// The sign of the amount is the direction, so one field says both how much and
// which way.
func TestE2EBalanceAddsAndSubtracts(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "register", "topup", 100000)
	account := strconv.FormatInt(sandbox.AccountID, 10)

	_, message := location(t, s.post(t, "/accounts/"+account+"/balance", url.Values{"amount": {"50000"}}))
	assert.Contains(t, message, "1 500.00")

	_, message = location(t, s.post(t, "/accounts/"+account+"/balance", url.Values{"amount": {"-25000"}}))
	assert.Contains(t, message, "1 250.00")
}

// A register cannot hold less than nothing, so the move is refused rather than
// clamped to zero.
func TestE2EBalanceRefusesToGoNegative(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "shallow", "topup", 1000)

	w := s.post(t, "/accounts/"+strconv.FormatInt(sandbox.AccountID, 10)+"/balance",
		url.Values{"amount": {"-2000"}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "negative balance")
}

func TestE2EBalanceRejectsAnUnreadableAmount(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "typo", "topup", 1000)

	w := s.post(t, "/accounts/"+strconv.FormatInt(sandbox.AccountID, 10)+"/balance",
		url.Values{"amount": {"a lot"}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "whole number")
}

func TestE2EBalanceRejectsAnUnknownPayer(t *testing.T) {
	s := newStand(t)

	assert.Contains(t, s.post(t, "/accounts/999999/balance", url.Values{"amount": {"1"}}).Body.String(),
		"payer is gone")
	assert.Contains(t, s.post(t, "/accounts/not-a-number/balance", url.Values{"amount": {"1"}}).Body.String(),
		"Unknown payer")
}

// Every move is recorded, because a balance says what a register holds now and
// nothing about how it got there.
func TestE2EBalanceMovesAreRecorded(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "audited", "topup", 100000)
	id := strconv.FormatInt(sandbox.ID, 10)

	s.post(t, "/accounts/"+strconv.FormatInt(sandbox.AccountID, 10)+"/balance",
		url.Values{"amount": {"5000"}, "back": {"/sandboxes/" + id}})

	body := s.get(t, "/sandboxes/"+id).Body.String()
	assert.Contains(t, body, "&#43;50.00", "the move is shown with its direction")
	assert.Contains(t, body, "console")
}

func TestE2EBlockAndUnblockThePayer(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "blockable", "topup", 0)
	account := strconv.FormatInt(sandbox.AccountID, 10)

	_, message := location(t, s.post(t, "/accounts/"+account+"/block", url.Values{"blocked": {"true"}}))
	assert.Contains(t, message, "blocked")
	assert.Contains(t, s.get(t, "/").Body.String(), "blocked")

	_, message = location(t, s.post(t, "/accounts/"+account+"/block", url.Values{"blocked": {"false"}}))
	assert.Contains(t, message, "unblocked")
}

func TestE2EBlockRejectsAnUnreadableID(t *testing.T) {
	s := newStand(t)

	assert.Contains(t, s.post(t, "/accounts/not-a-number/block", nil).Body.String(), "Unknown payer")
}

func TestE2EEditAndDeleteAPayer(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "payer", "topup", 0)
	account := strconv.FormatInt(sandbox.AccountID, 10)

	_, message := location(t, s.post(t, "/accounts/"+account, url.Values{
		"name":  {"Renamed payer"},
		"phone": {"998900000000"},
	}))
	assert.Contains(t, message, "Payer updated")

	w := s.post(t, "/accounts/"+account, url.Values{"name": {""}})
	assert.Contains(t, w.Body.String(), "needs a name")

	_, message = location(t, s.post(t, "/accounts/"+account+"/delete", nil))
	assert.Contains(t, message, "Payer deleted")
}

// An order is made and unmade from the stand that holds it. There is no order
// screen: nobody manages orders, they are only what a payment settles against.
func TestE2EOrderLifecycle(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "orders", "topup", 0)
	path := "/sandboxes/" + strconv.FormatInt(sandbox.ID, 10)

	_, message := location(t, s.post(t, path+"/orders", url.Values{
		"amount":      {"750000"},
		"description": {"Order #1"},
	}))
	assert.Contains(t, message, "Order created")

	orders, err := s.store.OrdersForSandbox(context.Background(), sandbox.ID, 10)
	require.NoError(t, err)
	require.Len(t, orders, 1)
	id := strconv.FormatInt(orders[0].ID, 10)

	// The stand's page is where the order is read, and the payer it is billed
	// to is the stand's own rather than one the form asked for.
	body := s.get(t, path).Body.String()
	assert.Contains(t, body, "7 500.00")
	assert.Contains(t, body, "Order #1")
	assert.Equal(t, sandbox.AccountID, orders[0].AccountID)

	_, message = location(t, s.post(t, "/orders/"+id+"/delete", url.Values{"back": {path}}))
	assert.Contains(t, message, "Order deleted")

	orders, err = s.store.OrdersForSandbox(context.Background(), sandbox.ID, 10)
	require.NoError(t, err)
	assert.Empty(t, orders)
}

func TestE2ECreateOrderRejectsBadInput(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "bad-orders", "topup", 0)
	path := "/sandboxes/" + strconv.FormatInt(sandbox.ID, 10)

	assert.Contains(t, s.post(t, path+"/orders", url.Values{"amount": {"0"}}).Body.String(),
		"more than zero")
	assert.Contains(t, s.post(t, path+"/orders", url.Values{"amount": {"nonsense"}}).Body.String(),
		"more than zero")
}

// An order has no page of its own, so the addresses that used to serve one
// answer nothing at all.
func TestE2EOrderPagesAreGone(t *testing.T) {
	s := newStand(t)

	assert.Equal(t, http.StatusNotFound, s.get(t, "/orders").Code)
	assert.Equal(t, http.StatusNotFound, s.get(t, "/orders/1").Code)
}
func TestE2ERuleLifecycle(t *testing.T) {
	s := newStand(t)

	_, message := location(t, s.post(t, "/rules", url.Values{
		"method":  {"PerformTransaction"},
		"service": {"merchant"},
		"outcome": {"-31008"},
	}))
	assert.Contains(t, message, "Rule saved")

	rules, err := s.store.Rules(context.Background(), "")
	require.NoError(t, err)

	var created ruleRow
	for _, rule := range rules {
		if rule.Method == "PerformTransaction" {
			created = rule
		}
	}
	require.NotZero(t, created.ID)
	id := strconv.FormatInt(created.ID, 10)

	body := s.get(t, "/rules/"+id).Body.String()
	assert.Contains(t, body, "PerformTransaction")
	assert.Contains(t, body, "-31008")

	_, message = location(t, s.post(t, "/rules/"+id+"/toggle", url.Values{"back": {"/rules/" + id}}))
	assert.Contains(t, message, "switched")

	_, message = location(t, s.post(t, "/rules/"+id, url.Values{
		"method":        {"PerformTransaction"},
		"service":       {"merchant"},
		"outcome":       {outcomeTimeout},
		"delay_seconds": {"30"},
		"back":          {"/rules/" + id},
	}))
	assert.Contains(t, message, "Rule updated")
	assert.Contains(t, s.get(t, "/rules/"+id).Body.String(), "closes the connection")

	_, message = location(t, s.post(t, "/rules/"+id+"/delete", nil))
	assert.Contains(t, message, "Rule deleted")
	assert.Equal(t, http.StatusNotFound, s.get(t, "/rules/"+id).Code)
}

// One method carries one rule: a second would leave the answer decided by
// evaluation order rather than by what was asked for last.
func TestE2EASecondRuleReplacesTheFirst(t *testing.T) {
	s := newStand(t)

	for _, outcome := range []string{"-31008", "-31003"} {
		s.post(t, "/rules", url.Values{
			"method":  {"CheckTransaction"},
			"service": {"merchant"},
			"outcome": {outcome},
		})
	}

	rules, err := s.store.Rules(context.Background(), "")
	require.NoError(t, err)

	// The seeded profiles carry rules of their own; only the ones the console
	// created stand on their own, without a profile behind them.
	var matching int
	for _, rule := range rules {
		// The seeded profiles carry rules of their own; the console names the
		// ones it creates after what they do.
		if strings.HasPrefix(rule.Name, "CheckTransaction returns") {
			matching++
			assert.Equal(t, "-31003", rule.Outcome, "the newer rule wins")
		}
	}

	assert.Equal(t, 1, matching, "the newer rule replaces the older one")
}

func TestE2ECreateRuleReportsABadForm(t *testing.T) {
	s := newStand(t)

	w := s.post(t, "/rules", url.Values{"outcome": {""}})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "pick what the method should return")
}

func TestE2EEditRuleReportsABadForm(t *testing.T) {
	s := newStand(t)

	assert.Contains(t, s.post(t, "/rules/not-a-number", nil).Body.String(), "gone")
	assert.Contains(t, s.post(t, "/rules/1", url.Values{"outcome": {"nonsense"}}).Body.String(),
		"pick what the method should return")
}

func TestE2ETogglingAnUnreadableRuleReportsIt(t *testing.T) {
	s := newStand(t)

	assert.Contains(t, s.post(t, "/rules/not-a-number/toggle", nil).Body.String(), "Unknown rule")
	assert.Contains(t, s.post(t, "/rules/not-a-number/delete", nil).Body.String(), "Unknown rule")
}

func TestE2ERulePageIsNotFoundForAnUnknownID(t *testing.T) {
	s := newStand(t)

	assert.Equal(t, http.StatusNotFound, s.get(t, "/rules/999999").Code)
	assert.Equal(t, http.StatusNotFound, s.get(t, "/rules/not-a-number").Code)
}

func TestE2ETrafficEntryPageIsNotFoundForAnUnknownID(t *testing.T) {
	s := newStand(t)

	assert.Equal(t, http.StatusNotFound, s.get(t, "/traffic/999999").Code)
	assert.Equal(t, http.StatusNotFound, s.get(t, "/traffic/not-a-number").Code)
}

// The log is not emptied from the screen any more: it is what a stand did, and
// a button that wipes it is one bad click away from losing the evidence a
// rehearsal was run for. A single entry can still be removed.
func TestE2ETrafficRowCanBeDeletedButTheLogCannotBeCleared(t *testing.T) {
	s := newStand(t)

	// The address is gone; what is left under /traffic takes no POST at all.
	assert.Equal(t, http.StatusMethodNotAllowed, s.post(t, "/traffic/clear", nil).Code)
	assert.Contains(t, s.post(t, "/traffic/not-a-number/delete", nil).Body.String(), "gone")
}

func TestE2ETransactionPageIsNotFoundForAnUnknownID(t *testing.T) {
	s := newStand(t)

	assert.Equal(t, http.StatusNotFound, s.get(t, "/transactions/999999").Code)
	assert.Equal(t, http.StatusNotFound, s.get(t, "/transactions/not-a-number").Code)
}

func TestE2EEditTransactionReportsABadForm(t *testing.T) {
	s := newStand(t)

	assert.Contains(t, s.post(t, "/transactions/not-a-number", nil).Body.String(), "gone")
	assert.Contains(t, s.post(t, "/transactions/1", url.Values{"state": {"soon"}}).Body.String(),
		"Pick a state")
	assert.Contains(t, s.post(t, "/transactions/not-a-number/delete", nil).Body.String(), "gone")
}

// The filter narrows by stand, by state and by identifier, and says on the
// page which of them is in force.
func TestE2ETransactionFilterIsShownBack(t *testing.T) {
	s := newStand(t)
	s.newSandbox(t, "filtered", "topup", 0)

	body := s.get(t, "/transactions?sandbox=filtered&state=2&q=5305").Body.String()

	assert.Contains(t, body, "filtered")
	assert.Contains(t, body, ">2<", "the state in force is named back")
	assert.Contains(t, body, "5305")
}

func TestE2EProfilesAreSeededOnce(t *testing.T) {
	s := newStand(t)
	ctx := context.Background()

	again, err := s.store.SeedProfiles(ctx)
	require.NoError(t, err)
	assert.Zero(t, again, "a restart leaves the existing profiles alone")

	againErrors, err := s.store.SeedErrorCatalog(ctx)
	require.NoError(t, err)
	assert.Zero(t, againErrors)

	profiles, err := s.store.Profiles(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, profiles)
}

func TestE2EErrorCatalogIsListedNewestCodeFirst(t *testing.T) {
	s := newStand(t)

	catalog, err := s.store.Errors(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, catalog)

	for i := 1; i < len(catalog); i++ {
		assert.Less(t, catalog[i].Code, catalog[i-1].Code)
	}
}

func TestE2ETrafficListIsReadable(t *testing.T) {
	s := newStand(t)

	entries, err := s.store.Traffic(context.Background(), 10)
	require.NoError(t, err)
	assert.Empty(t, entries, "a fresh stand has answered nothing yet")

	details, err := s.store.TrafficDetails(context.Background(), trafficFilter{}, pageRequest{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, details)
}

func TestE2EAccountsAreListedForTheOrderForm(t *testing.T) {
	s := newStand(t)
	s.newSandbox(t, "with-payer", "topup", 0)

	accounts, err := s.store.Accounts(context.Background())
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	assert.Equal(t, "with-payer", accounts[0].Sandbox)
}

func TestE2ESandboxByIDReportsAMissingRow(t *testing.T) {
	s := newStand(t)

	_, err := s.store.SandboxByID(context.Background(), 999999, "https://x")
	assert.Error(t, err)
}

func TestE2EByIDLookupsReportAMissingRow(t *testing.T) {
	s := newStand(t)
	ctx := context.Background()

	_, err := s.store.OrderByID(ctx, 999999)
	assert.ErrorIs(t, err, errNoRow)

	_, err = s.store.TransactionByID(ctx, 999999)
	assert.ErrorIs(t, err, errNoRow)

	_, err = s.store.RuleByID(ctx, 999999)
	assert.ErrorIs(t, err, errNoRow)

	_, err = s.store.TrafficByID(ctx, 999999)
	assert.ErrorIs(t, err, errNoRow)

	_, err = s.store.AccountByID(ctx, 999999)
	assert.ErrorIs(t, err, errNoRow)
}

func TestE2EAccountByIDReadsThePayer(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "one-payer", "topup", 4200)

	account, err := s.store.AccountByID(context.Background(), sandbox.AccountID)
	require.NoError(t, err)

	assert.Equal(t, int64(4200), account.Balance)
	assert.Equal(t, "one-payer", account.Sandbox)
}

// The transaction filter is SQL, so the cases it has to get right are checked
// against the database rather than in the handler.
func TestE2ETransactionFilterMatchesNothingOnAFreshStand(t *testing.T) {
	s := newStand(t)

	for _, filter := range []transactionFilter{
		{},
		{Sandbox: "nothing"},
		{State: "2"},
		{Query: "5305"},
	} {
		found, err := s.store.Transactions(context.Background(), filter, pageRequest{Limit: 10})
		require.NoError(t, err)
		assert.Empty(t, found)
	}
}

func TestE2EBalanceHistoryIsEmptyBeforeAnyMove(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "quiet", "topup", 0)

	history, err := s.store.BalanceHistory(context.Background(), sandbox.ID, 10)
	require.NoError(t, err)
	assert.Empty(t, history)
}
