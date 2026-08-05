package infrastructure_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/infrastructure"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres/testdb"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

const (
	cardNumber = "8600069195406311"
	receiptID  = "5305e3bab097f420a62ced0b"
	amount     = int64(500000)
	nowMs      = int64(1_399_114_284_039)
)

type stand struct {
	pool      *postgres.Pool
	cards     *infrastructure.CardRepository
	receipts  *infrastructure.ReceiptRepository
	ctx       context.Context
	sandboxID int64
}

func newStand(t *testing.T) *stand {
	t.Helper()

	pool := testdb.New(t)
	sandboxID := testdb.Seed(t, pool, "qa")

	return &stand{
		pool:      pool,
		cards:     infrastructure.NewCardRepository(pool),
		receipts:  infrastructure.NewReceiptRepository(pool),
		ctx:       sandboxctx.With(context.Background(), sandboxctx.Sandbox{ID: sandboxID, Slug: "qa"}),
		sandboxID: sandboxID,
	}
}

func (s *stand) newCard(token string) *domain.Card {
	return &domain.Card{
		SandboxID: s.sandboxID, Token: token, NumberFull: cardNumber,
		Expire: "03/99", Recurrent: true, Balance: 100_000_000,
	}
}

func (s *stand) newReceipt(id string) *domain.Receipt {
	r := domain.NewReceipt(s.sandboxID, id, "merchant-qa", amount,
		map[string]string{"order_id": "197"}, nowMs)
	r.Description = "Payment for order 197"
	return r
}

// unscoped is a context with no sandbox, which every repository must refuse
// rather than read across stands.
var unscoped = context.Background()

// ---------- cards ----------

func TestE2ECardCreateAndLoad(t *testing.T) {
	s := newStand(t)
	card := s.newCard("tok-1")

	require.NoError(t, s.cards.Create(s.ctx, card))
	assert.NotZero(t, card.ID, "the row's identifier is assigned on insert")

	byToken, err := s.cards.ByToken(s.ctx, "tok-1")
	require.NoError(t, err)
	assert.Equal(t, cardNumber, byToken.NumberFull)
	assert.Equal(t, "03/99", byToken.Expire)
	assert.True(t, byToken.Recurrent)
	assert.Empty(t, byToken.VerifyCode, "an unset code reads back as unset, not blank")
	assert.Empty(t, byToken.Phone)

	byID, err := s.cards.ByID(s.ctx, card.ID)
	require.NoError(t, err)
	assert.Equal(t, card.ID, byID.ID)
	assert.Equal(t, "860006******6311", byID.NumberMask())
}

func TestE2ECardUpdatePersistsVerificationAndCharge(t *testing.T) {
	s := newStand(t)
	card := s.newCard("tok-2")
	require.NoError(t, s.cards.Create(s.ctx, card))

	card.SendVerifyCode("666666", nowMs, domain.DefaultVerifyWaitMillis)
	card.Phone = "998901234567"
	require.NoError(t, s.cards.Update(s.ctx, card))

	stored, err := s.cards.ByToken(s.ctx, "tok-2")
	require.NoError(t, err)
	assert.Equal(t, "666666", stored.VerifyCode)
	assert.Equal(t, nowMs, stored.VerifyCodeSentAt)
	assert.Equal(t, domain.DefaultVerifyWaitMillis, stored.VerifyWaitMillis)
	assert.Equal(t, "998901234567", stored.Phone)

	require.NoError(t, stored.VerifyWith("666666", nowMs))
	stored.Charge(amount)
	stored.Removed = true
	require.NoError(t, s.cards.Update(s.ctx, stored))

	final, err := s.cards.ByToken(s.ctx, "tok-2")
	require.NoError(t, err)
	assert.True(t, final.Verify)
	assert.Empty(t, final.VerifyCode, "a spent code is cleared")
	assert.Equal(t, int64(100_000_000)-amount, final.Balance)
	assert.True(t, final.Removed)
}

func TestE2ECardMissing(t *testing.T) {
	s := newStand(t)

	_, err := s.cards.ByToken(s.ctx, "nobody")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	_, err = s.cards.ByID(s.ctx, 9999)
	assert.ErrorIs(t, err, domain.ErrNotFound)

	err = s.cards.Update(s.ctx, &domain.Card{ID: 9999})
	assert.ErrorIs(t, err, domain.ErrNotFound, "updating a row that is gone is reported, not silent")
}

// A card belongs to one stand. Reading it from another must find nothing, so
// an experiment in one sandbox cannot be seen from the next.
func TestE2ECardsAreIsolatedPerSandbox(t *testing.T) {
	s := newStand(t)
	other := testdb.Seed(t, s.pool, "demo")
	otherCtx := sandboxctx.With(context.Background(), sandboxctx.Sandbox{ID: other, Slug: "demo"})

	require.NoError(t, s.cards.Create(s.ctx, s.newCard("shared-token")))

	_, err := s.cards.ByToken(otherCtx, "shared-token")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// The other stand may hold the same number, as its own card with its own
	// token. The string itself is never reused: a token names one card at one
	// register, which is what makes it safe to look one up by token alone.
	twin := &domain.Card{SandboxID: other, Token: "other-token", NumberFull: cardNumber, Expire: "03/99"}
	require.NoError(t, s.cards.Create(otherCtx, twin))
}

// Registers named as one merchant's share their cards: the integration
// tokenizes through the top-up register and pays out through the dividend one,
// sending the same token, and that second call has to find the card.
func TestE2ECardsAreSharedInsideAMerchant(t *testing.T) {
	s := newStand(t)
	other := testdb.Seed(t, s.pool, "payout")
	otherCtx := sandboxctx.With(context.Background(), sandboxctx.Sandbox{ID: other, Slug: "payout"})

	_, err := s.pool.Exec(context.Background(),
		`UPDATE control.sandboxes SET merchant_group = 'one-merchant' WHERE id = ANY($1)`,
		[]int64{s.sandboxID, other})
	require.NoError(t, err)

	card := s.newCard("tok-shared")
	require.NoError(t, s.cards.Create(s.ctx, card))

	found, err := s.cards.ByToken(otherCtx, "tok-shared")
	require.NoError(t, err)
	assert.Equal(t, card.ID, found.ID, "the sibling register reads the merchant's card")

	byID, err := s.cards.ByID(otherCtx, card.ID)
	require.NoError(t, err)
	assert.Equal(t, card.ID, byID.ID)

	// A third stand outside the merchant still sees nothing.
	outsider := testdb.Seed(t, s.pool, "stranger")
	outsiderCtx := sandboxctx.With(context.Background(),
		sandboxctx.Sandbox{ID: outsider, Slug: "stranger"})

	_, err = s.cards.ByToken(outsiderCtx, "tok-shared")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// One card, a token per register. The registers of a merchant charge the same
// card, and each is handed its own string for it: an integration storing a
// token per till is storing what the provider gave that till.
func TestE2EEachRegisterGetsItsOwnToken(t *testing.T) {
	s := newStand(t)
	other := testdb.Seed(t, s.pool, "second-till")
	otherCtx := sandboxctx.With(context.Background(),
		sandboxctx.Sandbox{ID: other, Slug: "second-till"})

	_, err := s.pool.Exec(context.Background(),
		`UPDATE control.sandboxes SET merchant_group = 'one-merchant' WHERE id = ANY($1)`,
		[]int64{s.sandboxID, other})
	require.NoError(t, err)

	card := s.newCard("tok-first")
	require.NoError(t, s.cards.Create(s.ctx, card))

	second, err := s.cards.TokenFor(otherCtx, card.ID, "tok-second")
	require.NoError(t, err)
	assert.Equal(t, "tok-second", second, "the other register is handed its own token")

	again, err := s.cards.TokenFor(otherCtx, card.ID, "tok-third")
	require.NoError(t, err)
	assert.Equal(t, "tok-second", again, "asking twice does not mint a second token")

	first, err := s.cards.TokenFor(s.ctx, card.ID, "tok-ignored")
	require.NoError(t, err)
	assert.Equal(t, "tok-first", first, "the register that created it keeps the token it has")

	// Either token reaches the one card, with its one balance.
	byFirst, err := s.cards.ByToken(s.ctx, "tok-first")
	require.NoError(t, err)
	bySecond, err := s.cards.ByToken(otherCtx, "tok-second")
	require.NoError(t, err)
	assert.Equal(t, byFirst.ID, bySecond.ID)
	assert.Equal(t, byFirst.Balance, bySecond.Balance)
}

// A number is one card. A second row for the same number is refused by the
// schema, whichever stand of the merchant it is added to, because a card with
// two rows would have two balances and two behaviours.
func TestE2EANumberIsOneCard(t *testing.T) {
	s := newStand(t)

	held := s.newCard("tok-held")
	held.Outcome = domain.OutcomeBlocked
	require.NoError(t, s.cards.Create(s.ctx, held))

	twin := s.newCard("tok-twin")
	assert.Error(t, s.cards.Create(s.ctx, twin), "the same number twice is refused")

	found, err := s.cards.ByNumber(s.ctx, cardNumber)
	require.NoError(t, err)
	assert.Equal(t, held.ID, found.ID)
	assert.Equal(t, domain.OutcomeBlocked, found.Outcome)

	_, err = s.cards.ByNumber(s.ctx, "0000000000000000")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	_, err = s.cards.ByNumber(unscoped, cardNumber)
	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)
}

// A removed card is not held any more. Handing its token back would answer
// cards.create with a card that then refuses everything asked of it — the
// verify code, the payment — which is what a payer re-adding a card they had
// deleted would run into.
func TestE2EARemovedCardIsNotHeld(t *testing.T) {
	s := newStand(t)

	card := s.newCard("tok-removed")
	require.NoError(t, s.cards.Create(s.ctx, card))

	card.Removed = true
	require.NoError(t, s.cards.Update(s.ctx, card))

	_, err := s.cards.ByNumber(s.ctx, cardNumber)

	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// The registers of one merchant share a card, so the number one of them holds
// cannot be added again through another.
func TestE2EACardIsOnePerMerchantNotPerStand(t *testing.T) {
	s := newStand(t)
	other := testdb.Seed(t, s.pool, "sibling")

	_, err := s.pool.Exec(context.Background(),
		`UPDATE control.sandboxes SET merchant_group = 'one-merchant' WHERE id = ANY($1)`,
		[]int64{s.sandboxID, other})
	require.NoError(t, err)

	require.NoError(t, s.cards.Create(s.ctx, s.newCard("tok-here")))

	twin := &domain.Card{
		SandboxID: other, Token: "tok-there", NumberFull: cardNumber, Expire: "03/99",
	}
	otherCtx := sandboxctx.With(context.Background(),
		sandboxctx.Sandbox{ID: other, Slug: "sibling"})

	assert.Error(t, s.cards.Create(otherCtx, twin),
		"the merchant already holds this card")
}

func TestE2ECardRepositoryRefusesAnUnscopedRequest(t *testing.T) {
	s := newStand(t)

	_, err := s.cards.ByToken(unscoped, "tok-1")
	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)

	_, err = s.cards.ByID(unscoped, 1)
	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)
}

// ---------- receipts ----------

func TestE2EReceiptCreateAndLoad(t *testing.T) {
	s := newStand(t)
	rec := s.newReceipt(receiptID)
	rec.Detail = map[string]any{"receipt_type": float64(0)}
	rec.Payer = map[string]any{"phone": "998901234567"}

	require.NoError(t, s.receipts.Create(s.ctx, rec))
	assert.NotZero(t, rec.ID)

	got, err := s.receipts.ByReceiptID(s.ctx, receiptID)
	require.NoError(t, err)
	assert.Equal(t, amount, got.Amount)
	assert.Equal(t, domain.CurrencyUZS, got.Currency)
	assert.Equal(t, domain.StateCreated, got.State)
	assert.Equal(t, map[string]string{"order_id": "197"}, got.Account)
	assert.Equal(t, map[string]any{"receipt_type": float64(0)}, got.Detail)
	assert.Equal(t, map[string]any{"phone": "998901234567"}, got.Payer)
	assert.Empty(t, got.CardSystem, "an unpaid receipt has no network yet")
	assert.Empty(t, got.MerchantTxn, "nothing has been driven on the merchant yet")
}

// The transaction a receipt drove is the only link between a card and a payment
// on the merchant's books, so it has to survive both the insert and the walk.
func TestE2EReceiptKeepsTheMerchantTransaction(t *testing.T) {
	s := newStand(t)

	opened := s.newReceipt(receiptID)
	opened.MerchantTxn = "txn-opened"
	require.NoError(t, s.receipts.Create(s.ctx, opened))

	got, err := s.receipts.ByReceiptID(s.ctx, receiptID)
	require.NoError(t, err)
	assert.Equal(t, "txn-opened", got.MerchantTxn, "written on insert, for a payout")

	got.MerchantTxn = "txn-performed"
	require.NoError(t, s.receipts.Update(s.ctx, got))

	reloaded, err := s.receipts.ByReceiptID(s.ctx, receiptID)
	require.NoError(t, err)
	assert.Equal(t, "txn-performed", reloaded.MerchantTxn,
		"written on update, for a payment whose chain has run")
}

// The receipt walk is stepwise: each state reached must survive a reload, or
// the console would show a receipt that skipped a step.
func TestE2EReceiptWalkPersistsEveryStep(t *testing.T) {
	s := newStand(t)
	card := s.newCard("tok-pay")
	require.NoError(t, s.cards.Create(s.ctx, card))

	rec := s.newReceipt(receiptID)
	require.NoError(t, s.receipts.Create(s.ctx, rec))

	require.NoError(t, rec.BeginPay(card.ID, domain.CardUzcard, false, nowMs))
	require.NoError(t, s.receipts.Update(s.ctx, rec))

	states := []domain.ReceiptState{domain.StateWithdrawn, domain.StateClosing, domain.StatePaid}
	for _, want := range states {
		require.True(t, rec.Advance(nowMs))
		require.NoError(t, s.receipts.Update(s.ctx, rec))

		got, err := s.receipts.ByReceiptID(s.ctx, receiptID)
		require.NoError(t, err)
		assert.Equal(t, want, got.State)
		assert.Equal(t, domain.CardUzcard, got.CardSystem)
		require.NotNil(t, got.CardID)
		assert.Equal(t, card.ID, *got.CardID)
	}

	assert.False(t, rec.Advance(nowMs), "a paid receipt has nowhere left to go")
}

func TestE2EReceiptHoldSurvivesReload(t *testing.T) {
	s := newStand(t)
	rec := s.newReceipt(receiptID)
	rec.Hold = true
	rec.HoldExpire = nowMs + 1000
	require.NoError(t, s.receipts.Create(s.ctx, rec))

	got, err := s.receipts.ByReceiptID(s.ctx, receiptID)
	require.NoError(t, err)
	assert.True(t, got.Hold)
	assert.Equal(t, nowMs+1000, got.HoldExpire)
	assert.True(t, got.HoldExpired(nowMs+2000))
}

func TestE2EReceiptMissing(t *testing.T) {
	s := newStand(t)

	_, err := s.receipts.ByReceiptID(s.ctx, "nobody")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	err = s.receipts.Update(s.ctx, &domain.Receipt{ID: 9999})
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// receipts.get_all reports newest first within an inclusive window, which is
// what a caller reconciling a period depends on.
func TestE2EReceiptListIsNewestFirstAndInclusive(t *testing.T) {
	s := newStand(t)

	for i, at := range []int64{nowMs, nowMs + 1000, nowMs + 2000} {
		rec := s.newReceipt(string(rune('a'+i)) + "-receipt")
		rec.CreateTime = at
		require.NoError(t, s.receipts.Create(s.ctx, rec))
	}

	all, err := s.receipts.List(s.ctx, nowMs, nowMs+2000, 50, 0)
	require.NoError(t, err)
	require.Len(t, all, 3, "both bounds are included")
	assert.Equal(t, nowMs+2000, all[0].CreateTime)
	assert.Equal(t, nowMs, all[2].CreateTime)

	page, err := s.receipts.List(s.ctx, nowMs, nowMs+2000, 1, 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, nowMs+1000, page[0].CreateTime, "offset skips the newest")

	outside, err := s.receipts.List(s.ctx, nowMs+5000, nowMs+6000, 50, 0)
	require.NoError(t, err)
	assert.Empty(t, outside)
}

func TestE2EReceiptsAreIsolatedPerSandbox(t *testing.T) {
	s := newStand(t)
	other := testdb.Seed(t, s.pool, "demo")
	otherCtx := sandboxctx.With(context.Background(), sandboxctx.Sandbox{ID: other, Slug: "demo"})

	require.NoError(t, s.receipts.Create(s.ctx, s.newReceipt(receiptID)))

	_, err := s.receipts.ByReceiptID(otherCtx, receiptID)
	assert.ErrorIs(t, err, domain.ErrNotFound)

	listed, err := s.receipts.List(otherCtx, 0, nowMs+9999, 50, 0)
	require.NoError(t, err)
	assert.Empty(t, listed, "one stand's receipts never appear in another's listing")
}

func TestE2EReceiptRepositoryRefusesAnUnscopedRequest(t *testing.T) {
	s := newStand(t)

	_, err := s.receipts.ByReceiptID(unscoped, receiptID)
	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)

	_, err = s.receipts.List(unscoped, 0, nowMs, 10, 0)
	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)
}

// detail and payer arrive from the caller, so a value JSON cannot represent
// must be reported rather than stored as something else.
func TestE2EReceiptRejectsUnencodableObjects(t *testing.T) {
	s := newStand(t)

	rec := s.newReceipt("detail-receipt")
	rec.Detail = map[string]any{"items": make(chan int)}
	assert.ErrorContains(t, s.receipts.Create(s.ctx, rec), "encode detail")

	rec = s.newReceipt("payer-receipt")
	rec.Payer = map[string]any{"ip": make(chan int)}
	assert.ErrorContains(t, s.receipts.Create(s.ctx, rec), "encode payer")

	stored := s.newReceipt("stored-receipt")
	require.NoError(t, s.receipts.Create(s.ctx, stored))
	stored.Payer = map[string]any{"ip": make(chan int)}
	assert.ErrorContains(t, s.receipts.Update(s.ctx, stored), "encode payer")
}

// An account map cannot fail to encode, but a receipt whose stored JSON no
// longer decodes must surface rather than come back half-read.
func TestE2EReceiptReportsCorruptStoredJSON(t *testing.T) {
	s := newStand(t)
	rec := s.newReceipt(receiptID)
	rec.Detail = map[string]any{"receipt_type": float64(0)}
	rec.Payer = map[string]any{"phone": "998901234567"}
	require.NoError(t, s.receipts.Create(s.ctx, rec))

	corrupt := func(t *testing.T, column, value string) {
		t.Helper()
		_, err := s.pool.Exec(context.Background(),
			`UPDATE mock.receipts SET `+column+` = $1::jsonb WHERE id = $2`, value, rec.ID)
		require.NoError(t, err)
	}

	corrupt(t, "account", `"not an object"`)
	_, err := s.receipts.ByReceiptID(s.ctx, receiptID)
	assert.ErrorContains(t, err, "decode account")

	// The listing path reads the same rows, so it must report the same fault.
	_, err = s.receipts.List(s.ctx, 0, nowMs+9999, 50, 0)
	assert.ErrorContains(t, err, "read receipts")

	corrupt(t, "account", `{"order_id":"197"}`)
	corrupt(t, "detail", `"not an object"`)
	_, err = s.receipts.ByReceiptID(s.ctx, receiptID)
	assert.ErrorContains(t, err, "decode detail")

	corrupt(t, "detail", `{"receipt_type":0}`)
	corrupt(t, "payer", `"not an object"`)
	_, err = s.receipts.ByReceiptID(s.ctx, receiptID)
	assert.ErrorContains(t, err, "decode payer")
}

// Every method must report a database it can no longer reach, rather than
// answering as though the stand were empty.
func TestE2ERepositoriesReportALostDatabase(t *testing.T) {
	s := newStand(t)
	card := s.newCard("tok-lost")
	require.NoError(t, s.cards.Create(s.ctx, card))
	rec := s.newReceipt(receiptID)
	require.NoError(t, s.receipts.Create(s.ctx, rec))

	s.pool.Close()

	_, err := s.cards.ByToken(s.ctx, "tok-lost")
	assert.ErrorContains(t, err, "scan card")

	_, err = s.cards.ByID(s.ctx, card.ID)
	assert.ErrorContains(t, err, "scan card")

	assert.ErrorContains(t, s.cards.Create(s.ctx, s.newCard("tok-none")), "insert card")
	assert.ErrorContains(t, s.cards.Update(s.ctx, card), "update card")

	_, err = s.receipts.ByReceiptID(s.ctx, receiptID)
	assert.ErrorContains(t, err, "scan receipt")

	assert.ErrorContains(t, s.receipts.Create(s.ctx, s.newReceipt("none")), "insert receipt")
	assert.ErrorContains(t, s.receipts.Update(s.ctx, rec), "update receipt")

	_, err = s.receipts.List(s.ctx, 0, nowMs+9999, 50, 0)
	assert.ErrorContains(t, err, "select receipts")
}

// A token belongs to a register, so issuing one needs to know which register is
// asking. A call arriving outside any stand is refused rather than answered
// from whichever stand happens to hold the card.
func TestE2ETokenForNeedsAStand(t *testing.T) {
	s := newStand(t)
	card := s.newCard("tok-scoped")
	require.NoError(t, s.cards.Create(s.ctx, card))

	_, err := s.cards.TokenFor(unscoped, card.ID, "tok-other")

	assert.Error(t, err)
}

// A database that went away is reported. Answered with an empty token, the
// caller would be handed a card it could never charge again.
func TestE2ETokenForReportsALostDatabase(t *testing.T) {
	s := newStand(t)
	card := s.newCard("tok-lost")
	require.NoError(t, s.cards.Create(s.ctx, card))

	s.pool.Close()

	_, err := s.cards.TokenFor(s.ctx, card.ID, "tok-new")

	assert.ErrorContains(t, err, "issue card token")
}

// A card whose row was written but whose token could not be recorded is not a
// card: nothing could ever find it, because the API looks a card up by the
// token the caller sends.
func TestE2ECreateReportsATokenItCouldNotRecord(t *testing.T) {
	s := newStand(t)

	_, err := s.pool.Exec(s.ctx, `DROP TABLE mock.card_tokens`)
	require.NoError(t, err)

	err = s.cards.Create(s.ctx, s.newCard("tok-untracked"))

	assert.ErrorContains(t, err, "issue card token")
}

// What a card was saved for is the integration's own words about the token: an
// account it means to pay, a payer it calls its own. Both are optional, and a
// card saved without them must read back unset rather than as a blank object
// nobody sent.
func TestE2ECardKeepsWhatItWasSavedFor(t *testing.T) {
	s := newStand(t)

	saved := s.newCard("tok-saved")
	saved.Account = map[string]string{"order_id": "197"}
	saved.Customer = "customer-9"
	saved.Phone = "998901234567"
	saved.VerifyCode = "039999"
	require.NoError(t, s.cards.Create(s.ctx, saved))

	got, err := s.cards.ByToken(s.ctx, "tok-saved")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"order_id": "197"}, got.Account)
	assert.Equal(t, "customer-9", got.Customer)
	assert.Equal(t, "998901234567", got.Phone)
	assert.Equal(t, "039999", got.VerifyCode)

	bare := s.newCard("tok-bare")
	bare.NumberFull = "8600000000000000"
	require.NoError(t, s.cards.Create(s.ctx, bare))

	plain, err := s.cards.ByToken(s.ctx, "tok-bare")
	require.NoError(t, err)
	assert.Empty(t, plain.Account, "an account nobody sent reads back as unset")
	assert.Empty(t, plain.Customer)
	assert.Empty(t, plain.Phone)
	assert.Empty(t, plain.VerifyCode)
}
