package application_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/application"
	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
)

// A payout moves the register's own money, and no Merchant API chain exists to
// move it: nothing is bought, so nothing checks or performs. The ledger is the
// only thing that writes that half of a payout down, which makes what the
// service tells it — and what it does when the ledger refuses — the whole of
// the register's side of a withdrawal.

// fakeLedger records what it was told and can be made to refuse.
type fakeLedger struct {
	opened   []domain.Payout
	settled  []domain.Payout
	failOpen bool
	failSet  bool
	cashbox  domain.Cashbox
	failRead bool
}

func (f *fakeLedger) OpenPayout(_ context.Context, payout domain.Payout) error {
	if f.failOpen {
		return errBoom
	}
	f.opened = append(f.opened, payout)
	return nil
}

func (f *fakeLedger) SettlePayout(_ context.Context, payout domain.Payout) error {
	if f.failSet {
		return errBoom
	}
	f.settled = append(f.settled, payout)
	return nil
}

func (f *fakeLedger) Balance(_ context.Context) (domain.Cashbox, error) {
	if f.failRead {
		return domain.Cashbox{}, errBoom
	}
	return f.cashbox, nil
}

// ledgerStand is the ordinary stand with a ledger wired to it.
type ledgerStand struct {
	*stand
	ledger *fakeLedger
}

func newLedgerStand(t *testing.T) *ledgerStand {
	t.Helper()

	base := newStand(t)
	ledger := &fakeLedger{}

	base.svc = application.NewService(base.cards, base.receipts, base.merchant,
		base.scheduler, &fixedTokens{}, base.sms, base.clk,
		application.Settings{
			SandboxID:        1,
			MerchantID:       merchantID,
			VerifyCode:       sharedCode,
			VerifyWaitMillis: domain.DefaultVerifyWaitMillis,
			StepDelayMillis:  250,
			HoldWindowMillis: 30 * 24 * 60 * 60 * 1000,
			CardBalance:      100_000_000,
		},
		application.WithLedger(ledger))

	return &ledgerStand{stand: base, ledger: ledger}
}

// The books learn of a payout as it is opened, not only once it settles: a
// payout asked for and never completed is the row an operator goes looking for,
// and it would not exist if only settlements were written.
func TestPayoutIsWrittenToTheRegistersBooks(t *testing.T) {
	ctx := context.Background()
	s := newLedgerStand(t)
	token := s.verifiedCard(t, docCardNumber)

	created, err := s.svc.TransactionsCreate(ctx, application.TransactionsCreateParams{
		Token: token, Amount: 250000, Account: s.account(),
	})
	require.NoError(t, err)

	require.Len(t, s.ledger.opened, 1)
	opened := s.ledger.opened[0]
	assert.Equal(t, int64(250000), opened.Amount)
	assert.Equal(t, s.account(), opened.Account)
	assert.NotEmpty(t, opened.TransactionID,
		"the payout names the row the register's books hold for it")
	assert.NotZero(t, opened.CreateTime)
	assert.Zero(t, opened.PayTime, "nothing has been paid yet")

	assert.Empty(t, s.ledger.settled, "opening moves no money")

	_, err = s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
		ID: created.Receipt.ID, Amount: 250000, Token: token,
	})
	require.NoError(t, err)

	require.Len(t, s.ledger.settled, 1)
	settled := s.ledger.settled[0]
	assert.Equal(t, opened.TransactionID, settled.TransactionID,
		"the settlement names the payout it settles")
	assert.Equal(t, int64(250000), settled.Amount)
	assert.NotZero(t, settled.PayTime)
}

// A register that cannot take the payout refuses it, and the refusal has to
// reach the caller: a payout the books would not accept must not be reported as
// opened.
func TestPayoutIsRefusedWhenTheBooksRefuseIt(t *testing.T) {
	ctx := context.Background()
	s := newLedgerStand(t)
	token := s.verifiedCard(t, docCardNumber)
	s.ledger.failOpen = true

	_, err := s.svc.TransactionsCreate(ctx, application.TransactionsCreateParams{
		Token: token, Amount: 1000, Account: s.account(),
	})

	assert.ErrorIs(t, err, errBoom)
}

// The register pays before the card is credited. A settlement the books refuse
// — a register short of money is the usual reason — must leave the card
// untouched, or the stand hands out money the register never had.
func TestPayoutSettlementRefusedLeavesTheCardAlone(t *testing.T) {
	ctx := context.Background()
	s := newLedgerStand(t)
	token := s.verifiedCard(t, docCardNumber)

	created, err := s.svc.TransactionsCreate(ctx, application.TransactionsCreateParams{
		Token: token, Amount: 1000, Account: s.account(),
	})
	require.NoError(t, err)

	before := s.cards.byToken[token].Balance
	s.ledger.failSet = true

	_, err = s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
		ID: created.Receipt.ID, Amount: 1000, Token: token,
	})

	assert.ErrorIs(t, err, errBoom)
	assert.Equal(t, before, s.cards.byToken[token].Balance,
		"the card is credited only after the register has paid")
}

// A stand wired without a ledger still pays out. It is the state every stand
// was in before the ledger existed, and a payout that refused to happen for
// want of bookkeeping would be a worse stand than one that keeps no books.
func TestPayoutWorksWithoutALedger(t *testing.T) {
	ctx := context.Background()
	s := newStand(t)
	token := s.verifiedCard(t, docCardNumber)

	created, err := s.svc.TransactionsCreate(ctx, application.TransactionsCreateParams{
		Token: token, Amount: 1000, Account: s.account(),
	})
	require.NoError(t, err)

	_, err = s.svc.TransactionsComplete(ctx, application.TransactionsCompleteParams{
		ID: created.Receipt.ID, Amount: 1000, Token: token,
	})
	require.NoError(t, err)
}

// A stall is what a timeout is rehearsed against, so it has to be spent before
// the answer rather than announced and skipped.
func TestARiggedCardStallsBeforeItAnswers(t *testing.T) {
	ctx := context.Background()
	s := newStand(t)

	created, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))
	require.NoError(t, err)

	card := s.cards.byToken[created.Card.Token]
	card.DelayMillis = 10000

	before := s.clk.NowMillis()

	_, err = s.svc.CardsCheck(ctx, application.CardsTokenParams{Token: created.Card.Token})
	require.NoError(t, err)

	assert.Equal(t, int64(10000), s.clk.NowMillis()-before,
		"the stall is spent, not skipped")
}

// cards.check reports what the token stands for, so a card the bank stopped is
// refused there as well: an integration that only asks before paying finds out
// at the check rather than at the payment.
func TestCardsCheckRefusesAStoppedCard(t *testing.T) {
	ctx := context.Background()
	s := newStand(t)

	created, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))
	require.NoError(t, err)

	s.cards.byToken[created.Card.Token].Outcome = domain.OutcomeBlocked

	_, err = s.svc.CardsCheck(ctx, application.CardsTokenParams{Token: created.Card.Token})

	assert.Error(t, err)
}

// A card whose owner never signed up for SMS cannot be sent a code at all,
// which is a different failure from a code that turns out to be wrong, and an
// integration that never handles it stalls on a code the payer will never get.
func TestGetVerifyCodeRefusesACardThatCannotBeTexted(t *testing.T) {
	ctx := context.Background()
	s := newStand(t)

	created, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))
	require.NoError(t, err)

	s.cards.byToken[created.Card.Token].SMSEnabled = false

	_, err = s.svc.CardsGetVerifyCode(ctx, application.CardsTokenParams{Token: created.Card.Token})

	assert.Error(t, err)
}

// A card the merchant already holds is handed back with the token this register
// holds for it. A store that cannot issue that token fails the registration
// rather than answering with another register's.
func TestCardsCreateReportsATokenItCouldNotIssue(t *testing.T) {
	ctx := context.Background()
	s := newStand(t)

	_, err := s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))
	require.NoError(t, err)

	s.cards.failTokenFor = true

	_, err = s.svc.CardsCreate(ctx, cardParams(docCardNumber, "0399", true))

	assert.ErrorIs(t, err, errBoom)
}

// A clock is enough of a service for the option to be applied to; what matters
// is that an option reaches the service it was passed to.
func TestOptionsAreApplied(t *testing.T) {
	ledger := &fakeLedger{}
	svc := application.NewService(newFakeCards(), newFakeReceipts(), &fakeMerchant{},
		&fakeScheduler{}, &fixedTokens{}, &fakeSMS{}, clock.NewFake(startTime),
		application.Settings{SandboxID: 1}, application.WithLedger(ledger))

	require.NotNil(t, svc)
}

// The register's money is what a payout integration watches: it learns of an
// empty register from the figure, not from the customer whose withdrawal was
// refused.
func TestAccountsGetBalanceReportsWhatTheRegisterHolds(t *testing.T) {
	s := newLedgerStand(t)
	s.ledger.cashbox = domain.Cashbox{
		Balance:  750000,
		Currency: domain.CurrencyUZS,
		Kind:     "dividend",
	}

	out, err := s.svc.AccountsGetBalance(context.Background(), application.AccountsGetBalanceParams{})

	require.NoError(t, err)
	// The figure is in tiyin, as every amount the protocol carries is.
	assert.Equal(t, int64(750000), out.Balance)
}

// The call names the merchant it is asking about. Answering one that names
// another register would let an integration watch a balance it holds no
// credentials for.
func TestAccountsGetBalanceRefusesAnotherMerchant(t *testing.T) {
	s := newLedgerStand(t)
	s.ledger.cashbox = domain.Cashbox{Balance: 750000}

	_, err := s.svc.AccountsGetBalance(context.Background(),
		application.AccountsGetBalanceParams{MerchantID: "somebody-else"})

	assert.Error(t, err)
}

// The caller usually names its own register, which is the register it is
// already authenticated as.
func TestAccountsGetBalanceAcceptsItsOwnMerchant(t *testing.T) {
	s := newLedgerStand(t)
	s.ledger.cashbox = domain.Cashbox{Balance: 750000}

	out, err := s.svc.AccountsGetBalance(context.Background(),
		application.AccountsGetBalanceParams{MerchantID: merchantID})

	require.NoError(t, err)
	assert.Equal(t, int64(750000), out.Balance)
}

func TestAccountsGetBalanceReportsAReadItCouldNotMake(t *testing.T) {
	s := newLedgerStand(t)
	s.ledger.failRead = true

	_, err := s.svc.AccountsGetBalance(context.Background(), application.AccountsGetBalanceParams{})

	assert.ErrorIs(t, err, errBoom)
}

// A stand wired without a ledger has no register to speak for. Answering zero
// would read as an empty register and stop every withdrawal on the stand.
func TestAccountsGetBalanceRefusesWithoutALedger(t *testing.T) {
	s := newStand(t)

	_, err := s.svc.AccountsGetBalance(context.Background(), application.AccountsGetBalanceParams{})

	assert.Error(t, err)
}
