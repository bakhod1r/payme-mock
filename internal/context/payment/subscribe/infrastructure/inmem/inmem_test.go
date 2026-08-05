package inmem_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	domain "github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/infrastructure/inmem"
)

func TestCards(t *testing.T) {
	ctx := context.Background()

	t.Run("a card is found by every route into it", func(t *testing.T) {
		cards := inmem.NewCards()
		card := &domain.Card{Token: "tok-1", NumberFull: "8600123456789012"}

		require.NoError(t, cards.Create(ctx, card))
		assert.NotZero(t, card.ID, "the store assigns the identifier")

		byToken, err := cards.ByToken(ctx, "tok-1")
		require.NoError(t, err)
		assert.Equal(t, card.ID, byToken.ID)

		byID, err := cards.ByID(ctx, card.ID)
		require.NoError(t, err)
		assert.Equal(t, "8600123456789012", byID.NumberFull)

		byNumber, err := cards.ByNumber(ctx, "8600123456789012")
		require.NoError(t, err)
		assert.Equal(t, card.ID, byNumber.ID)
	})

	t.Run("nothing found", func(t *testing.T) {
		cards := inmem.NewCards()

		_, err := cards.ByToken(ctx, "absent")
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = cards.ByID(ctx, 99)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = cards.ByNumber(ctx, "0000")
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("a card handed out is a copy", func(t *testing.T) {
		cards := inmem.NewCards()
		require.NoError(t, cards.Create(ctx, &domain.Card{Token: "t", NumberFull: "n"}))

		got, err := cards.ByToken(ctx, "t")
		require.NoError(t, err)
		got.NumberFull = "tampered"

		again, err := cards.ByToken(ctx, "t")
		require.NoError(t, err)
		assert.Equal(t, "n", again.NumberFull,
			"a caller holding a pointer into the store could rewrite it from outside")
	})

	t.Run("update writes back, and only to a card that exists", func(t *testing.T) {
		cards := inmem.NewCards()
		card := &domain.Card{Token: "t", NumberFull: "n"}
		require.NoError(t, cards.Create(ctx, card))

		card.Verify = true
		require.NoError(t, cards.Update(ctx, card))

		got, err := cards.ByID(ctx, card.ID)
		require.NoError(t, err)
		assert.True(t, got.Verify)

		assert.ErrorIs(t, cards.Update(ctx, &domain.Card{ID: 404}), domain.ErrNotFound)
	})

	t.Run("the first token issued is the one the card keeps", func(t *testing.T) {
		cards := inmem.NewCards()
		card := &domain.Card{NumberFull: "n"}
		require.NoError(t, cards.Create(ctx, card))

		first, err := cards.TokenFor(ctx, card.ID, "offered-1")
		require.NoError(t, err)
		assert.Equal(t, "offered-1", first)

		second, err := cards.TokenFor(ctx, card.ID, "offered-2")
		require.NoError(t, err)
		assert.Equal(t, "offered-1", second, "a second offer does not replace a live token")

		found, err := cards.ByToken(ctx, "offered-1")
		require.NoError(t, err)
		assert.Equal(t, card.ID, found.ID)

		_, err = cards.TokenFor(ctx, 404, "x")
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("listing is newest first", func(t *testing.T) {
		cards := inmem.NewCards()
		require.NoError(t, cards.Create(ctx, &domain.Card{Token: "a"}))
		require.NoError(t, cards.Create(ctx, &domain.Card{Token: "b"}))

		got := cards.Cards()
		require.Len(t, got, 2)
		assert.Equal(t, "b", got[0].Token)
	})
}

func TestReceipts(t *testing.T) {
	ctx := context.Background()

	t.Run("created, found and written back", func(t *testing.T) {
		receipts := inmem.NewReceipts()
		require.NoError(t, receipts.Create(ctx, &domain.Receipt{ReceiptID: "r1", Amount: 500}))

		got, err := receipts.ByReceiptID(ctx, "r1")
		require.NoError(t, err)
		assert.EqualValues(t, 500, got.Amount)

		got.Amount = 900
		require.NoError(t, receipts.Update(ctx, got))

		again, err := receipts.ByReceiptID(ctx, "r1")
		require.NoError(t, err)
		assert.EqualValues(t, 900, again.Amount)
	})

	t.Run("nothing found", func(t *testing.T) {
		receipts := inmem.NewReceipts()

		_, err := receipts.ByReceiptID(ctx, "absent")
		assert.ErrorIs(t, err, domain.ErrNotFound)
		assert.ErrorIs(t, receipts.Update(ctx, &domain.Receipt{ReceiptID: "absent"}),
			domain.ErrNotFound)
	})

	t.Run("a receipt handed out is a copy", func(t *testing.T) {
		receipts := inmem.NewReceipts()
		require.NoError(t, receipts.Create(ctx, &domain.Receipt{ReceiptID: "r", Amount: 100}))

		got, err := receipts.ByReceiptID(ctx, "r")
		require.NoError(t, err)
		got.Amount = 1

		again, err := receipts.ByReceiptID(ctx, "r")
		require.NoError(t, err)
		assert.EqualValues(t, 100, again.Amount)
	})

	t.Run("listed newest first, windowed and paged", func(t *testing.T) {
		receipts := inmem.NewReceipts()
		for i, id := range []string{"r1", "r2", "r3"} {
			require.NoError(t, receipts.Create(ctx,
				&domain.Receipt{ReceiptID: id, CreateTime: int64(10 + i)}))
		}

		all, err := receipts.List(ctx, 0, 0, 0, 0)
		require.NoError(t, err)
		require.Len(t, all, 3)
		assert.Equal(t, "r3", all[0].ReceiptID, "newest first")

		windowed, err := receipts.List(ctx, 11, 11, 0, 0)
		require.NoError(t, err)
		require.Len(t, windowed, 1)
		assert.Equal(t, "r2", windowed[0].ReceiptID)

		capped, err := receipts.List(ctx, 0, 0, 2, 0)
		require.NoError(t, err)
		assert.Len(t, capped, 2)

		skipped, err := receipts.List(ctx, 0, 0, 0, 2)
		require.NoError(t, err)
		require.Len(t, skipped, 1)
		assert.Equal(t, "r1", skipped[0].ReceiptID)

		past, err := receipts.List(ctx, 0, 0, 0, 99)
		require.NoError(t, err)
		assert.Empty(t, past, "an offset past the end is empty, not an error")
	})

	t.Run("the playground's own listing", func(t *testing.T) {
		receipts := inmem.NewReceipts()
		require.NoError(t, receipts.Create(ctx, &domain.Receipt{ReceiptID: "r1"}))
		require.NoError(t, receipts.Create(ctx, &domain.Receipt{ReceiptID: "r2"}))

		got := receipts.Receipts()
		require.Len(t, got, 2)
		assert.Equal(t, "r2", got[0].ReceiptID)
	})
}

func TestLedger(t *testing.T) {
	ctx := context.Background()

	t.Run("opening a payout moves nothing", func(t *testing.T) {
		ledger := inmem.NewLedger(1000, "topup")
		require.NoError(t, ledger.OpenPayout(ctx, domain.Payout{TransactionID: "p1", Amount: 400}))

		box, err := ledger.Balance(ctx)
		require.NoError(t, err)
		assert.EqualValues(t, 1000, box.Balance, "money leaves when the payout completes")
		assert.Equal(t, 860, box.Currency)
		assert.Equal(t, "topup", box.Kind)
	})

	t.Run("settling takes the money out once", func(t *testing.T) {
		ledger := inmem.NewLedger(1000, "topup")
		payout := domain.Payout{TransactionID: "p1", Amount: 400}

		require.NoError(t, ledger.SettlePayout(ctx, payout))
		require.NoError(t, ledger.SettlePayout(ctx, payout))

		box, err := ledger.Balance(ctx)
		require.NoError(t, err)
		assert.EqualValues(t, 600, box.Balance,
			"the protocol allows a settling call to be repeated; the balance may not move twice")
	})
}

func TestMerchant(t *testing.T) {
	ctx := context.Background()
	merchant := inmem.NewMerchant()

	require.NoError(t, merchant.CheckPerformTransaction(ctx, 100, nil))
	id, err := merchant.CreateTransaction(ctx, "abc", 0, 100, nil)
	require.NoError(t, err)
	assert.Equal(t, "merchant-abc", id)
	require.NoError(t, merchant.PerformTransaction(ctx, "abc"))
	require.NoError(t, merchant.CancelTransaction(ctx, "abc", 1))

	assert.Equal(t,
		[]string{"CheckPerformTransaction", "CreateTransaction", "PerformTransaction", "CancelTransaction"},
		merchant.Calls(), "the page shows the chain a real integration would have received")
}

func TestScheduler(t *testing.T) {
	assert.NoError(t, inmem.NewScheduler().ScheduleAdvance(context.Background(), "r1", 5000),
		"there is no worker in a browser tab, so the delay is dropped rather than faked")
}

func TestSMS(t *testing.T) {
	ctx := context.Background()
	sms := inmem.NewSMS()

	_, ok := sms.Last()
	assert.False(t, ok, "an empty outbox has no last message")
	assert.Empty(t, sms.Sent())

	require.NoError(t, sms.Send(ctx, "901234567", "666666"))
	require.NoError(t, sms.Send(ctx, "901234567", "second"))

	assert.Len(t, sms.Sent(), 2)
	last, ok := sms.Last()
	require.True(t, ok)
	assert.Equal(t, "second", last.Text)
	assert.Equal(t, "901234567", last.Phone)
}

func TestTokens(t *testing.T) {
	tokens := inmem.NewTokens()

	assert.Len(t, tokens.CardToken(), 64, "the provider issues 64-character tokens")
	assert.Len(t, tokens.ReceiptID(), 24, "and 24-character hex identifiers")
	assert.Len(t, tokens.TransactionID(), 24)

	assert.NotEqual(t, tokens.CardToken(), tokens.CardToken(),
		"two ids from one run must differ")

	t.Run("the same run always produces the same ids", func(t *testing.T) {
		first, second := inmem.NewTokens(), inmem.NewTokens()
		assert.Equal(t, first.ReceiptID(), second.ReceiptID(),
			"a reloaded page can be read against what it said before")
	})
}
