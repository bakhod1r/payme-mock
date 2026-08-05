package infrastructure_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/merchant/domain"
	"github.com/bakhod1r/payme-mock/internal/context/payment/merchant/infrastructure"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres/testdb"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

const (
	paymeID = "5305e3bab097f420a62ced0b"
	amount  = int64(500000)
	nowMs   = int64(1_399_114_284_039)
)

type stand struct {
	pool      *postgres.Pool
	repo      *infrastructure.TransactionRepository
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
		repo:      infrastructure.NewTransactionRepository(pool),
		ctx:       sandboxctx.With(context.Background(), sandboxctx.Sandbox{ID: sandboxID, Slug: "qa"}),
		sandboxID: sandboxID,
		accountID: accountID,
		orderID:   orderID,
	}
}

func (s *stand) newTransaction(paymeID string) *domain.Transaction {
	return &domain.Transaction{
		SandboxID: s.sandboxID, PaymeID: paymeID, OrderID: s.orderID,
		AccountID: s.accountID, Account: map[string]string{"phone": "901234567"},
		Amount: amount, State: domain.StateCreated,
		PaymeTime: nowMs, CreateTime: nowMs,
	}
}

func TestE2ECreateAndLoad(t *testing.T) {
	s := newStand(t)
	tx := s.newTransaction(paymeID)

	require.NoError(t, s.repo.Create(s.ctx, tx))
	assert.NotZero(t, tx.ID, "the row's identifier is assigned on insert")

	got, err := s.repo.ByPaymeID(s.ctx, paymeID)

	require.NoError(t, err)
	assert.Equal(t, tx.ID, got.ID)
	assert.Equal(t, amount, got.Amount)
	assert.Equal(t, domain.StateCreated, got.State)
	assert.Equal(t, map[string]string{"phone": "901234567"}, got.Account)
	assert.Nil(t, got.Reason)
}

func TestE2ELoadingAnAbsentTransaction(t *testing.T) {
	s := newStand(t)

	_, err := s.repo.ByPaymeID(s.ctx, "no-such-transaction")

	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// The unique index on (sandbox_id, payme_id) is the idempotency guarantee. A
// second insert of the same request must be reported as a duplicate so the
// caller replays the stored response.
func TestE2EDuplicatePaymeIDIsReported(t *testing.T) {
	s := newStand(t)
	require.NoError(t, s.repo.Create(s.ctx, s.newTransaction(paymeID)))

	err := s.repo.Create(s.ctx, s.newTransaction(paymeID))

	assert.ErrorIs(t, err, domain.ErrDuplicate)
}

// Fifty concurrent deliveries of the same request: exactly one row survives.
// No in-memory fake can prove this; the unique index has to.
func TestE2EConcurrentCreatesOfTheSameRequestYieldOneRow(t *testing.T) {
	s := newStand(t)

	const attempts = 50
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		succeeded  int
		duplicates int
	)

	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()

			err := s.repo.Create(s.ctx, s.newTransaction(paymeID))

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case assert.ErrorIs(t, err, domain.ErrDuplicate):
				duplicates++
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, succeeded, "exactly one delivery may create the transaction")
	assert.Equal(t, attempts-1, duplicates, "every other delivery is a duplicate")

	var rows int
	require.NoError(t, s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM merchant.transactions WHERE sandbox_id = $1 AND payme_id = $2`,
		s.sandboxID, paymeID).Scan(&rows))
	assert.Equal(t, 1, rows)
}

// One order may hold only one live transaction. Two different requests racing
// for the same order must not both succeed.
func TestE2EConcurrentCreatesOnOneOrderYieldOneActive(t *testing.T) {
	s := newStand(t)

	const attempts = 20
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)

	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(n int) {
			defer wg.Done()

			tx := s.newTransaction(paymeID + "-" + string(rune('a'+n)))

			if err := s.repo.Create(s.ctx, tx); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	assert.Equal(t, 1, succeeded, "an order accepts only one live transaction")
}

func TestE2EUpdatePersistsTheStateChange(t *testing.T) {
	s := newStand(t)
	tx := s.newTransaction(paymeID)
	require.NoError(t, s.repo.Create(s.ctx, tx))

	require.NoError(t, tx.Perform(nowMs+900, 43_200_000))
	require.NoError(t, s.repo.Update(s.ctx, tx))

	got, err := s.repo.ByPaymeID(s.ctx, paymeID)

	require.NoError(t, err)
	assert.Equal(t, domain.StatePerformed, got.State)
	assert.Equal(t, nowMs+900, got.PerformTime)
}

func TestE2EUpdatePersistsTheCancellationReason(t *testing.T) {
	s := newStand(t)
	tx := s.newTransaction(paymeID)
	require.NoError(t, s.repo.Create(s.ctx, tx))

	require.NoError(t, tx.Cancel(domain.ReasonTimeout, nowMs+1000))
	require.NoError(t, s.repo.Update(s.ctx, tx))

	got, err := s.repo.ByPaymeID(s.ctx, paymeID)

	require.NoError(t, err)
	assert.Equal(t, domain.StateCancelled, got.State)
	require.NotNil(t, got.Reason)
	assert.Equal(t, domain.ReasonTimeout, *got.Reason)
}

func TestE2EUpdatingAnAbsentRow(t *testing.T) {
	s := newStand(t)
	tx := s.newTransaction(paymeID)
	tx.ID = 999999

	err := s.repo.Update(s.ctx, tx)

	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// Cancelling frees the order, so a later payment attempt can create a new
// transaction against it.
func TestE2ECancellingReleasesTheOrder(t *testing.T) {
	s := newStand(t)
	first := s.newTransaction(paymeID)
	require.NoError(t, s.repo.Create(s.ctx, first))
	require.NoError(t, first.Cancel(1, nowMs+10))
	require.NoError(t, s.repo.Update(s.ctx, first))

	second := s.newTransaction("second-payme-id")

	assert.NoError(t, s.repo.Create(s.ctx, second))
}

func TestE2EActiveByOrder(t *testing.T) {
	s := newStand(t)

	t.Run("reports nothing before a payment", func(t *testing.T) {
		_, err := s.repo.ActiveByOrder(s.ctx, s.orderID)

		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	tx := s.newTransaction(paymeID)
	require.NoError(t, s.repo.Create(s.ctx, tx))

	t.Run("finds the live transaction", func(t *testing.T) {
		got, err := s.repo.ActiveByOrder(s.ctx, s.orderID)

		require.NoError(t, err)
		assert.Equal(t, tx.ID, got.ID)
	})

	t.Run("reports nothing once it is cancelled", func(t *testing.T) {
		require.NoError(t, tx.Cancel(1, nowMs+10))
		require.NoError(t, s.repo.Update(s.ctx, tx))

		_, err := s.repo.ActiveByOrder(s.ctx, s.orderID)

		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
}

// The protocol says the period bounds are inclusive and the order is oldest
// first, which reconciliation depends on.
func TestE2EStatementBoundsAreInclusiveAndOrdered(t *testing.T) {
	s := newStand(t)

	times := []int64{100, 200, 300, 400}
	for i, at := range times {
		tx := s.newTransaction(paymeID + string(rune('a'+i)))
		tx.CreateTime = at
		tx.OrderID = 0 // unattached, so the one-active-per-order rule allows them all
		require.NoError(t, s.repo.Create(s.ctx, tx))
	}

	got, err := s.repo.Statement(s.ctx, 100, 300)

	require.NoError(t, err)
	require.Len(t, got, 3, "both bounds are inclusive")
	assert.Equal(t, int64(100), got[0].CreateTime)
	assert.Equal(t, int64(200), got[1].CreateTime)
	assert.Equal(t, int64(300), got[2].CreateTime, "results are oldest first")
}

func TestE2EStatementOverAQuietPeriod(t *testing.T) {
	s := newStand(t)

	got, err := s.repo.Statement(s.ctx, 1, 2)

	require.NoError(t, err)
	assert.Empty(t, got)
}

// Two sandboxes may hold the same Payme identifier without colliding, and
// neither can see the other's rows.
func TestE2ESandboxesAreIsolated(t *testing.T) {
	s := newStand(t)

	otherSandbox := testdb.Seed(t, s.pool, "dev")
	otherAccount, otherOrder := testdb.SeedAccountAndOrder(t, s.pool, otherSandbox, amount)
	otherCtx := sandboxctx.With(context.Background(), sandboxctx.Sandbox{ID: otherSandbox, Slug: "dev"})

	require.NoError(t, s.repo.Create(s.ctx, s.newTransaction(paymeID)))

	other := &domain.Transaction{
		SandboxID: otherSandbox, PaymeID: paymeID, OrderID: otherOrder,
		AccountID: otherAccount, Account: map[string]string{"phone": "901234567"},
		Amount: amount, State: domain.StateCreated, PaymeTime: nowMs, CreateTime: nowMs,
	}

	require.NoError(t, s.repo.Create(otherCtx, other),
		"the same Payme id in another sandbox must not collide")

	fromQA, err := s.repo.ByPaymeID(s.ctx, paymeID)
	require.NoError(t, err)
	fromDev, err := s.repo.ByPaymeID(otherCtx, paymeID)
	require.NoError(t, err)

	assert.NotEqual(t, fromQA.ID, fromDev.ID, "each sandbox sees only its own row")

	statement, err := s.repo.Statement(s.ctx, 0, nowMs*2)
	require.NoError(t, err)
	assert.Len(t, statement, 1, "a statement must not leak another sandbox's transactions")
}

// A repository must never run unscoped: without a sandbox the query would
// span every stand at once.
func TestE2EUnscopedContextIsRefused(t *testing.T) {
	s := newStand(t)
	bare := context.Background()

	_, err := s.repo.ByPaymeID(bare, paymeID)
	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)

	_, err = s.repo.Statement(bare, 0, 1)
	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)

	_, err = s.repo.ActiveByOrder(bare, s.orderID)
	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)
}

// A database that has gone away must surface as an error from every method,
// not as an empty result that would read as "no such transaction".
func TestE2EEveryMethodReportsALostDatabase(t *testing.T) {
	s := newStand(t)
	require.NoError(t, s.repo.Create(s.ctx, s.newTransaction(paymeID)))
	s.pool.Close()

	t.Run("ByPaymeID", func(t *testing.T) {
		_, err := s.repo.ByPaymeID(s.ctx, paymeID)

		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrNotFound, "a lost database is not a missing row")
	})

	t.Run("ActiveByOrder", func(t *testing.T) {
		_, err := s.repo.ActiveByOrder(s.ctx, s.orderID)

		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("Create", func(t *testing.T) {
		err := s.repo.Create(s.ctx, s.newTransaction("another-id"))

		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrDuplicate, "a lost database is not a duplicate")
	})

	t.Run("Update", func(t *testing.T) {
		tx := s.newTransaction(paymeID)
		tx.ID = 1

		err := s.repo.Update(s.ctx, tx)

		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("Statement", func(t *testing.T) {
		_, err := s.repo.Statement(s.ctx, 0, nowMs*2)

		assert.Error(t, err)
	})
}

// Rows written outside the application could hold anything. Corrupt JSON must
// be reported rather than silently yielding an empty account.
func TestE2ECorruptStoredJSONIsReported(t *testing.T) {
	tests := []struct {
		name   string
		column string
		value  string
	}{
		{"an account that is not an object", "account", `"just a string"`},
		{"receivers that are not an array", "receivers", `{"not":"an array"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStand(t)
			tx := s.newTransaction(paymeID)
			require.NoError(t, s.repo.Create(s.ctx, tx))

			_, err := s.pool.Exec(context.Background(),
				`UPDATE merchant.transactions SET `+tt.column+` = $1::jsonb WHERE id = $2`,
				tt.value, tx.ID)
			require.NoError(t, err)

			_, err = s.repo.ByPaymeID(s.ctx, paymeID)
			assert.ErrorContains(t, err, "decode")

			// A statement must fail on the same row rather than silently
			// skipping it and reporting a short reconciliation list.
			_, err = s.repo.Statement(s.ctx, 0, nowMs*2)
			assert.ErrorContains(t, err, "read statement")
		})
	}
}

func TestE2EReceiversRoundTrip(t *testing.T) {
	s := newStand(t)
	tx := s.newTransaction(paymeID)
	tx.Receivers = []domain.Receiver{
		{ID: "5305e3bab097f420a62ced0b", Amount: 200000},
		{ID: "4215e6bab097f420a62ced01", Amount: 300000},
	}
	require.NoError(t, s.repo.Create(s.ctx, tx))

	got, err := s.repo.ByPaymeID(s.ctx, paymeID)

	require.NoError(t, err)
	assert.Equal(t, tx.Receivers, got.Receivers)
}

// Row locks make two concurrent readers of the same transaction serialise, so
// the state machine cannot be entered twice at once.
func TestE2EConcurrentPerformYieldsOneTransition(t *testing.T) {
	s := newStand(t)
	require.NoError(t, s.repo.Create(s.ctx, s.newTransaction(paymeID)))

	const attempts = 20
	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		transitions int
	)

	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()

			err := postgres.WithTx(s.ctx, s.pool, func(ctx context.Context) error {
				tx, err := s.repo.ByPaymeID(ctx, paymeID)
				if err != nil {
					return err
				}
				if tx.State != domain.StateCreated {
					return nil // another delivery already performed it
				}
				if err := tx.Perform(nowMs+1, 43_200_000); err != nil {
					return err
				}
				if err := s.repo.Update(ctx, tx); err != nil {
					return err
				}
				mu.Lock()
				transitions++
				mu.Unlock()
				return nil
			})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, transitions, "the row lock lets exactly one delivery perform the transaction")

	got, err := s.repo.ByPaymeID(s.ctx, paymeID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatePerformed, got.State)
}
