package application_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/application"
	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

const (
	amount        = int64(500000)
	docCardNumber = "8600069195406311"
	docCardMask   = "860006******6311"
	humoNumber    = "9860016101234567"
	// sharedCode is the stand's fallback OTP, which only a card whose expiry
	// spells no code ever sees.
	sharedCode = "666666"
	merchantID = "587f72c72cac0d162c722ae2"
)

// verifyCode is the OTP every card in these tests takes: its own expiry with
// the year repeated, which is what the mock sends.
var verifyCode = domain.ExpiryCode("0399")

var startTime = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

type stand struct {
	svc       *application.Service
	cards     *fakeCards
	receipts  *fakeReceipts
	merchant  *fakeMerchant
	scheduler *fakeScheduler
	sms       *fakeSMS
	clk       *clock.Fake
}

func newStand(t *testing.T) *stand {
	t.Helper()

	cards := newFakeCards()
	receipts := newFakeReceipts()
	merchant := &fakeMerchant{}
	scheduler := &fakeScheduler{}
	sms := &fakeSMS{}
	clk := clock.NewFake(startTime)

	svc := application.NewService(cards, receipts, merchant, scheduler, &fixedTokens{}, sms, clk,
		application.Settings{
			SandboxID:        1,
			MerchantID:       merchantID,
			VerifyCode:       sharedCode,
			VerifyWaitMillis: domain.DefaultVerifyWaitMillis,
			StepDelayMillis:  250,
			HoldWindowMillis: 30 * 24 * 60 * 60 * 1000, // 30 days
			CardBalance:      100_000_000,
		})

	return &stand{svc: svc, cards: cards, receipts: receipts, merchant: merchant,
		scheduler: scheduler, sms: sms, clk: clk}
}

func (s *stand) account() map[string]string { return map[string]string{"order_id": "197"} }

// verifiedCard tokenizes and verifies a card, the state paying requires.
func (s *stand) verifiedCard(t *testing.T, number string) string {
	t.Helper()
	ctx := context.Background()

	created, err := s.svc.CardsCreate(ctx, cardParams(number, "0399", true))
	require.NoError(t, err)

	s.cards.byToken[created.Card.Token].Phone = "998901304527"

	_, err = s.svc.CardsGetVerifyCode(ctx, application.CardsTokenParams{Token: created.Card.Token})
	require.NoError(t, err)

	_, err = s.svc.CardsVerify(ctx, application.CardsVerifyParams{Token: created.Card.Token, Code: verifyCode})
	require.NoError(t, err)

	return created.Card.Token
}

func (s *stand) receipt(t *testing.T) string {
	t.Helper()

	got, err := s.svc.ReceiptsCreate(context.Background(), application.ReceiptsCreateParams{
		Amount: amount, Account: s.account(),
	})
	require.NoError(t, err)
	return got.Receipt.ID
}

func cardParams(number, expire string, save bool) application.CardsCreateParams {
	var p application.CardsCreateParams
	p.Card.Number = number
	p.Card.Expire = expire
	p.Save = save
	return p
}

// ---------- cards ----------

func TestCardsCreate(t *testing.T) {
	ctx := context.Background()

	t.Run("returns a masked number and a token", func(t *testing.T) {
		s := newStand(t)

		got, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))

		require.NoError(t, err)
		assert.Equal(t, docCardMask, got.Card.Number, "the full number must never leave the mock")
		assert.Equal(t, "03/99", got.Card.Expire)
		assert.NotEmpty(t, got.Card.Token)
		assert.False(t, got.Card.Verify, "a fresh card is not yet verified")
	})

	t.Run("save=true makes the token reusable", func(t *testing.T) {
		s := newStand(t)

		got, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))

		require.NoError(t, err)
		assert.True(t, got.Card.Recurrent)
	})

	t.Run("save=false yields a single-use token", func(t *testing.T) {
		s := newStand(t)

		got, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", false))

		require.NoError(t, err)
		assert.False(t, got.Card.Recurrent)
	})

	t.Run("rejects a missing number", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.CardsCreate(ctx, cardParams("", "0399", true))

		assert.ErrorIs(t, err, payerr.ErrInvalidRequest)
	})

	t.Run("rejects a missing expiry", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "", true))

		assert.ErrorIs(t, err, payerr.ErrInvalidRequest)
	})

	t.Run("propagates a store failure", func(t *testing.T) {
		s := newStand(t)
		s.cards.failCreate = true

		_, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))

		assert.ErrorIs(t, err, errBoom)
	})

	// An operator rigs a card by its number; the integration then tokenizes
	// that number through the API. What comes back is that same card — one
	// card, one token, one balance — or the rigging would never fire.
	t.Run("hands back the card the stand already holds", func(t *testing.T) {
		s := newStand(t)
		s.cards.byToken["rigged"] = &domain.Card{
			ID: 42, Token: "rigged", NumberFull: docCardNumber, Expire: "03/99",
			Outcome: domain.OutcomeBlocked, SMSEnabled: false, Frozen: true,
			DelayMillis: 1500, Balance: 250,
		}

		got, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))

		require.NoError(t, err)
		assert.Equal(t, "rigged", got.Card.Token,
			"the card already has a token; a second one would be a second card")

		stored := s.cards.byToken["rigged"]
		assert.Equal(t, domain.OutcomeBlocked, stored.Outcome)
		assert.False(t, stored.SMSEnabled)
		assert.True(t, stored.Frozen)
		assert.Equal(t, int64(1500), stored.DelayMillis)
		assert.Equal(t, int64(250), stored.Balance, "the balance is the card's, not the call's")
		assert.Len(t, s.cards.byToken, 1, "no second row for the same number")
	})

	// What a repeat registration may still say is who the card is saved for,
	// and that the token is to be reusable.
	t.Run("a repeat registration updates what it is saved for", func(t *testing.T) {
		s := newStand(t)
		s.cards.byToken["held"] = &domain.Card{
			ID: 9, Token: "held", NumberFull: docCardNumber, Expire: "03/99",
		}

		p := cardParams(docCardNumber, "0399", true)
		p.Account = map[string]string{"order_id": "197"}
		p.Customer = "998901234567"

		got, err := s.svc.CardsCreate(ctx, p)

		require.NoError(t, err)
		assert.True(t, got.Card.Recurrent)

		stored := s.cards.byToken["held"]
		assert.Equal(t, map[string]string{"order_id": "197"}, stored.Account)
		assert.Equal(t, "998901234567", stored.Customer)
	})

	t.Run("a number the stand has never seen is tokenized fresh", func(t *testing.T) {
		s := newStand(t)

		got, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))

		require.NoError(t, err)
		stored := s.cards.byToken[got.Card.Token]
		assert.Equal(t, domain.Outcome(""), stored.Outcome)
		assert.True(t, stored.SMSEnabled)
	})

	t.Run("propagates a failed lookup of the held card", func(t *testing.T) {
		s := newStand(t)
		s.cards.failByNumber = true

		_, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))

		assert.ErrorIs(t, err, errBoom)
	})
}

func TestCardsGetVerifyCode(t *testing.T) {
	ctx := context.Background()

	t.Run("reports delivery with a masked phone and the wait window", func(t *testing.T) {
		s := newStand(t)
		created, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))
		require.NoError(t, err)
		s.cards.byToken[created.Card.Token].Phone = "998901304527"

		got, err := s.svc.CardsGetVerifyCode(ctx, application.CardsTokenParams{Token: created.Card.Token})

		require.NoError(t, err)
		assert.True(t, got.Sent)
		assert.Equal(t, int64(60000), got.Wait, "the documented OTP window")
		assert.Equal(t, "99890*****27", got.Phone)
		assert.NotContains(t, got.Phone, "1304")
	})

	t.Run("falls back to the documented window when unset", func(t *testing.T) {
		s := newStand(t)
		svc := application.NewService(s.cards, s.receipts, s.merchant, s.scheduler,
			&fixedTokens{}, s.sms, s.clk, application.Settings{VerifyCode: sharedCode})
		created, err := svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))
		require.NoError(t, err)

		got, err := svc.CardsGetVerifyCode(ctx, application.CardsTokenParams{Token: created.Card.Token})

		require.NoError(t, err)
		assert.Equal(t, domain.DefaultVerifyWaitMillis, got.Wait)
	})

	t.Run("an SMS failure does not fail the call", func(t *testing.T) {
		s := newStand(t)
		s.sms.fail = true
		created, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))
		require.NoError(t, err)
		s.cards.byToken[created.Card.Token].Phone = "998901304527"

		got, err := s.svc.CardsGetVerifyCode(ctx, application.CardsTokenParams{Token: created.Card.Token})

		require.NoError(t, err)
		assert.True(t, got.Sent)
	})

	t.Run("rejects an unknown token", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.CardsGetVerifyCode(ctx, application.CardsTokenParams{Token: "nope"})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	t.Run("rejects an empty token", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.CardsGetVerifyCode(ctx, application.CardsTokenParams{})

		assert.ErrorIs(t, err, payerr.ErrInvalidRequest)
	})

	t.Run("propagates a lookup failure", func(t *testing.T) {
		s := newStand(t)
		s.cards.failByToken = true

		_, err := s.svc.CardsGetVerifyCode(ctx, application.CardsTokenParams{Token: "x"})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("propagates an update failure", func(t *testing.T) {
		s := newStand(t)
		created, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))
		require.NoError(t, err)
		s.cards.failUpdate = true

		_, err = s.svc.CardsGetVerifyCode(ctx, application.CardsTokenParams{Token: created.Card.Token})

		assert.ErrorIs(t, err, errBoom)
	})

	// A card the bank stopped is refused before an OTP is spent on it, which
	// is where the real provider refuses one too.
	t.Run("a card that refuses everything gets no code", func(t *testing.T) {
		for _, outcome := range []domain.Outcome{domain.OutcomeBlocked, domain.OutcomeExpired} {
			t.Run(string(outcome), func(t *testing.T) {
				s := newStand(t)
				created, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))
				require.NoError(t, err)
				s.cards.byToken[created.Card.Token].Outcome = outcome

				_, err = s.svc.CardsGetVerifyCode(ctx, application.CardsTokenParams{Token: created.Card.Token})

				assert.ErrorIs(t, err, payerr.ErrCannotPerform)
				assert.Empty(t, s.cards.byToken[created.Card.Token].VerifyCode,
					"no code may be issued to a card that cannot be operated")
			})
		}
	})
}

func TestCardsVerify(t *testing.T) {
	ctx := context.Background()

	t.Run("the right code verifies the card", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)

		got, err := s.svc.CardsCheck(ctx, application.CardsTokenParams{Token: token})

		require.NoError(t, err)
		assert.True(t, got.Card.Verify)
	})

	t.Run("the wrong code is refused", func(t *testing.T) {
		s := newStand(t)
		created, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))
		require.NoError(t, err)
		_, err = s.svc.CardsGetVerifyCode(ctx, application.CardsTokenParams{Token: created.Card.Token})
		require.NoError(t, err)

		_, err = s.svc.CardsVerify(ctx, application.CardsVerifyParams{Token: created.Card.Token, Code: "000000"})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	// The documented `wait` is a real deadline.
	t.Run("the right code past its window is refused", func(t *testing.T) {
		s := newStand(t)
		created, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))
		require.NoError(t, err)
		_, err = s.svc.CardsGetVerifyCode(ctx, application.CardsTokenParams{Token: created.Card.Token})
		require.NoError(t, err)

		s.clk.Advance(61 * time.Second)

		_, err = s.svc.CardsVerify(ctx, application.CardsVerifyParams{Token: created.Card.Token, Code: verifyCode})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	t.Run("propagates an update failure", func(t *testing.T) {
		s := newStand(t)
		created, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))
		require.NoError(t, err)
		_, err = s.svc.CardsGetVerifyCode(ctx, application.CardsTokenParams{Token: created.Card.Token})
		require.NoError(t, err)
		s.cards.failUpdate = true

		_, err = s.svc.CardsVerify(ctx, application.CardsVerifyParams{Token: created.Card.Token, Code: verifyCode})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("propagates a lookup failure", func(t *testing.T) {
		s := newStand(t)
		s.cards.failByToken = true

		_, err := s.svc.CardsVerify(ctx, application.CardsVerifyParams{Token: "x", Code: verifyCode})

		assert.ErrorIs(t, err, errBoom)
	})
}

func TestCardsCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("returns the card", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)

		got, err := s.svc.CardsCheck(ctx, application.CardsTokenParams{Token: token})

		require.NoError(t, err)
		assert.Equal(t, docCardMask, got.Card.Number)
	})

	t.Run("rejects an unknown token", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.CardsCheck(ctx, application.CardsTokenParams{Token: "nope"})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})
}

func TestCardsRemove(t *testing.T) {
	ctx := context.Background()

	t.Run("removes the token and stops it paying", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		receiptID := s.receipt(t)

		got, err := s.svc.CardsRemove(ctx, application.CardsTokenParams{Token: token})
		require.NoError(t, err)
		assert.True(t, got.Success)

		_, err = s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})
		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	t.Run("rejects an unknown token", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.CardsRemove(ctx, application.CardsTokenParams{Token: "nope"})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	t.Run("propagates an update failure", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		s.cards.failUpdate = true

		_, err := s.svc.CardsRemove(ctx, application.CardsTokenParams{Token: token})

		assert.ErrorIs(t, err, errBoom)
	})
}

// ---------- receipts ----------

func TestReceiptsCreate(t *testing.T) {
	ctx := context.Background()

	t.Run("opens a receipt awaiting payment", func(t *testing.T) {
		s := newStand(t)

		got, err := s.svc.ReceiptsCreate(ctx, application.ReceiptsCreateParams{
			Amount: amount, Account: s.account(), Description: "order 197",
		})

		require.NoError(t, err)
		assert.Equal(t, domain.StateCreated, got.Receipt.State)
		assert.Equal(t, domain.CurrencyUZS, got.Receipt.Currency)
		assert.Equal(t, amount, got.Receipt.Amount)
		assert.NotEmpty(t, got.Receipt.ID)
	})

	t.Run("asks the merchant before opening the receipt", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.ReceiptsCreate(ctx, application.ReceiptsCreateParams{
			Amount: amount, Account: s.account(),
		})

		require.NoError(t, err)
		assert.Equal(t, []string{"CheckPerformTransaction"}, s.merchant.calls)
	})

	t.Run("a merchant refusal prevents the receipt", func(t *testing.T) {
		s := newStand(t)
		s.merchant.failCheck = payerr.ErrInvalidAmount

		_, err := s.svc.ReceiptsCreate(ctx, application.ReceiptsCreateParams{
			Amount: amount, Account: s.account(),
		})

		assert.ErrorIs(t, err, payerr.ErrInvalidAmount)
		assert.Empty(t, s.receipts.byID)
	})

	t.Run("rejects a non-positive amount", func(t *testing.T) {
		s := newStand(t)

		for _, bad := range []int64{0, -1} {
			_, err := s.svc.ReceiptsCreate(ctx, application.ReceiptsCreateParams{
				Amount: bad, Account: s.account(),
			})

			assert.ErrorIs(t, err, payerr.ErrInvalidAmount)
		}
	})

	t.Run("propagates a store failure", func(t *testing.T) {
		s := newStand(t)
		s.receipts.failCreate = true

		_, err := s.svc.ReceiptsCreate(ctx, application.ReceiptsCreateParams{
			Amount: amount, Account: s.account(),
		})

		assert.ErrorIs(t, err, errBoom)
	})
}

// The contract between the two protocols: paying a receipt drives the
// merchant's Merchant API through the documented sequence.
func TestReceiptsPayDrivesTheMerchantAPISequence(t *testing.T) {
	s := newStand(t)
	ctx := context.Background()
	token := s.verifiedCard(t, docCardNumber)
	receiptID := s.receipt(t)
	s.merchant.calls = nil

	_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})

	require.NoError(t, err)
	assert.Equal(t, []string{
		"CheckPerformTransaction",
		"CreateTransaction",
		"PerformTransaction",
	}, s.merchant.calls)
}

func TestReceiptsPay(t *testing.T) {
	ctx := context.Background()

	t.Run("charges the card and leaves the receipt mid-walk", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		receiptID := s.receipt(t)
		before := s.cards.byToken[token].Balance

		got, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})

		require.NoError(t, err)
		assert.Equal(t, domain.StateChecking, got.Receipt.State,
			"payment starts the walk rather than jumping to paid")
		assert.Equal(t, before-amount, s.cards.byToken[token].Balance)
	})

	t.Run("queues the background walk", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		receiptID := s.receipt(t)

		_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})

		require.NoError(t, err)
		assert.Equal(t, []string{receiptID}, s.scheduler.queued)
	})

	t.Run("records the payer", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		receiptID := s.receipt(t)

		_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{
			ID: receiptID, Token: token,
			Payer: map[string]any{"phone": "998901304527"},
		})

		require.NoError(t, err)
		assert.Equal(t, "998901304527", s.receipts.byID[receiptID].Payer["phone"])
	})

	t.Run("detects the card network", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, humoNumber)
		receiptID := s.receipt(t)

		_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})

		require.NoError(t, err)
		assert.Equal(t, domain.CardHumo, s.receipts.byID[receiptID].CardSystem)
	})

	t.Run("refuses an unverified card", func(t *testing.T) {
		s := newStand(t)
		created, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))
		require.NoError(t, err)
		receiptID := s.receipt(t)

		_, err = s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: created.Card.Token})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	t.Run("refuses when the balance is short", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		s.cards.byToken[token].Balance = amount - 1
		receiptID := s.receipt(t)

		_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	t.Run("refuses to pay a receipt twice", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		receiptID := s.receipt(t)
		_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})
		require.NoError(t, err)

		_, err = s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	t.Run("reports an unknown receipt", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)

		_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: "nope", Token: token})

		assert.ErrorIs(t, err, payerr.ErrTransactionNotFound)
	})

	t.Run("reports an unknown card", func(t *testing.T) {
		s := newStand(t)
		receiptID := s.receipt(t)

		_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: "nope"})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	failures := []struct {
		name  string
		setup func(*stand)
		want  error
	}{
		{"merchant refuses the check", func(s *stand) { s.merchant.failCheck = payerr.ErrCannotPerform }, payerr.ErrCannotPerform},
		{"merchant refuses to create", func(s *stand) { s.merchant.failCreate = payerr.ErrCannotPerform }, payerr.ErrCannotPerform},
		{"merchant refuses to perform", func(s *stand) { s.merchant.failPerform = payerr.ErrCannotPerform }, payerr.ErrCannotPerform},
		{"the card store fails", func(s *stand) { s.cards.failUpdate = true }, errBoom},
		{"the receipt store fails", func(s *stand) { s.receipts.failUpdate = true }, errBoom},
		{"the scheduler fails", func(s *stand) { s.scheduler.fail = true }, errBoom},
	}

	for _, tt := range failures {
		t.Run(tt.name, func(t *testing.T) {
			s := newStand(t)
			token := s.verifiedCard(t, docCardNumber)
			receiptID := s.receipt(t)
			tt.setup(s)

			_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})

			assert.ErrorIs(t, err, tt.want)
		})
	}
}

// The receipt reaches paid only after the background walk, one state at a time.
func TestBackgroundWalkReachesPaidStepByStep(t *testing.T) {
	s := newStand(t)
	ctx := context.Background()
	token := s.verifiedCard(t, docCardNumber)
	receiptID := s.receipt(t)
	_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})
	require.NoError(t, err)

	var walked []domain.ReceiptState
	walked = append(walked, s.receipts.byID[receiptID].State)

	for i := 0; i < 10; i++ {
		require.NoError(t, s.svc.AdvanceReceipt(ctx, receiptID))
		state := s.receipts.byID[receiptID].State
		if state == walked[len(walked)-1] {
			break
		}
		walked = append(walked, state)
	}

	assert.Equal(t, []domain.ReceiptState{
		domain.StateChecking,
		domain.StateWithdrawn,
		domain.StateClosing,
		domain.StatePaid,
	}, walked)
}

func TestAdvanceReceipt(t *testing.T) {
	ctx := context.Background()

	t.Run("a settled receipt schedules nothing further", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		receiptID := s.receipt(t)
		_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})
		require.NoError(t, err)
		for i := 0; i < 5; i++ {
			require.NoError(t, s.svc.AdvanceReceipt(ctx, receiptID))
		}
		queuedWhenPaid := len(s.scheduler.queued)

		require.NoError(t, s.svc.AdvanceReceipt(ctx, receiptID))

		assert.Equal(t, queuedWhenPaid, len(s.scheduler.queued))
	})

	t.Run("reports an unknown receipt", func(t *testing.T) {
		s := newStand(t)

		err := s.svc.AdvanceReceipt(ctx, "nope")

		assert.ErrorIs(t, err, payerr.ErrTransactionNotFound)
	})

	t.Run("propagates an update failure", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		receiptID := s.receipt(t)
		_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})
		require.NoError(t, err)
		s.receipts.failUpdate = true

		assert.ErrorIs(t, s.svc.AdvanceReceipt(ctx, receiptID), errBoom)
	})

	t.Run("propagates a scheduling failure", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		receiptID := s.receipt(t)
		_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})
		require.NoError(t, err)
		s.scheduler.fail = true

		assert.ErrorIs(t, s.svc.AdvanceReceipt(ctx, receiptID), errBoom)
	})
}

// ---------- holding ----------

func TestHoldStopsShortOfPaidAndConfirmCompletesIt(t *testing.T) {
	s := newStand(t)
	ctx := context.Background()
	token := s.verifiedCard(t, docCardNumber)
	receiptID := s.receipt(t)

	_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token, Hold: true})
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		require.NoError(t, s.svc.AdvanceReceipt(ctx, receiptID))
	}
	require.Equal(t, domain.StateHeld, s.receipts.byID[receiptID].State)

	got, err := s.svc.ReceiptsConfirmHold(ctx, application.ReceiptsIDParams{ID: receiptID})

	require.NoError(t, err)
	assert.Equal(t, domain.StatePaid, got.Receipt.State)
}

func TestHoldRecordsItsExpiry(t *testing.T) {
	s := newStand(t)
	token := s.verifiedCard(t, docCardNumber)
	receiptID := s.receipt(t)

	_, err := s.svc.ReceiptsPay(context.Background(), application.ReceiptsPayParams{
		ID: receiptID, Token: token, Hold: true,
	})

	require.NoError(t, err)
	r := s.receipts.byID[receiptID]
	assert.Equal(t, s.clk.NowMillis()+30*24*60*60*1000, r.HoldExpire, "Uzcard releases a hold after 30 days")
	assert.False(t, r.HoldExpired(s.clk.NowMillis()))
	assert.True(t, r.HoldExpired(r.HoldExpire+1))
}

func TestConfirmHold(t *testing.T) {
	ctx := context.Background()

	t.Run("refuses a receipt that is not held", func(t *testing.T) {
		s := newStand(t)
		receiptID := s.receipt(t)

		_, err := s.svc.ReceiptsConfirmHold(ctx, application.ReceiptsIDParams{ID: receiptID})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	t.Run("reports an unknown receipt", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.ReceiptsConfirmHold(ctx, application.ReceiptsIDParams{ID: "nope"})

		assert.ErrorIs(t, err, payerr.ErrTransactionNotFound)
	})

	t.Run("propagates an update failure", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		receiptID := s.receipt(t)
		_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token, Hold: true})
		require.NoError(t, err)
		for i := 0; i < 5; i++ {
			require.NoError(t, s.svc.AdvanceReceipt(ctx, receiptID))
		}
		s.receipts.failUpdate = true

		_, err = s.svc.ReceiptsConfirmHold(ctx, application.ReceiptsIDParams{ID: receiptID})

		assert.ErrorIs(t, err, errBoom)
	})
}

// ---------- cancellation ----------

// Cancelling a paid receipt queues state 21 and only the background step
// reaches 50, exactly as the provider behaves.
func TestCancelQueuesThenCompletes(t *testing.T) {
	s := newStand(t)
	ctx := context.Background()
	token := s.verifiedCard(t, docCardNumber)
	receiptID := s.receipt(t)
	_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		require.NoError(t, s.svc.AdvanceReceipt(ctx, receiptID))
	}
	require.Equal(t, domain.StatePaid, s.receipts.byID[receiptID].State)

	got, err := s.svc.ReceiptsCancel(ctx, application.ReceiptsIDParams{ID: receiptID})
	require.NoError(t, err)
	assert.Equal(t, domain.StateCancelQueued, got.Receipt.State)

	require.NoError(t, s.svc.AdvanceReceipt(ctx, receiptID))
	assert.Equal(t, domain.StateCancelled, s.receipts.byID[receiptID].State)
}

func TestCancelRefundsTheCard(t *testing.T) {
	s := newStand(t)
	ctx := context.Background()
	token := s.verifiedCard(t, docCardNumber)
	before := s.cards.byToken[token].Balance
	receiptID := s.receipt(t)
	_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})
	require.NoError(t, err)
	require.Equal(t, before-amount, s.cards.byToken[token].Balance)

	_, err = s.svc.ReceiptsCancel(ctx, application.ReceiptsIDParams{ID: receiptID})

	require.NoError(t, err)
	assert.Equal(t, before, s.cards.byToken[token].Balance)
}

func TestReceiptsCancel(t *testing.T) {
	ctx := context.Background()

	t.Run("an unpaid receipt cancels outright", func(t *testing.T) {
		s := newStand(t)
		receiptID := s.receipt(t)

		got, err := s.svc.ReceiptsCancel(ctx, application.ReceiptsIDParams{ID: receiptID})

		require.NoError(t, err)
		assert.Equal(t, domain.StateCancelled, got.Receipt.State)
	})

	t.Run("repeating a cancellation changes nothing", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		receiptID := s.receipt(t)
		_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})
		require.NoError(t, err)

		first, err := s.svc.ReceiptsCancel(ctx, application.ReceiptsIDParams{ID: receiptID})
		require.NoError(t, err)
		balanceAfterRefund := s.cards.byToken[token].Balance

		s.clk.Advance(time.Hour)
		second, err := s.svc.ReceiptsCancel(ctx, application.ReceiptsIDParams{ID: receiptID})

		require.NoError(t, err)
		assert.Equal(t, first, second)
		assert.Equal(t, balanceAfterRefund, s.cards.byToken[token].Balance,
			"a repeated cancellation must not refund twice")
	})

	t.Run("reports an unknown receipt", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.ReceiptsCancel(ctx, application.ReceiptsIDParams{ID: "nope"})

		assert.ErrorIs(t, err, payerr.ErrTransactionNotFound)
	})

	t.Run("refuses a paused receipt", func(t *testing.T) {
		s := newStand(t)
		receiptID := s.receipt(t)
		s.receipts.byID[receiptID].State = domain.StatePaused

		_, err := s.svc.ReceiptsCancel(ctx, application.ReceiptsIDParams{ID: receiptID})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	t.Run("tolerates a card that has vanished", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		receiptID := s.receipt(t)
		_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})
		require.NoError(t, err)
		s.cards.missingByID = true

		_, err = s.svc.ReceiptsCancel(ctx, application.ReceiptsIDParams{ID: receiptID})

		assert.NoError(t, err)
	})

	t.Run("propagates a card lookup failure", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		receiptID := s.receipt(t)
		_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})
		require.NoError(t, err)
		s.cards.failByID = true

		_, err = s.svc.ReceiptsCancel(ctx, application.ReceiptsIDParams{ID: receiptID})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("propagates a receipt update failure", func(t *testing.T) {
		s := newStand(t)
		receiptID := s.receipt(t)
		s.receipts.failUpdate = true

		_, err := s.svc.ReceiptsCancel(ctx, application.ReceiptsIDParams{ID: receiptID})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("propagates a scheduling failure", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		receiptID := s.receipt(t)
		_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})
		require.NoError(t, err)
		s.scheduler.fail = true

		_, err = s.svc.ReceiptsCancel(ctx, application.ReceiptsIDParams{ID: receiptID})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("propagates a refund update failure", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		receiptID := s.receipt(t)
		_, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{ID: receiptID, Token: token})
		require.NoError(t, err)
		s.cards.failUpdate = true

		_, err = s.svc.ReceiptsCancel(ctx, application.ReceiptsIDParams{ID: receiptID})

		assert.ErrorIs(t, err, errBoom)
	})
}

// ---------- queries ----------

func TestReceiptsCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("returns only the state", func(t *testing.T) {
		s := newStand(t)
		receiptID := s.receipt(t)

		got, err := s.svc.ReceiptsCheck(ctx, application.ReceiptsIDParams{ID: receiptID})

		require.NoError(t, err)
		assert.Equal(t, domain.StateCreated, got.State)
	})

	t.Run("reports an unknown receipt", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.ReceiptsCheck(ctx, application.ReceiptsIDParams{ID: "nope"})

		assert.ErrorIs(t, err, payerr.ErrTransactionNotFound)
	})

	t.Run("propagates a store failure", func(t *testing.T) {
		s := newStand(t)
		s.receipts.failByID = true

		_, err := s.svc.ReceiptsCheck(ctx, application.ReceiptsIDParams{ID: "x"})

		assert.ErrorIs(t, err, errBoom)
	})
}

func TestReceiptsGet(t *testing.T) {
	ctx := context.Background()

	t.Run("returns the receipt in full", func(t *testing.T) {
		s := newStand(t)
		receiptID := s.receipt(t)

		got, err := s.svc.ReceiptsGet(ctx, application.ReceiptsIDParams{ID: receiptID})

		require.NoError(t, err)
		assert.Equal(t, receiptID, got.Receipt.ID)
		assert.Equal(t, merchantID, got.Receipt.Merchant.ID)
	})

	t.Run("reports an unknown receipt", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.ReceiptsGet(ctx, application.ReceiptsIDParams{ID: "nope"})

		assert.ErrorIs(t, err, payerr.ErrTransactionNotFound)
	})

	t.Run("rejects an empty id", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.ReceiptsGet(ctx, application.ReceiptsIDParams{})

		assert.ErrorIs(t, err, payerr.ErrInvalidRequest)
	})
}

func TestReceiptsGetAll(t *testing.T) {
	ctx := context.Background()

	// The documentation caps count at fifty; the mock enforces it too.
	t.Run("refuses a count above fifty", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.ReceiptsGetAll(ctx, application.ReceiptsGetAllParams{Count: 51, From: 1, To: 2})

		assert.ErrorIs(t, err, payerr.ErrInvalidRequest)
	})

	t.Run("accepts exactly fifty", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.ReceiptsGetAll(ctx, application.ReceiptsGetAllParams{Count: 50, From: 1, To: 2})

		assert.NoError(t, err)
	})

	t.Run("an unset count falls back to the ceiling", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.ReceiptsGetAll(ctx, application.ReceiptsGetAllParams{From: 1, To: 2})

		require.NoError(t, err)
		assert.Equal(t, application.MaxGetAllCount, s.receipts.lastList.count)
	})

	t.Run("passes the period and offset through", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.ReceiptsGetAll(ctx, application.ReceiptsGetAllParams{
			Count: 10, From: 100, To: 200, Offset: 5,
		})

		require.NoError(t, err)
		assert.Equal(t, int64(100), s.receipts.lastList.from)
		assert.Equal(t, int64(200), s.receipts.lastList.to)
		assert.Equal(t, 10, s.receipts.lastList.count)
		assert.Equal(t, 5, s.receipts.lastList.offset)
	})

	t.Run("an empty period returns an empty list", func(t *testing.T) {
		s := newStand(t)

		got, err := s.svc.ReceiptsGetAll(ctx, application.ReceiptsGetAllParams{Count: 10, From: 1, To: 2})

		require.NoError(t, err)
		assert.NotNil(t, got)
		assert.Empty(t, got)
	})

	t.Run("renders every receipt", func(t *testing.T) {
		s := newStand(t)
		s.receipts.listed = []*domain.Receipt{
			domain.NewReceipt(1, "a", merchantID, 100, nil, 1),
			domain.NewReceipt(1, "b", merchantID, 200, nil, 2),
		}

		got, err := s.svc.ReceiptsGetAll(ctx, application.ReceiptsGetAllParams{Count: 10, From: 1, To: 2})

		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, "a", got[0].ID)
		assert.Equal(t, int64(200), got[1].Amount)
	})

	t.Run("propagates a store failure", func(t *testing.T) {
		s := newStand(t)
		s.receipts.failList = true

		_, err := s.svc.ReceiptsGetAll(ctx, application.ReceiptsGetAllParams{Count: 10, From: 1, To: 2})

		assert.ErrorIs(t, err, errBoom)
	})
}

func TestReceiptsSend(t *testing.T) {
	ctx := context.Background()

	t.Run("delivers the receipt", func(t *testing.T) {
		s := newStand(t)
		receiptID := s.receipt(t)

		got, err := s.svc.ReceiptsSend(ctx, application.ReceiptsSendParams{
			ID: receiptID, Phone: "998901304527",
		})

		require.NoError(t, err)
		assert.True(t, got.Success)
		assert.Equal(t, []string{"998901304527:" + receiptID}, s.sms.sent)
	})

	t.Run("reports failure rather than erroring", func(t *testing.T) {
		s := newStand(t)
		receiptID := s.receipt(t)
		s.sms.fail = true

		got, err := s.svc.ReceiptsSend(ctx, application.ReceiptsSendParams{
			ID: receiptID, Phone: "998901304527",
		})

		require.NoError(t, err)
		assert.False(t, got.Success)
	})

	t.Run("rejects a missing phone", func(t *testing.T) {
		s := newStand(t)
		receiptID := s.receipt(t)

		_, err := s.svc.ReceiptsSend(ctx, application.ReceiptsSendParams{ID: receiptID})

		assert.ErrorIs(t, err, payerr.ErrInvalidRequest)
	})

	t.Run("reports an unknown receipt", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.ReceiptsSend(ctx, application.ReceiptsSendParams{ID: "nope", Phone: "998901304527"})

		assert.ErrorIs(t, err, payerr.ErrTransactionNotFound)
	})
}

// The provider answers with the account as a list of labelled fields, not as
// the object the receipt was created with. A client written against the real
// API decodes that list, and an object fails it outright.
func TestReceiptAccountIsAListOfFields(t *testing.T) {
	s := newStand(t)

	got, err := s.svc.ReceiptsCreate(context.Background(), application.ReceiptsCreateParams{
		Amount: amount,
		Account: map[string]string{
			"order_id":  "197",
			"full_name": "ABROR GAFUROV",
		},
	})

	require.NoError(t, err)
	// Sorted by field name, so two identical receipts never answer differently.
	// The first field is the main one, and a field the provider has no label
	// for is titled by its own name in all three languages rather than blank.
	assert.Equal(t, []application.AccountFieldView{
		{
			Name:  "full_name",
			Title: payerr.NewMessage("full_name", "full_name", "full_name"),
			Value: "ABROR GAFUROV",
			Main:  true,
		},
		{
			Name:  "order_id",
			Title: payerr.NewMessage("Код заказа", "Buyurtma kodi", "Order code"),
			Value: "197",
		},
	}, got.Receipt.Account)
}

// An account with no fields is still a list: a null would make a client
// decoding an array fail on a receipt that is otherwise fine.
func TestReceiptAccountIsNeverNull(t *testing.T) {
	s := newStand(t)

	got, err := s.svc.ReceiptsCreate(context.Background(), application.ReceiptsCreateParams{
		Amount: amount, Account: map[string]string{},
	})

	require.NoError(t, err)
	assert.NotNil(t, got.Receipt.Account)
	assert.Empty(t, got.Receipt.Account)
}

// ---------- payouts ----------

// payout opens a payout to a verified card and returns its id and token.
func (s *stand) payout(t *testing.T) (string, string) {
	t.Helper()

	token := s.verifiedCard(t, docCardNumber)

	created, err := s.svc.TransactionsCreate(context.Background(), application.TransactionsCreateParams{
		Amount:  amount,
		Token:   token,
		Account: s.account(),
	})
	require.NoError(t, err)

	return created.Receipt.ID, token
}

func TestTransactionsCreate(t *testing.T) {
	ctx := context.Background()

	t.Run("opens a payout awaiting settlement", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)

		got, err := s.svc.TransactionsCreate(ctx, application.TransactionsCreateParams{
			Amount: amount, Token: token, Account: s.account(),
		})

		require.NoError(t, err)
		assert.Equal(t, domain.StateCreated, got.Receipt.State)
		assert.Equal(t, domain.TypeCardPayment, got.Receipt.Type,
			"the provider reports one type for a card receipt, payout or not")
		assert.Equal(t, amount, got.Receipt.Amount)
		assert.NotEmpty(t, got.Receipt.ID)
	})

	// Nothing was bought, so there is no order for the merchant to be asked
	// about. Asking would refuse every payout on a stand with no such order.
	t.Run("never asks the merchant", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)

		_, err := s.svc.TransactionsCreate(ctx, application.TransactionsCreateParams{
			Amount: amount, Token: token, Account: s.account(),
		})

		require.NoError(t, err)
		assert.Empty(t, s.merchant.calls)
	})

	// The balance is what the card holds, not a limit on what may arrive.
	t.Run("a card with no balance can still be paid out to", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		s.cards.byToken[token].Balance = 0

		got, err := s.svc.TransactionsCreate(ctx, application.TransactionsCreateParams{
			Amount: amount, Token: token, Account: s.account(),
		})

		require.NoError(t, err)
		assert.Equal(t, amount, got.Receipt.Amount)
	})

	// A payout goes to a token minted for the paying-out register, and only the
	// register that saves a card ever confirms it with an OTP. Requiring a
	// verification here would refuse every payout an integration makes.
	t.Run("an unverified token can be paid out to", func(t *testing.T) {
		s := newStand(t)

		created, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", false))
		require.NoError(t, err)

		got, err := s.svc.TransactionsCreate(ctx, application.TransactionsCreateParams{
			Amount: amount, Token: created.Card.Token, Account: s.account(),
		})

		require.NoError(t, err)
		assert.Equal(t, amount, got.Receipt.Amount)
	})

	// A card the bank stopped takes no money either way.
	t.Run("a card that refuses everything is refused", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		s.cards.byToken[token].Outcome = domain.OutcomeBlocked

		_, err := s.svc.TransactionsCreate(ctx, application.TransactionsCreateParams{
			Amount: amount, Token: token, Account: s.account(),
		})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	t.Run("a non-positive amount is refused", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)

		for _, bad := range []int64{0, -1} {
			_, err := s.svc.TransactionsCreate(ctx, application.TransactionsCreateParams{
				Amount: bad, Token: token, Account: s.account(),
			})

			assert.ErrorIs(t, err, payerr.ErrInvalidAmount, "amount %d", bad)
		}
	})

	t.Run("an unknown card is refused", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.TransactionsCreate(ctx, application.TransactionsCreateParams{
			Amount: amount, Token: "nobody", Account: s.account(),
		})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})
}

func TestTransactionsComplete(t *testing.T) {
	ctx := context.Background()

	t.Run("settles the payout and credits the card", func(t *testing.T) {
		s := newStand(t)
		id, token := s.payout(t)
		before := s.cards.byToken[token].Balance

		got, err := s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
			ID: id, Amount: amount, Token: token, Account: s.account(),
		})

		require.NoError(t, err)
		assert.Equal(t, domain.StatePaid, got.Receipt.State)
		assert.Equal(t, before+amount, s.cards.byToken[token].Balance)
	})

	// The caller cannot tell a lost response from a lost payout, so it retries.
	// A retry must return the settled payout rather than pay it again.
	t.Run("settling twice pays once", func(t *testing.T) {
		s := newStand(t)
		id, token := s.payout(t)
		before := s.cards.byToken[token].Balance

		params := application.TransactionsCompleteParams{
			ID: id, Amount: amount, Token: token, Account: s.account(),
		}
		first, err := s.svc.TransactionsComplete(ctx, params)
		require.NoError(t, err)

		second, err := s.svc.TransactionsComplete(ctx, params)

		require.NoError(t, err)
		assert.Equal(t, first.Receipt.ID, second.Receipt.ID)
		assert.Equal(t, domain.StatePaid, second.Receipt.State)
		assert.Equal(t, before+amount, s.cards.byToken[token].Balance)
	})

	t.Run("a different amount is refused", func(t *testing.T) {
		s := newStand(t)
		id, token := s.payout(t)
		before := s.cards.byToken[token].Balance

		_, err := s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
			ID: id, Amount: amount + 1, Token: token, Account: s.account(),
		})

		assert.ErrorIs(t, err, payerr.ErrInvalidAmount)
		assert.Equal(t, before, s.cards.byToken[token].Balance)
	})

	// A payout settles to the card it was opened for; another card would be a
	// different payout however well the amount matches.
	t.Run("another card is refused", func(t *testing.T) {
		s := newStand(t)
		id, _ := s.payout(t)
		other := s.verifiedCard(t, humoNumber)

		_, err := s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
			ID: id, Amount: amount, Token: other, Account: s.account(),
		})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	// A payment is closed by paying it, never by settling it as a payout: the
	// two move money in opposite directions.
	t.Run("a payment cannot be completed as a payout", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		receiptID := s.receipt(t)

		_, err := s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
			ID: receiptID, Amount: amount, Token: token, Account: s.account(),
		})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	t.Run("an unknown payout is refused", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
			ID: "nope", Amount: amount, Token: "nobody", Account: s.account(),
		})

		assert.Error(t, err)
	})
}

// A stand whose storage is down must say so rather than report a payout that
// was never written, or a settled one that was never recorded.
func TestPayoutStorageFailuresSurface(t *testing.T) {
	ctx := context.Background()

	t.Run("the payout cannot be stored", func(t *testing.T) {
		s := newStand(t)
		token := s.verifiedCard(t, docCardNumber)
		s.receipts.failCreate = true

		_, err := s.svc.TransactionsCreate(ctx, application.TransactionsCreateParams{
			Amount: amount, Token: token, Account: s.account(),
		})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("the credited card cannot be stored", func(t *testing.T) {
		s := newStand(t)
		id, token := s.payout(t)
		s.cards.failUpdate = true

		_, err := s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
			ID: id, Amount: amount, Token: token, Account: s.account(),
		})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("the settled payout cannot be stored", func(t *testing.T) {
		s := newStand(t)
		id, token := s.payout(t)
		s.receipts.failUpdate = true

		_, err := s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
			ID: id, Amount: amount, Token: token, Account: s.account(),
		})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("the payout cannot be read", func(t *testing.T) {
		s := newStand(t)
		id, token := s.payout(t)
		s.receipts.failByID = true

		_, err := s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
			ID: id, Amount: amount, Token: token, Account: s.account(),
		})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("the card cannot be read", func(t *testing.T) {
		s := newStand(t)
		id, token := s.payout(t)
		s.cards.failByToken = true

		_, err := s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
			ID: id, Amount: amount, Token: token, Account: s.account(),
		})

		assert.ErrorIs(t, err, errBoom)
	})
}

// A removed card is gone: money must not be sent to it.
func TestPayoutToARemovedCardIsRefused(t *testing.T) {
	s := newStand(t)
	token := s.verifiedCard(t, docCardNumber)
	s.cards.byToken[token].Removed = true

	_, err := s.svc.TransactionsCreate(context.Background(), application.TransactionsCreateParams{
		Amount: amount, Token: token, Account: s.account(),
	})

	assert.ErrorIs(t, err, payerr.ErrCannotPerform)
}

// A payout already cancelled is not waiting to be settled.
func TestCompletingACancelledPayoutIsRefused(t *testing.T) {
	ctx := context.Background()
	s := newStand(t)
	id, token := s.payout(t)

	_, err := s.svc.ReceiptsCancel(ctx, application.ReceiptsIDParams{ID: id})
	require.NoError(t, err)

	_, err = s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
		ID: id, Amount: amount, Token: token, Account: s.account(),
	})

	assert.ErrorIs(t, err, payerr.ErrCannotPerform)
}

// The payout already names its card, so a caller that leaves the token out is
// not refused for it: an integration settling by receipt id alone still gets
// its money.
func TestCompletingAPayoutWithoutATokenSettlesIt(t *testing.T) {
	ctx := context.Background()
	s := newStand(t)
	id, token := s.payout(t)
	before := s.cards.byToken[token].Balance

	got, err := s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
		ID: id, Amount: amount, Account: s.account(),
	})

	require.NoError(t, err)
	assert.Equal(t, domain.StatePaid, got.Receipt.State)
	assert.Equal(t, before+amount, s.cards.byToken[token].Balance)
}

// A payout whose card is gone cannot be settled: there is nowhere to send it.
func TestCompletingAPayoutWithAMissingCardIsRefused(t *testing.T) {
	ctx := context.Background()

	t.Run("the card was deleted", func(t *testing.T) {
		s := newStand(t)
		id, token := s.payout(t)
		delete(s.cards.byID, s.cards.byToken[token].ID)

		_, err := s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
			ID: id, Amount: amount, Account: s.account(),
		})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	t.Run("the card cannot be read", func(t *testing.T) {
		s := newStand(t)
		id, _ := s.payout(t)
		s.cards.failByID = true

		_, err := s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
			ID: id, Amount: amount, Account: s.account(),
		})

		assert.ErrorIs(t, err, errBoom)
	})

	// A payment carries no card until it is paid, and one settled as a payout
	// would have nothing to credit.
	t.Run("the receipt never had a card", func(t *testing.T) {
		s := newStand(t)
		id, _ := s.payout(t)
		s.receipts.byID[id].CardID = nil

		_, err := s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
			ID: id, Amount: amount, Account: s.account(),
		})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})
}

// The documented receipt carries every field on every response, including the
// ones a stand has nothing to say about. A client decoding the full object
// must find them present and null rather than missing.
func TestReceiptViewMatchesTheDocumentedShape(t *testing.T) {
	ctx := context.Background()
	s := newStand(t)
	receiptID := s.receipt(t)

	got, err := s.svc.ReceiptsGet(ctx, application.ReceiptsIDParams{ID: receiptID})
	require.NoError(t, err)

	raw, err := json.Marshal(got.Receipt)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))

	for _, field := range []string{
		"_id", "create_time", "pay_time", "cancel_time", "state", "type",
		"external", "operation", "category", "error", "description", "detail",
		"amount", "currency", "commission", "account", "card", "merchant",
		"meta", "processing_id",
	} {
		_, present := decoded[field]
		assert.True(t, present, "%s is missing from the receipt", field)
	}

	// The values the documented responses fix for a receipt nothing unusual
	// happened to.
	assert.Equal(t, false, decoded["external"])
	assert.Equal(t, float64(-1), decoded["operation"])
	assert.Nil(t, decoded["category"])
	assert.Nil(t, decoded["error"])
	assert.Nil(t, decoded["processing_id"])
	assert.Equal(t, float64(domain.TypeCardPayment), decoded["type"])
	assert.Equal(t, float64(domain.CurrencyUZS), decoded["currency"])
}

// An unpaid receipt reports no card, and a paid one reports the masked number
// and expiry — never the token, which belongs to the card methods.
func TestReceiptCardAppearsOnlyOncePaid(t *testing.T) {
	ctx := context.Background()
	s := newStand(t)

	created, err := s.svc.ReceiptsCreate(ctx, application.ReceiptsCreateParams{
		Amount: amount, Account: s.account(),
	})
	require.NoError(t, err)
	assert.Nil(t, created.Receipt.Card, "a receipt awaiting payment has no card")

	token := s.verifiedCard(t, docCardNumber)
	paid, err := s.svc.ReceiptsPay(ctx, application.ReceiptsPayParams{
		ID: created.Receipt.ID, Token: token,
	})

	require.NoError(t, err)
	require.NotNil(t, paid.Receipt.Card)
	assert.Equal(t, docCardMask, paid.Receipt.Card.Number)
	assert.Equal(t, "0399", paid.Receipt.Card.Expire)
}

// A receipt whose card has since been removed still reads: the card is
// reported as absent rather than failing a call that only looks at it.
func TestReceiptCardIsAbsentWhenTheCardCannotBeRead(t *testing.T) {
	ctx := context.Background()
	s := newStand(t)
	id, _ := s.payout(t)
	s.cards.failByID = true

	got, err := s.svc.ReceiptsGet(ctx, application.ReceiptsIDParams{ID: id})

	require.NoError(t, err)
	assert.Nil(t, got.Receipt.Card)
}
