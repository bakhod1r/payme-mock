package application_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	billing "github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
	"github.com/bakhod1r/payme-mock/internal/context/payment/merchant/application"
	"github.com/bakhod1r/payme-mock/internal/context/payment/merchant/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

const (
	timeoutMillis = int64(43_200_000) // 12 hours
	paymeID       = "5305e3bab097f420a62ced0b"
	orderAmount   = int64(500000)
	payerPhone    = "901234567"
)

var startTime = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

// stand assembles a service with in-memory ports and one payable order.
type stand struct {
	svc     *application.Service
	txs     *fakeTransactions
	events  *fakeEvents
	accts   *fakeAccounts
	orders  *fakeOrders
	walkIns *fakeWalkIns
	clk     *clock.Fake
}

func newStand(t *testing.T) *stand {
	t.Helper()

	txs := newFakeTransactions()
	events := &fakeEvents{}
	accts := newFakeAccounts()
	orders := newFakeOrders()
	walkIns := newFakeWalkIns()
	clk := clock.NewFake(startTime)

	acc := &billing.Account{ID: 42, SandboxID: 1, Phone: payerPhone}
	accts.add("phone", payerPhone, acc)

	order := &billing.Order{ID: 197, SandboxID: 1, AccountID: acc.ID, Amount: orderAmount, Status: billing.StatusNew}
	orders.byID[order.ID] = order
	orders.byAccount[acc.ID] = []*billing.Order{order}

	svc := application.NewService(txs, events, accts, orders, walkIns, clk, application.Settings{
		TransactionTimeoutMillis: timeoutMillis,
		AccountField:             "phone",
	})

	return &stand{svc: svc, txs: txs, events: events, accts: accts, orders: orders, walkIns: walkIns, clk: clk}
}

func (s *stand) account() map[string]string { return map[string]string{"phone": payerPhone} }

// stopRegister blocks the register's own payer, which is what the console's
// Block button does to a cash register.
func (s *stand) stopRegister() {
	for _, acc := range s.accts.byID {
		acc.Blocked = true
	}
}

// registerBalance is the figure the register holds, which a payment through it
// moves and a stopped register must not.
func (s *stand) registerBalance() int64 {
	for _, acc := range s.accts.byID {
		return acc.Balance
	}

	return 0
}

func (s *stand) createTx(t *testing.T) *application.CreateResult {
	t.Helper()

	got, err := s.svc.CreateTransaction(context.Background(), application.CreateParams{
		ID: paymeID, Time: s.clk.NowMillis(), Amount: orderAmount, Account: s.account(),
	})
	require.NoError(t, err)
	return got
}

// A register an operator stopped refuses every payment through it, at each of
// the three points one could get to: the check that says whether it may happen,
// the call that creates it, and the move of the money itself.
//
// The flag had been stored, shown on the register's page and consulted nowhere,
// so a stopped register answered 200 to everything — which is the one thing a
// stand exists to let anyone rehearse.
func TestAStoppedRegisterRefusesPayments(t *testing.T) {
	ctx := context.Background()

	t.Run("CheckPerformTransaction", func(t *testing.T) {
		s := newStand(t)
		s.stopRegister()

		_, err := s.svc.CheckPerformTransaction(ctx, application.CheckPerformParams{
			Amount: orderAmount, Account: s.account(),
		})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	t.Run("CreateTransaction", func(t *testing.T) {
		s := newStand(t)
		s.stopRegister()

		_, err := s.svc.CreateTransaction(ctx, application.CreateParams{
			ID: paymeID, Time: s.clk.NowMillis(), Amount: orderAmount, Account: s.account(),
		})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
		assert.Empty(t, s.txs.byPaymeID, "nothing is created that could never be performed")
	})

	// Blocking between the create and the perform is the case the other two
	// cannot cover: the payment exists already, and the money must still not
	// move.
	t.Run("PerformTransaction", func(t *testing.T) {
		s := newStand(t)
		created := s.createTx(t)
		s.stopRegister()

		_, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})

		require.Error(t, err)
		assert.Equal(t, int64(0), s.registerBalance(), "a stopped register moves no money")
		_ = created
	})
}

// ---------- CheckPerformTransaction ----------

func TestCheckPerformTransaction(t *testing.T) {
	ctx := context.Background()

	t.Run("allows a payable order", func(t *testing.T) {
		s := newStand(t)

		got, err := s.svc.CheckPerformTransaction(ctx, application.CheckPerformParams{
			Amount: orderAmount, Account: s.account(),
		})

		require.NoError(t, err)
		assert.True(t, got.Allow)
	})

	t.Run("rejects a wrong amount", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.CheckPerformTransaction(ctx, application.CheckPerformParams{
			Amount: 1, Account: s.account(),
		})

		assert.ErrorIs(t, err, payerr.ErrInvalidAmount)
	})

	t.Run("rejects an unknown account and names the field", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.CheckPerformTransaction(ctx, application.CheckPerformParams{
			Amount: orderAmount, Account: map[string]string{"phone": "000000000"},
		})

		pe, ok := payerr.As(err)
		require.True(t, ok)
		assert.True(t, pe.Code.IsAccountCode())
		assert.Equal(t, "phone", pe.Data)
	})

	t.Run("rejects a missing account field", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.CheckPerformTransaction(ctx, application.CheckPerformParams{
			Amount: orderAmount, Account: map[string]string{"login": "x"},
		})

		pe, ok := payerr.As(err)
		require.True(t, ok)
		assert.Equal(t, "phone", pe.Data)
	})

	t.Run("rejects an empty account field", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.CheckPerformTransaction(ctx, application.CheckPerformParams{
			Amount: orderAmount, Account: map[string]string{"phone": ""},
		})

		pe, ok := payerr.As(err)
		require.True(t, ok)
		assert.Equal(t, "phone", pe.Data)
	})

	t.Run("rejects an order that is already paid", func(t *testing.T) {
		s := newStand(t)
		s.orders.byID[197].MarkPaid()

		_, err := s.svc.CheckPerformTransaction(ctx, application.CheckPerformParams{
			Amount: orderAmount, Account: s.account(),
		})

		// No payable order remains, so the account lookup itself fails.
		pe, ok := payerr.As(err)
		require.True(t, ok)
		assert.True(t, pe.Code.IsAccountCode())
	})

	t.Run("propagates an account store failure", func(t *testing.T) {
		s := newStand(t)
		s.accts.fail = true

		_, err := s.svc.CheckPerformTransaction(ctx, application.CheckPerformParams{
			Amount: orderAmount, Account: s.account(),
		})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("propagates an order store failure", func(t *testing.T) {
		s := newStand(t)
		s.orders.failByAcct = true

		_, err := s.svc.CheckPerformTransaction(ctx, application.CheckPerformParams{
			Amount: orderAmount, Account: s.account(),
		})

		assert.ErrorIs(t, err, errBoom)
	})
}

// An order held by an active transaction is still payable by that transaction,
// but CheckPerform must refuse once the order leaves a payable status.
func TestCheckPerformRefusesUnpayableOrderFoundDirectly(t *testing.T) {
	s := newStand(t)
	// A cancelled order is still returned by the account lookup path when it is
	// the only order, but it must not be payable.
	order := s.orders.byID[197]
	order.MarkProcessing()

	got, err := s.svc.CheckPerformTransaction(context.Background(), application.CheckPerformParams{
		Amount: orderAmount, Account: s.account(),
	})

	require.NoError(t, err)
	assert.True(t, got.Allow, "an order being processed can still be paid")
}

// ---------- CreateTransaction ----------

func TestCreateTransaction(t *testing.T) {
	ctx := context.Background()

	t.Run("creates a transaction in the created state", func(t *testing.T) {
		s := newStand(t)

		got := s.createTx(t)

		assert.Equal(t, domain.StateCreated, got.State)
		assert.Equal(t, clock.ToMillis(startTime), got.CreateTime)
		assert.Equal(t, "5123", got.Transaction)
	})

	t.Run("marks the order as processing", func(t *testing.T) {
		s := newStand(t)

		s.createTx(t)

		assert.Equal(t, billing.StatusProcessing, s.orders.byID[197].Status)
	})

	t.Run("records the state change", func(t *testing.T) {
		s := newStand(t)

		s.createTx(t)

		e := s.events.last()
		assert.Equal(t, "CreateTransaction", e.Method)
		assert.False(t, e.IdempotentHit)
		require.NotNil(t, e.ToState)
		assert.Equal(t, domain.StateCreated, *e.ToState)
	})

	t.Run("rejects a wrong amount", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.CreateTransaction(ctx, application.CreateParams{
			ID: paymeID, Time: 1, Amount: 999, Account: s.account(),
		})

		assert.ErrorIs(t, err, payerr.ErrInvalidAmount)
	})

	t.Run("rejects an unknown account", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.CreateTransaction(ctx, application.CreateParams{
			ID: paymeID, Time: 1, Amount: orderAmount, Account: map[string]string{"phone": "nope"},
		})

		pe, ok := payerr.As(err)
		require.True(t, ok)
		assert.True(t, pe.Code.IsAccountCode())
	})

	t.Run("refuses a second active transaction on the same order", func(t *testing.T) {
		s := newStand(t)
		s.createTx(t)

		_, err := s.svc.CreateTransaction(ctx, application.CreateParams{
			ID: "another-payme-id", Time: 1, Amount: orderAmount, Account: s.account(),
		})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	t.Run("propagates a lookup failure", func(t *testing.T) {
		s := newStand(t)
		s.txs.failByPaymeID = true

		_, err := s.svc.CreateTransaction(ctx, application.CreateParams{
			ID: paymeID, Time: 1, Amount: orderAmount, Account: s.account(),
		})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("propagates an active-transaction lookup failure", func(t *testing.T) {
		s := newStand(t)
		s.txs.failActiveByOrder = true

		_, err := s.svc.CreateTransaction(ctx, application.CreateParams{
			ID: paymeID, Time: 1, Amount: orderAmount, Account: s.account(),
		})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("propagates a create failure", func(t *testing.T) {
		s := newStand(t)
		s.txs.failCreate = true

		_, err := s.svc.CreateTransaction(ctx, application.CreateParams{
			ID: paymeID, Time: 1, Amount: orderAmount, Account: s.account(),
		})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("propagates an order update failure", func(t *testing.T) {
		s := newStand(t)
		s.orders.failUpdate = true

		_, err := s.svc.CreateTransaction(ctx, application.CreateParams{
			ID: paymeID, Time: 1, Amount: orderAmount, Account: s.account(),
		})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("refuses to recreate a transaction that has moved on", func(t *testing.T) {
		s := newStand(t)
		s.createTx(t)
		_, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})
		require.NoError(t, err)

		_, err = s.svc.CreateTransaction(ctx, application.CreateParams{
			ID: paymeID, Time: 1, Amount: orderAmount, Account: s.account(),
		})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
		assert.True(t, s.events.last().IdempotentHit)
	})
}

// The sandbox sends every write twice and requires identical responses.
func TestCreateTransactionIsIdempotent(t *testing.T) {
	s := newStand(t)
	ctx := context.Background()
	params := application.CreateParams{
		ID: paymeID, Time: 1, Amount: orderAmount, Account: s.account(),
	}

	first, err := s.svc.CreateTransaction(ctx, params)
	require.NoError(t, err)

	s.clk.Advance(9 * time.Second) // time moves between the two calls

	second, err := s.svc.CreateTransaction(ctx, params)
	require.NoError(t, err)

	assert.Equal(t, first, second, "a repeated request must return the original response")
	assert.True(t, s.events.last().IdempotentHit)
}

// Two concurrent deliveries of the same request both reach Create; the unique
// index lets one through and the loser must replay the winner's response
// rather than report a failure.
func TestCreateTransactionLosingARaceReplaysTheWinnersResponse(t *testing.T) {
	s := newStand(t)
	winner := &domain.Transaction{
		ID: 5123, SandboxID: 1, PaymeID: paymeID, OrderID: 197, AccountID: 42,
		Amount: orderAmount, State: domain.StateCreated,
		CreateTime: clock.ToMillis(startTime),
	}
	s.txs.duplicateOnCreate = winner

	got, err := s.svc.CreateTransaction(context.Background(), application.CreateParams{
		ID: paymeID, Time: 1, Amount: orderAmount, Account: s.account(),
	})

	require.NoError(t, err)
	assert.Equal(t, "5123", got.Transaction, "the loser reports the winner's transaction")
	assert.Equal(t, domain.StateCreated, got.State)
	assert.True(t, s.events.last().IdempotentHit)
}

func TestCreateTransactionLosingARaceToAMovedOnTransaction(t *testing.T) {
	s := newStand(t)
	s.txs.duplicateOnCreate = &domain.Transaction{
		ID: 5123, PaymeID: paymeID, State: domain.StatePerformed,
	}

	_, err := s.svc.CreateTransaction(context.Background(), application.CreateParams{
		ID: paymeID, Time: 1, Amount: orderAmount, Account: s.account(),
	})

	assert.ErrorIs(t, err, payerr.ErrCannotPerform)
}

func TestCreateTransactionRaceReplayPropagatesALookupFailure(t *testing.T) {
	s := newStand(t)
	s.txs.duplicateOnCreate = &domain.Transaction{ID: 1, PaymeID: paymeID, State: domain.StateCreated}

	// The winning row vanishes before the loser can read it back.
	s.txs.byPaymeID = map[string]*domain.Transaction{}
	s.txs.failByPaymeIDAfterCreate = true

	_, err := s.svc.CreateTransaction(context.Background(), application.CreateParams{
		ID: paymeID, Time: 1, Amount: orderAmount, Account: s.account(),
	})

	assert.ErrorIs(t, err, errBoom)
}

// ---------- PerformTransaction ----------

func TestPerformTransaction(t *testing.T) {
	ctx := context.Background()

	t.Run("performs a created transaction", func(t *testing.T) {
		s := newStand(t)
		s.createTx(t)
		s.clk.Advance(963 * time.Millisecond)

		got, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})

		require.NoError(t, err)
		assert.Equal(t, domain.StatePerformed, got.State)
		assert.Equal(t, s.clk.NowMillis(), got.PerformTime)
		assert.Equal(t, "5123", got.Transaction)
	})

	t.Run("marks the order paid", func(t *testing.T) {
		s := newStand(t)
		s.createTx(t)

		_, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})

		require.NoError(t, err)
		assert.Equal(t, billing.StatusPaid, s.orders.byID[197].Status)
	})

	t.Run("reports an unknown transaction", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: "nope"})

		assert.ErrorIs(t, err, payerr.ErrTransactionNotFound)
	})

	t.Run("refuses a cancelled transaction", func(t *testing.T) {
		s := newStand(t)
		s.createTx(t)
		_, err := s.svc.CancelTransaction(ctx, application.CancelParams{ID: paymeID, Reason: 1})
		require.NoError(t, err)

		_, err = s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	t.Run("propagates a lookup failure", func(t *testing.T) {
		s := newStand(t)
		s.txs.failByPaymeID = true

		_, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("propagates an update failure", func(t *testing.T) {
		s := newStand(t)
		s.createTx(t)
		s.txs.failUpdate = true

		_, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("propagates an order update failure", func(t *testing.T) {
		s := newStand(t)
		s.createTx(t)
		s.orders.failUpdate = true

		_, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("tolerates a transaction whose order has vanished", func(t *testing.T) {
		s := newStand(t)
		s.createTx(t)
		s.orders.missingByID = true

		_, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})

		assert.NoError(t, err)
	})

	t.Run("propagates an order load failure", func(t *testing.T) {
		s := newStand(t)
		s.createTx(t)
		s.orders.failByID = true

		_, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})

		assert.ErrorIs(t, err, errBoom)
	})
}

func TestPerformTransactionIsIdempotent(t *testing.T) {
	s := newStand(t)
	ctx := context.Background()
	s.createTx(t)

	first, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})
	require.NoError(t, err)

	s.clk.Advance(time.Minute)

	second, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.True(t, s.events.last().IdempotentHit)
}

// Past the confirmation window the call is refused and the transaction is
// auto-cancelled, so a later CheckTransaction shows the timeout reason.
func TestPerformTransactionAfterTimeout(t *testing.T) {
	s := newStand(t)
	ctx := context.Background()
	s.createTx(t)

	s.clk.Advance(12*time.Hour + time.Millisecond)

	_, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})
	require.ErrorIs(t, err, payerr.ErrCannotPerform)

	got, err := s.svc.CheckTransaction(ctx, application.CheckParams{ID: paymeID})
	require.NoError(t, err)
	assert.Equal(t, domain.StateCancelled, got.State)
	require.NotNil(t, got.Reason)
	assert.Equal(t, domain.ReasonTimeout, *got.Reason)
}

func TestPerformTransactionAfterTimeoutPropagatesUpdateFailure(t *testing.T) {
	s := newStand(t)
	s.createTx(t)
	s.clk.Advance(12*time.Hour + time.Millisecond)
	s.txs.failUpdate = true

	_, err := s.svc.PerformTransaction(context.Background(), application.PerformParams{ID: paymeID})

	assert.ErrorIs(t, err, errBoom)
}

// ---------- CancelTransaction ----------

func TestCancelTransaction(t *testing.T) {
	ctx := context.Background()

	t.Run("cancelling a created transaction yields -1", func(t *testing.T) {
		s := newStand(t)
		s.createTx(t)

		got, err := s.svc.CancelTransaction(ctx, application.CancelParams{ID: paymeID, Reason: 1})

		require.NoError(t, err)
		assert.Equal(t, domain.StateCancelled, got.State)
		assert.Equal(t, s.clk.NowMillis(), got.CancelTime)
	})

	t.Run("cancelling a performed transaction yields -2", func(t *testing.T) {
		s := newStand(t)
		s.createTx(t)
		_, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})
		require.NoError(t, err)

		got, err := s.svc.CancelTransaction(ctx, application.CancelParams{ID: paymeID, Reason: 5})

		require.NoError(t, err)
		assert.Equal(t, domain.StateCancelledAfterDo, got.State)
	})

	t.Run("releases the order", func(t *testing.T) {
		s := newStand(t)
		s.createTx(t)

		_, err := s.svc.CancelTransaction(ctx, application.CancelParams{ID: paymeID, Reason: 1})

		require.NoError(t, err)
		assert.Equal(t, billing.StatusCancelled, s.orders.byID[197].Status)
	})

	t.Run("reports an unknown transaction", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.CancelTransaction(ctx, application.CancelParams{ID: "nope", Reason: 1})

		assert.ErrorIs(t, err, payerr.ErrTransactionNotFound)
	})

	t.Run("propagates a lookup failure", func(t *testing.T) {
		s := newStand(t)
		s.txs.failByPaymeID = true

		_, err := s.svc.CancelTransaction(ctx, application.CancelParams{ID: paymeID, Reason: 1})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("propagates an update failure", func(t *testing.T) {
		s := newStand(t)
		s.createTx(t)
		s.txs.failUpdate = true

		_, err := s.svc.CancelTransaction(ctx, application.CancelParams{ID: paymeID, Reason: 1})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("propagates an order update failure", func(t *testing.T) {
		s := newStand(t)
		s.createTx(t)
		s.orders.failUpdate = true

		_, err := s.svc.CancelTransaction(ctx, application.CancelParams{ID: paymeID, Reason: 1})

		assert.ErrorIs(t, err, errBoom)
	})

	t.Run("refuses an unknown state", func(t *testing.T) {
		s := newStand(t)
		s.createTx(t)
		s.txs.byPaymeID[paymeID].State = domain.State(99)

		_, err := s.svc.CancelTransaction(ctx, application.CancelParams{ID: paymeID, Reason: 1})

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})
}

func TestCancelTransactionIsIdempotent(t *testing.T) {
	s := newStand(t)
	ctx := context.Background()
	s.createTx(t)

	first, err := s.svc.CancelTransaction(ctx, application.CancelParams{ID: paymeID, Reason: 1})
	require.NoError(t, err)

	s.clk.Advance(time.Hour)

	second, err := s.svc.CancelTransaction(ctx, application.CancelParams{ID: paymeID, Reason: 3})
	require.NoError(t, err)

	assert.Equal(t, first, second, "a repeated cancel must not rewrite the reason or time")
	assert.True(t, s.events.last().IdempotentHit)
}

// ---------- CheckTransaction ----------

func TestCheckTransaction(t *testing.T) {
	ctx := context.Background()

	t.Run("reports a performed transaction", func(t *testing.T) {
		s := newStand(t)
		s.createTx(t)
		_, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})
		require.NoError(t, err)

		got, err := s.svc.CheckTransaction(ctx, application.CheckParams{ID: paymeID})

		require.NoError(t, err)
		assert.Equal(t, domain.StatePerformed, got.State)
		assert.NotZero(t, got.PerformTime)
		assert.Zero(t, got.CancelTime)
		assert.Nil(t, got.Reason, "a transaction that was never cancelled reports a null reason")
	})

	t.Run("reports an unknown transaction", func(t *testing.T) {
		s := newStand(t)

		_, err := s.svc.CheckTransaction(ctx, application.CheckParams{ID: "nope"})

		assert.ErrorIs(t, err, payerr.ErrTransactionNotFound)
	})

	t.Run("propagates a lookup failure", func(t *testing.T) {
		s := newStand(t)
		s.txs.failByPaymeID = true

		_, err := s.svc.CheckTransaction(ctx, application.CheckParams{ID: paymeID})

		assert.ErrorIs(t, err, errBoom)
	})
}

// ---------- GetStatement ----------

func TestGetStatement(t *testing.T) {
	ctx := context.Background()

	t.Run("returns an empty array for a quiet period", func(t *testing.T) {
		s := newStand(t)

		got, err := s.svc.GetStatement(ctx, application.StatementParams{From: 1, To: 2})

		require.NoError(t, err)
		assert.NotNil(t, got.Transactions, "an empty statement must be [] rather than null")
		assert.Empty(t, got.Transactions)
	})

	t.Run("includes both period bounds", func(t *testing.T) {
		s := newStand(t)
		reason := domain.Reason(1)
		s.txs.statement = []*domain.Transaction{
			{PaymeID: "a", CreateTime: 100, Amount: 1, State: domain.StateCreated},
			{PaymeID: "b", CreateTime: 200, Amount: 2, State: domain.StatePerformed},
			{PaymeID: "c", CreateTime: 300, Amount: 3, State: domain.StateCancelled, Reason: &reason},
			{PaymeID: "d", CreateTime: 400, Amount: 4, State: domain.StateCreated},
		}

		got, err := s.svc.GetStatement(ctx, application.StatementParams{From: 100, To: 300})

		require.NoError(t, err)
		require.Len(t, got.Transactions, 3)
		assert.Equal(t, "a", got.Transactions[0].ID, "the from bound is inclusive")
		assert.Equal(t, "c", got.Transactions[2].ID, "the to bound is inclusive")
		assert.Equal(t, domain.Reason(1), *got.Transactions[2].Reason)
	})

	t.Run("propagates a store failure", func(t *testing.T) {
		s := newStand(t)
		s.txs.failStatement = true

		_, err := s.svc.GetStatement(ctx, application.StatementParams{From: 1, To: 2})

		assert.ErrorIs(t, err, errBoom)
	})
}

// Losing an audit line must never change what the payment system reports.
func TestAuditFailureDoesNotFailTheRequest(t *testing.T) {
	s := newStand(t)
	s.events.fail = true

	got, err := s.svc.CreateTransaction(context.Background(), application.CreateParams{
		ID: paymeID, Time: 1, Amount: orderAmount, Account: s.account(),
	})

	require.NoError(t, err)
	assert.Equal(t, domain.StateCreated, got.State)
}

// ---------- the register's balance ----------

// newRegister assembles a stand whose register moves money in the given
// direction, starting from the given balance.
func newRegister(t *testing.T, kind billing.Kind, balance int64) *stand {
	t.Helper()

	s := newStand(t)
	s.accts.byID[42].Balance = balance
	s.svc = application.NewService(s.txs, s.events, s.accts, s.orders, s.walkIns, s.clk, application.Settings{
		TransactionTimeoutMillis: timeoutMillis,
		AccountField:             "phone",
		RegisterKind:             string(kind),
	})

	return s
}

// A stand that never said what kind of register it is takes money in, which is
// what an integration starts from.
func TestPerformAddsToTheBalanceByDefault(t *testing.T) {
	s := newStand(t)
	s.accts.byID[42].Balance = 1000
	s.createTx(t)

	_, err := s.svc.PerformTransaction(context.Background(), application.PerformParams{ID: paymeID})

	require.NoError(t, err)
	assert.Equal(t, int64(1000)+orderAmount, s.accts.balances[42])
}

func TestPerformMovesTheBalanceTheRegistersWay(t *testing.T) {
	tests := []struct {
		kind billing.Kind
		want int64
	}{
		{billing.KindTopup, 1_000_000 + orderAmount},
		{billing.KindDividend, 1_000_000 - orderAmount},
		{billing.KindDeposit, 1_000_000 - orderAmount},
	}

	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			s := newRegister(t, tt.kind, 1_000_000)
			s.createTx(t)

			_, err := s.svc.PerformTransaction(context.Background(), application.PerformParams{ID: paymeID})

			require.NoError(t, err)
			assert.Equal(t, tt.want, s.accts.balances[42])
		})
	}
}

// A payout the register cannot cover fails the whole call: the transaction
// stays created rather than being performed against money that is not there.
func TestPerformRefusesAPayoutTheRegisterCannotCover(t *testing.T) {
	s := newRegister(t, billing.KindDividend, orderAmount-1)
	s.createTx(t)

	_, err := s.svc.PerformTransaction(context.Background(), application.PerformParams{ID: paymeID})

	require.ErrorIs(t, err, billing.ErrInsufficientFunds)
	assert.Empty(t, s.accts.balances, "a refused payout stores no balance")
	assert.Equal(t, billing.StatusProcessing, s.orders.byID[197].Status,
		"the order is left where CreateTransaction put it, not marked paid")

	// The refusal is recorded as an operation that could not be carried out,
	// which is the code the payer's side sees.
	recorded := s.events.last()
	require.NotNil(t, recorded.ErrorCode)
	assert.Equal(t, int(payerr.CodeCannotPerform), *recorded.ErrorCode)
	assert.Nil(t, recorded.ToState, "nothing was stored, so no state was reached")

}

// A repeated PerformTransaction must not move the balance twice.
func TestPerformDoesNotMoveTheBalanceOnAReplay(t *testing.T) {
	s := newRegister(t, billing.KindTopup, 0)
	ctx := context.Background()
	s.createTx(t)

	_, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})
	require.NoError(t, err)
	s.accts.balances[42] = -1 // any second move would overwrite this

	_, err = s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})

	require.NoError(t, err)
	assert.Equal(t, int64(-1), s.accts.balances[42])
}

func TestPerformReportsALostAccountLookup(t *testing.T) {
	s := newStand(t)
	s.createTx(t)
	s.accts.failByID = true

	_, err := s.svc.PerformTransaction(context.Background(), application.PerformParams{ID: paymeID})

	assert.ErrorIs(t, err, errBoom)
}

func TestPerformReportsAFailedBalanceWrite(t *testing.T) {
	s := newStand(t)
	s.createTx(t)
	s.accts.failUpdate = true

	_, err := s.svc.PerformTransaction(context.Background(), application.PerformParams{ID: paymeID})

	assert.ErrorIs(t, err, errBoom)
}

// Cancelling a performed payment puts back exactly what performing it moved.
func TestCancelAfterPerformReversesTheBalance(t *testing.T) {
	for _, kind := range billing.Kinds {
		t.Run(string(kind), func(t *testing.T) {
			s := newRegister(t, kind, 1_000_000)
			ctx := context.Background()
			s.createTx(t)

			_, err := s.svc.PerformTransaction(ctx, application.PerformParams{ID: paymeID})
			require.NoError(t, err)

			_, err = s.svc.CancelTransaction(ctx, application.CancelParams{ID: paymeID, Reason: 5})

			require.NoError(t, err)
			assert.Equal(t, int64(1_000_000), s.accts.balances[42])
		})
	}
}

// Only a payment that was performed moved the balance, so cancelling one that
// never was has nothing to put back.
func TestCancelBeforePerformLeavesTheBalanceAlone(t *testing.T) {
	s := newRegister(t, billing.KindTopup, 1_000_000)
	s.createTx(t)

	_, err := s.svc.CancelTransaction(context.Background(), application.CancelParams{ID: paymeID, Reason: 1})

	require.NoError(t, err)
	assert.Empty(t, s.accts.balances)
}

func TestCancelReportsAFailedReversal(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(s *stand)
	}{
		{"the payer cannot be loaded", func(s *stand) { s.accts.failByID = true }},
		{"the balance cannot be stored", func(s *stand) { s.accts.failUpdate = true }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newRegister(t, billing.KindTopup, 1_000_000)
			ctx := context.Background()
			s.createTx(t)
			require.NoError(t, mustPerform(t, s))

			tt.break_(s)

			_, err := s.svc.CancelTransaction(ctx, application.CancelParams{ID: paymeID, Reason: 5})

			assert.ErrorIs(t, err, errBoom)
		})
	}
}

// mustPerform performs the stand's transaction, reporting the error so a test
// can require it away.
func mustPerform(t *testing.T, s *stand) error {
	t.Helper()

	_, err := s.svc.PerformTransaction(context.Background(), application.PerformParams{ID: paymeID})
	return err
}

// ---------- payers registered on arrival ----------

// newWalkInStand assembles a stand that accepts payers it has never seen.
func newWalkInStand(t *testing.T) *stand {
	t.Helper()

	s := newStand(t)
	s.svc = application.NewService(s.txs, s.events, s.accts, s.orders, s.walkIns, s.clk,
		application.Settings{
			TransactionTimeoutMillis: timeoutMillis,
			AccountField:             "phone",
			AutoRegisterAccounts:     true,
		})

	return s
}

// unknownAccount is a payer nothing in the stand has heard of, which is what a
// register that generates its account per payment sends.
func unknownAccount() map[string]string { return map[string]string{"phone": "901112233"} }

// Off, an unknown payer is refused: that is the documented -31050 and the
// default every other profile keeps.
func TestCheckPerformRefusesAnUnknownPayerByDefault(t *testing.T) {
	s := newStand(t)

	_, err := s.svc.CheckPerformTransaction(context.Background(), application.CheckPerformParams{
		Amount: orderAmount, Account: unknownAccount(),
	})

	assert.ErrorIs(t, err, payerr.ErrAccountNotFound)
	assert.Zero(t, s.walkIns.calls, "the walk-in port must not be reached")
}

func TestCheckPerformRegistersAnUnknownPayer(t *testing.T) {
	s := newWalkInStand(t)

	got, err := s.svc.CheckPerformTransaction(context.Background(), application.CheckPerformParams{
		Amount: orderAmount, Account: unknownAccount(),
	})

	require.NoError(t, err)
	assert.True(t, got.Allow)
}

// The check and the payment that follows it must settle the same order, or the
// stand would approve one and charge another.
func TestCreateReusesTheOrderTheCheckRegistered(t *testing.T) {
	s := newWalkInStand(t)
	account := unknownAccount()

	_, err := s.svc.CheckPerformTransaction(context.Background(), application.CheckPerformParams{
		Amount: orderAmount, Account: account,
	})
	require.NoError(t, err)

	got, err := s.svc.CreateTransaction(context.Background(), application.CreateParams{
		ID: paymeID, Time: s.clk.NowMillis(), Amount: orderAmount, Account: account,
	})

	require.NoError(t, err)
	assert.Equal(t, domain.StateCreated, got.State)
	assert.Len(t, s.walkIns.orders, 1, "the second call registered another order")
}

// A payer whose orders are all settled is registered a new one, so a card can
// be charged twice.
func TestKnownPayerWithNothingLeftToPayIsRegisteredAgain(t *testing.T) {
	s := newWalkInStand(t)
	s.orders.byAccount[42][0].Status = billing.StatusPaid

	got, err := s.svc.CheckPerformTransaction(context.Background(), application.CheckPerformParams{
		Amount: orderAmount, Account: s.account(),
	})

	require.NoError(t, err)
	assert.True(t, got.Allow)
	assert.Equal(t, 1, s.walkIns.calls)
}

// An amount no order could carry is the payment's error, not a row the stand
// should try to write.
func TestRegisteringRefusesANonPositiveAmount(t *testing.T) {
	s := newWalkInStand(t)

	_, err := s.svc.CheckPerformTransaction(context.Background(), application.CheckPerformParams{
		Amount: 0, Account: unknownAccount(),
	})

	assert.ErrorIs(t, err, payerr.ErrInvalidAmount)
	assert.Zero(t, s.walkIns.calls)
}

// A storage failure while registering is not a missing payer: reporting it as
// one would tell the caller their account is wrong when nothing is.
func TestRegisteringSurfacesAStorageFailure(t *testing.T) {
	s := newWalkInStand(t)
	s.walkIns.fail = true

	_, err := s.svc.CheckPerformTransaction(context.Background(), application.CheckPerformParams{
		Amount: orderAmount, Account: unknownAccount(),
	})

	assert.ErrorIs(t, err, errBoom)
}

// A stand that cannot register the payer after all answers as it would have
// without the setting.
func TestRegisteringAMissingPayerIsReportedAsNotFound(t *testing.T) {
	s := newWalkInStand(t)
	s.walkIns.absent = true

	_, err := s.svc.CheckPerformTransaction(context.Background(), application.CheckPerformParams{
		Amount: orderAmount, Account: unknownAccount(),
	})

	assert.ErrorIs(t, err, payerr.ErrAccountNotFound)
}

// The account object must still name the payer: an empty field is the caller's
// mistake and registering something for it would hide that.
func TestRegisteringStillNeedsTheAccountField(t *testing.T) {
	s := newWalkInStand(t)

	_, err := s.svc.CheckPerformTransaction(context.Background(), application.CheckPerformParams{
		Amount: orderAmount, Account: map[string]string{"phone": ""},
	})

	assert.ErrorIs(t, err, payerr.ErrAccountNotFound)
	assert.Zero(t, s.walkIns.calls)
}

// orderStand is a stand configured the way the default profile is: payers are
// identified by the order they are paying, so the account value is an order id
// rather than a property of the payer.
func orderStand(t *testing.T) (*stand, *billing.Order, *billing.Order) {
	t.Helper()

	s := newStand(t)

	acc := &billing.Account{ID: 42, SandboxID: 1, Phone: payerPhone}
	older := &billing.Order{ID: 26, SandboxID: 1, AccountID: acc.ID, Amount: orderAmount, Status: billing.StatusNew}
	named := &billing.Order{ID: 27, SandboxID: 1, AccountID: acc.ID, Amount: orderAmount, Status: billing.StatusNew}

	for _, order := range []*billing.Order{older, named} {
		s.orders.byID[order.ID] = order
		s.accts.add("order_id", strconv.FormatInt(order.ID, 10), acc)
	}
	s.orders.byAccount[acc.ID] = []*billing.Order{older, named}

	s.svc = application.NewService(s.txs, s.events, s.accts, s.orders, s.walkIns, s.clk,
		application.Settings{TransactionTimeoutMillis: timeoutMillis, AccountField: "order_id"})

	return s, older, named
}

// A payment names an order; that order is the one that settles. Picking the
// payer's first payable order instead would close order 26 for a payment made
// against order 27, which is a debt paid off the wrong ledger line.
func TestCreateTransactionSettlesTheOrderNamed(t *testing.T) {
	s, older, named := orderStand(t)

	got, err := s.svc.CreateTransaction(context.Background(), application.CreateParams{
		ID: "txn-named", Time: s.clk.NowMillis(), Amount: orderAmount,
		Account: map[string]string{"order_id": strconv.FormatInt(named.ID, 10)},
	})
	require.NoError(t, err)
	require.NotNil(t, got)

	stored, err := s.txs.ByPaymeID(context.Background(), "txn-named")
	require.NoError(t, err)

	assert.Equal(t, named.ID, stored.OrderID)
	assert.NotEqual(t, older.ID, stored.OrderID, "the older payable order is not the one named")
}

// The same rule holds for the check that runs before the payment: approving
// one order and settling another is worse than refusing outright.
func TestCheckPerformTransactionChecksTheOrderNamed(t *testing.T) {
	s, older, named := orderStand(t)

	// The older order is closed, so falling back to "first payable" would pick
	// the named one by accident and hide the bug.
	older.Status = billing.StatusPaid
	named.Status = billing.StatusPaid

	_, err := s.svc.CheckPerformTransaction(context.Background(), application.CheckPerformParams{
		Amount:  orderAmount,
		Account: map[string]string{"order_id": strconv.FormatInt(named.ID, 10)},
	})

	assert.ErrorIs(t, err, payerr.ErrAccountNotFound, "an order already paid cannot be paid again")
}

// An order that is not the payer's is not reachable by naming it.
func TestCreateTransactionRefusesAnOrderOfAnotherPayer(t *testing.T) {
	s, _, named := orderStand(t)

	// The order moves to another payer between the lookups, which is the only
	// way the two can disagree.
	named.AccountID = 99

	_, err := s.svc.CreateTransaction(context.Background(), application.CreateParams{
		ID: "txn-crossed", Time: s.clk.NowMillis(), Amount: orderAmount,
		Account: map[string]string{"order_id": strconv.FormatInt(named.ID, 10)},
	})

	assert.ErrorIs(t, err, payerr.ErrAccountNotFound)
}

// An account value that names no order at all cannot be an order id, whatever
// payer it resolved to. Reading it as one would parse a payer's phone number
// into an order number and settle whatever that turned out to be.
func TestNamedOrderRefusesAValueThatIsNotAnOrderID(t *testing.T) {
	s, _, _ := orderStand(t)
	s.accts.add("order_id", "not-a-number", &billing.Account{ID: 42, SandboxID: 1})

	_, err := s.svc.CheckPerformTransaction(context.Background(), application.CheckPerformParams{
		Amount:  orderAmount,
		Account: map[string]string{"order_id": "not-a-number"},
	})

	assert.ErrorIs(t, err, payerr.ErrAccountNotFound)
}

// An order that is not there and an order that could not be read are different
// answers: the first is the payer's mistake, the second is the stand's, and
// reporting the stand's failure as "no such order" sends an integration looking
// in the wrong place.
func TestNamedOrderSeparatesAMissingOrderFromAFailedLookup(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		s, _, named := orderStand(t)
		s.orders.missingByID = true

		_, err := s.svc.CheckPerformTransaction(context.Background(), application.CheckPerformParams{
			Amount:  orderAmount,
			Account: map[string]string{"order_id": strconv.FormatInt(named.ID, 10)},
		})

		assert.ErrorIs(t, err, payerr.ErrAccountNotFound)
	})

	t.Run("unreadable", func(t *testing.T) {
		s, _, named := orderStand(t)
		s.orders.failByID = true

		_, err := s.svc.CheckPerformTransaction(context.Background(), application.CheckPerformParams{
			Amount:  orderAmount,
			Account: map[string]string{"order_id": strconv.FormatInt(named.ID, 10)},
		})

		assert.ErrorIs(t, err, errBoom)
	})
}

// A register that generates an order per payment sends the same value twice and
// means a second payment by it. A stand that accepts walk-in payers writes the
// second order rather than refusing the payment as a repeat.
func TestNamedOrderWritesAnotherOrderForAStandThatAcceptsWalkIns(t *testing.T) {
	s, _, named := orderStand(t)
	named.Status = billing.StatusPaid

	s.svc = application.NewService(s.txs, s.events, s.accts, s.orders, s.walkIns, s.clk,
		application.Settings{
			TransactionTimeoutMillis: timeoutMillis,
			AccountField:             "order_id",
			AutoRegisterAccounts:     true,
		})

	got, err := s.svc.CheckPerformTransaction(context.Background(), application.CheckPerformParams{
		Amount:  orderAmount,
		Account: map[string]string{"order_id": strconv.FormatInt(named.ID, 10)},
	})

	require.NoError(t, err)
	assert.True(t, got.Allow)
	assert.Equal(t, 1, s.walkIns.calls, "the payment wrote its own order")
}

// A register with no payer of its own has nothing to block, which is a stand
// that was never set up rather than a stand that was stopped. The payment paths
// report that where they need it; the block check must not turn it into a
// refusal of its own.
func TestARegisterWithNoPayerIsNotAStoppedRegister(t *testing.T) {
	s := newStand(t)
	s.accts.byID = map[int64]*billing.Account{}

	_, err := s.svc.CheckPerformTransaction(context.Background(), application.CheckPerformParams{
		Amount: orderAmount, Account: s.account(),
	})

	assert.NoError(t, err)
}

// A payer that could not be read is the stand's failure and is reported as one:
// answered as "not blocked", a stopped register would take the payment.
func TestAPayerThatCannotBeReadStopsThePayment(t *testing.T) {
	s := newStand(t)
	s.accts.failByID = true

	_, err := s.svc.CheckPerformTransaction(context.Background(), application.CheckPerformParams{
		Amount: orderAmount, Account: s.account(),
	})

	assert.ErrorIs(t, err, errBoom)
}
