package infrastructure_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
	"github.com/bakhod1r/payme-mock/internal/context/payment/billing/infrastructure"
	merchant "github.com/bakhod1r/payme-mock/internal/context/payment/merchant/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres/testdb"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

const amount = int64(500000)

// seedPhone is the number testdb gives the payer it inserts.
const seedPhone = "901234567"

type stand struct {
	pool      *postgres.Pool
	accounts  *infrastructure.AccountRepository
	orders    *infrastructure.OrderRepository
	walkIns   *infrastructure.WalkInRepository
	ctx       context.Context
	sandboxID int64
	accountID int64
	orderID   int64
}

func newStand(t *testing.T) *stand {
	t.Helper()

	pool := testdb.New(t)
	sandboxID := testdb.Seed(t, pool, "qa")
	accountID, orderID := testdb.SeedAccountAndOrder(t, pool, sandboxID, amount)

	return &stand{
		pool:      pool,
		accounts:  infrastructure.NewAccountRepository(pool),
		orders:    infrastructure.NewOrderRepository(pool),
		walkIns:   infrastructure.NewWalkInRepository(pool),
		ctx:       sandboxctx.With(context.Background(), sandboxctx.Sandbox{ID: sandboxID}),
		sandboxID: sandboxID,
		accountID: accountID,
		orderID:   orderID,
	}
}

func TestE2EAccountByFieldFindsThePayer(t *testing.T) {
	s := newStand(t)

	acc, err := s.accounts.ByField(s.ctx, "phone", seedPhone)

	require.NoError(t, err)
	assert.Equal(t, s.accountID, acc.ID)
	assert.Equal(t, s.sandboxID, acc.SandboxID)
	assert.Equal(t, seedPhone, acc.Phone)
	assert.Equal(t, "Test payer", acc.Name)
	assert.False(t, acc.Blocked)
	// The column is NULL for this payer, which reaches the domain as an empty
	// string rather than a failed scan.
	assert.Empty(t, acc.Login)
}

// The default profile identifies a payer by the order they are paying, so the
// account object carries an order id rather than anything on the payer.
func TestE2EAccountByFieldResolvesAnOrderID(t *testing.T) {
	s := newStand(t)

	acc, err := s.accounts.ByField(s.ctx, "order_id", strconv.FormatInt(s.orderID, 10))

	require.NoError(t, err)
	assert.Equal(t, s.accountID, acc.ID)
}

func TestE2EAccountByFieldReportsAnUnknownOrderID(t *testing.T) {
	s := newStand(t)

	_, err := s.accounts.ByField(s.ctx, "order_id", strconv.FormatInt(s.orderID+1000, 10))

	assert.ErrorIs(t, err, merchant.ErrNotFound)
}

// A payer may type anything into the checkout form, so a value that is not a
// number is a miss rather than a failed cast.
func TestE2EAccountByFieldSurvivesANonNumericOrderID(t *testing.T) {
	s := newStand(t)

	_, err := s.accounts.ByField(s.ctx, "order_id", "not-a-number")

	assert.ErrorIs(t, err, merchant.ErrNotFound)
}

func TestE2EAccountByFieldReportsAnUnknownValue(t *testing.T) {
	s := newStand(t)

	_, err := s.accounts.ByField(s.ctx, "phone", "000000000")

	assert.ErrorIs(t, err, merchant.ErrNotFound)
}

// A profile may name a field this merchant does not keep. That is a miss, not
// a crash, and it must never reach the database as an interpolated column.
func TestE2EAccountByFieldRejectsAnUnknownField(t *testing.T) {
	s := newStand(t)

	_, err := s.accounts.ByField(s.ctx, "id; DROP TABLE merchant.accounts", "1")

	assert.ErrorIs(t, err, merchant.ErrNotFound)

	// The table is still there, which is what the mapping exists to guarantee.
	var count int
	require.NoError(t, s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM merchant.accounts`).Scan(&count))
	assert.Equal(t, 1, count)
}

// Another stand's payer is invisible even when the phone number is identical.
func TestE2EAccountByFieldIsScopedToTheSandbox(t *testing.T) {
	s := newStand(t)
	other := testdb.Seed(t, s.pool, "other")
	testdb.SeedAccountAndOrder(t, s.pool, other, amount)

	acc, err := s.accounts.ByField(s.ctx, "phone", seedPhone)

	require.NoError(t, err)
	assert.Equal(t, s.accountID, acc.ID)
}

func TestE2EAccountByFieldNeedsASandbox(t *testing.T) {
	s := newStand(t)

	_, err := s.accounts.ByField(context.Background(), "phone", seedPhone)

	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)
}

func TestE2EOrderByIDLoadsTheOrder(t *testing.T) {
	s := newStand(t)

	order, err := s.orders.ByID(s.ctx, s.orderID)

	require.NoError(t, err)
	assert.Equal(t, s.orderID, order.ID)
	assert.Equal(t, s.accountID, order.AccountID)
	assert.Equal(t, amount, order.Amount)
	assert.Equal(t, domain.StatusNew, order.Status)
}

func TestE2EOrderByIDReportsAMissingOrder(t *testing.T) {
	s := newStand(t)

	_, err := s.orders.ByID(s.ctx, s.orderID+1000)

	assert.ErrorIs(t, err, merchant.ErrNotFound)
}

func TestE2EOrderByIDNeedsASandbox(t *testing.T) {
	s := newStand(t)

	_, err := s.orders.ByID(context.Background(), s.orderID)

	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)
}

func TestE2EOrderByAccountListsOldestFirst(t *testing.T) {
	s := newStand(t)
	second := insertOrder(t, s, amount*2)

	orders, err := s.orders.ByAccount(s.ctx, s.accountID)

	require.NoError(t, err)
	require.Len(t, orders, 2)
	assert.Equal(t, s.orderID, orders[0].ID)
	assert.Equal(t, second, orders[1].ID)
}

func TestE2EOrderByAccountIsEmptyForAnUnknownAccount(t *testing.T) {
	s := newStand(t)

	orders, err := s.orders.ByAccount(s.ctx, s.accountID+1000)

	require.NoError(t, err)
	assert.Empty(t, orders)
}

func TestE2EOrderByAccountNeedsASandbox(t *testing.T) {
	s := newStand(t)

	_, err := s.orders.ByAccount(context.Background(), s.accountID)

	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)
}

func TestE2EOrderUpdatePersistsTheStatus(t *testing.T) {
	s := newStand(t)

	order, err := s.orders.ByID(s.ctx, s.orderID)
	require.NoError(t, err)
	order.MarkPaid()

	require.NoError(t, s.orders.Update(s.ctx, order))

	reloaded, err := s.orders.ByID(s.ctx, s.orderID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusPaid, reloaded.Status)
}

func TestE2EOrderUpdateReportsAMissingOrder(t *testing.T) {
	s := newStand(t)

	err := s.orders.Update(s.ctx, &domain.Order{ID: s.orderID + 1000, Status: domain.StatusPaid})

	assert.ErrorIs(t, err, merchant.ErrNotFound)
}

func TestE2EOrderUpdateNeedsASandbox(t *testing.T) {
	s := newStand(t)

	err := s.orders.Update(context.Background(), &domain.Order{ID: s.orderID})

	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)
}

// The whole use case runs in one transaction, so an order update must roll
// back with everything else rather than survive on its own.
func TestE2EOrderUpdateRollsBackWithTheTransaction(t *testing.T) {
	s := newStand(t)
	boom := errors.New("use case failed")

	err := postgres.WithTx(s.ctx, s.pool, func(inner context.Context) error {
		order, err := s.orders.ByID(inner, s.orderID)
		require.NoError(t, err)
		order.MarkPaid()

		require.NoError(t, s.orders.Update(inner, order))

		return boom
	})

	assert.ErrorIs(t, err, boom)

	reloaded, err := s.orders.ByID(s.ctx, s.orderID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusNew, reloaded.Status)
}

// A row that no longer fits the domain must be reported, not skipped: a
// statement that silently dropped it would read as a shorter, healthy result.
func TestE2EOrderScanReportsABadRow(t *testing.T) {
	s := newStand(t)

	// The schema forbids a NULL description, so the constraint is lifted to
	// produce the corrupt row a scan has to survive.
	_, err := s.pool.Exec(context.Background(),
		`ALTER TABLE merchant.orders ALTER COLUMN description DROP NOT NULL`)
	require.NoError(t, err)

	_, err = s.pool.Exec(context.Background(),
		`UPDATE merchant.orders SET description = NULL WHERE id = $1`, s.orderID)
	require.NoError(t, err)

	_, err = s.orders.ByID(s.ctx, s.orderID)
	assert.ErrorContains(t, err, "scan order")

	_, err = s.orders.ByAccount(s.ctx, s.accountID)
	assert.ErrorContains(t, err, "read orders")
}

// A database that has gone away must surface as an error from every method,
// not as an empty result that would read as "no such payer".
func TestE2EEveryBillingMethodReportsALostDatabase(t *testing.T) {
	s := newStand(t)
	s.pool.Close()

	t.Run("accounts.ByField", func(t *testing.T) {
		_, err := s.accounts.ByField(s.ctx, "phone", seedPhone)

		require.Error(t, err)
		assert.NotErrorIs(t, err, merchant.ErrNotFound, "a lost database is not a missing payer")
	})

	t.Run("orders.ByID", func(t *testing.T) {
		_, err := s.orders.ByID(s.ctx, s.orderID)

		require.Error(t, err)
		assert.NotErrorIs(t, err, merchant.ErrNotFound)
	})

	t.Run("orders.ByAccount", func(t *testing.T) {
		_, err := s.orders.ByAccount(s.ctx, s.accountID)

		assert.ErrorContains(t, err, "select orders")
	})

	t.Run("orders.Update", func(t *testing.T) {
		err := s.orders.Update(s.ctx, &domain.Order{ID: s.orderID, Status: domain.StatusPaid})

		assert.ErrorContains(t, err, "update order")
	})
}

func insertOrder(t *testing.T, s *stand, amount int64) int64 {
	t.Helper()

	var id int64
	require.NoError(t, s.pool.QueryRow(context.Background(), `
		INSERT INTO merchant.orders (sandbox_id, account_id, amount, status)
		VALUES ($1, $2, $3, 'new')
		RETURNING id`, s.sandboxID, s.accountID, amount).Scan(&id))

	return id
}

func TestE2EAccountByIDLoadsThePayer(t *testing.T) {
	s := newStand(t)

	acc, err := s.accounts.ByID(s.ctx, s.accountID)

	require.NoError(t, err)
	assert.Equal(t, s.accountID, acc.ID)
	assert.Equal(t, s.sandboxID, acc.SandboxID)
	assert.Equal(t, seedPhone, acc.Phone)
}

func TestE2EAccountByIDReportsAMissingPayer(t *testing.T) {
	s := newStand(t)

	_, err := s.accounts.ByID(s.ctx, s.accountID+1000)

	assert.ErrorIs(t, err, merchant.ErrNotFound)
}

// Another stand's payer is invisible even when their identifier is known.
func TestE2EAccountByIDIsScopedToTheSandbox(t *testing.T) {
	s := newStand(t)
	other := testdb.Seed(t, s.pool, "other")
	otherAccount, _ := testdb.SeedAccountAndOrder(t, s.pool, other, amount)

	_, err := s.accounts.ByID(s.ctx, otherAccount)

	assert.ErrorIs(t, err, merchant.ErrNotFound)
}

func TestE2EAccountByIDNeedsASandbox(t *testing.T) {
	s := newStand(t)

	_, err := s.accounts.ByID(context.Background(), s.accountID)

	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)
}

func TestE2EUpdateBalancePersistsTheMove(t *testing.T) {
	s := newStand(t)

	require.NoError(t, s.accounts.UpdateBalance(s.ctx, s.accountID, 750_000))

	acc, err := s.accounts.ByID(s.ctx, s.accountID)
	require.NoError(t, err)
	assert.Equal(t, int64(750_000), acc.Balance)
}

func TestE2EUpdateBalanceReportsAMissingPayer(t *testing.T) {
	s := newStand(t)

	err := s.accounts.UpdateBalance(s.ctx, s.accountID+1000, 1)

	assert.ErrorIs(t, err, merchant.ErrNotFound)
}

func TestE2EUpdateBalanceNeedsASandbox(t *testing.T) {
	s := newStand(t)

	err := s.accounts.UpdateBalance(context.Background(), s.accountID, 1)

	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)
}

// The balance moves inside the payment's transaction, so a failed settlement
// takes the move back with it.
func TestE2EUpdateBalanceRollsBackWithTheTransaction(t *testing.T) {
	s := newStand(t)
	boom := errors.New("settlement failed")

	err := postgres.WithTx(s.ctx, s.pool, func(inner context.Context) error {
		require.NoError(t, s.accounts.UpdateBalance(inner, s.accountID, 999))
		return boom
	})

	assert.ErrorIs(t, err, boom)

	acc, err := s.accounts.ByID(s.ctx, s.accountID)
	require.NoError(t, err)
	assert.Zero(t, acc.Balance)
}

func TestE2EBalanceMethodsReportALostDatabase(t *testing.T) {
	s := newStand(t)
	s.pool.Close()

	t.Run("ByID", func(t *testing.T) {
		_, err := s.accounts.ByID(s.ctx, s.accountID)

		require.Error(t, err)
		assert.NotErrorIs(t, err, merchant.ErrNotFound, "a lost database is not a missing payer")
	})

	t.Run("UpdateBalance", func(t *testing.T) {
		err := s.accounts.UpdateBalance(s.ctx, s.accountID, 1)

		assert.ErrorContains(t, err, "update balance")
	})
}

func TestE2EWalkInRegistersPayerAndOrder(t *testing.T) {
	s := newStand(t)

	order, err := s.walkIns.Register(s.ctx, "order-abc", amount)

	require.NoError(t, err)
	assert.Equal(t, s.sandboxID, order.SandboxID)
	assert.Equal(t, amount, order.Amount)
	assert.Equal(t, domain.StatusNew, order.Status)
	assert.True(t, order.Payable())

	// The payer is stored under the value the account arrived with, so the
	// console shows who was registered rather than a blank row.
	acc, err := s.accounts.ByField(s.ctx, "login", "order-abc")
	require.NoError(t, err)
	assert.Equal(t, order.AccountID, acc.ID)
}

// The check and the payment that follows it register the same thing, and must
// get one order rather than two.
func TestE2EWalkInReusesThePayableOrder(t *testing.T) {
	s := newStand(t)

	first, err := s.walkIns.Register(s.ctx, "order-abc", amount)
	require.NoError(t, err)

	second, err := s.walkIns.Register(s.ctx, "order-abc", amount)
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, first.AccountID, second.AccountID)
}

// A settled order cannot be paid again, so the next payment is given a new one
// under the same payer.
func TestE2EWalkInRegistersAgainOnceTheOrderIsPaid(t *testing.T) {
	s := newStand(t)

	first, err := s.walkIns.Register(s.ctx, "order-abc", amount)
	require.NoError(t, err)

	first.MarkPaid()
	require.NoError(t, s.orders.Update(s.ctx, first))

	second, err := s.walkIns.Register(s.ctx, "order-abc", amount)

	require.NoError(t, err)
	assert.NotEqual(t, first.ID, second.ID)
	assert.Equal(t, first.AccountID, second.AccountID)
	assert.True(t, second.Payable())
}

// A different amount is a different bill: reusing the order would settle the
// wrong one, and CheckAmount would refuse it anyway.
func TestE2EWalkInSeparatesOrdersByAmount(t *testing.T) {
	s := newStand(t)

	first, err := s.walkIns.Register(s.ctx, "order-abc", amount)
	require.NoError(t, err)

	second, err := s.walkIns.Register(s.ctx, "order-abc", amount+1)

	require.NoError(t, err)
	assert.NotEqual(t, first.ID, second.ID)
	assert.Equal(t, amount+1, second.Amount)
}

// Registering is a write like any other: without a sandbox it must fail rather
// than create a row belonging to no stand.
func TestE2EWalkInNeedsASandbox(t *testing.T) {
	s := newStand(t)

	_, err := s.walkIns.Register(context.Background(), "order-abc", amount)

	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)
}

// A stand that no longer exists cannot have a payer registered against it. The
// insert fails on the foreign key, and the failure must surface rather than be
// reported as a payer who simply could not be found.
func TestE2EWalkInFailsForAnUnknownSandbox(t *testing.T) {
	s := newStand(t)

	ctx := sandboxctx.With(context.Background(), sandboxctx.Sandbox{ID: s.sandboxID + 10_000})

	_, err := s.walkIns.Register(ctx, "order-abc", amount)

	require.Error(t, err)
	assert.NotErrorIs(t, err, merchant.ErrNotFound)
}
