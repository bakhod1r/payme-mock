package postgres_test

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres/testdb"
)

func TestConnectRejectsAMalformedURL(t *testing.T) {
	_, err := postgres.Connect(context.Background(), "://not a url")

	assert.ErrorContains(t, err, "open pool")
}

func TestConnectReportsAnUnreachableDatabase(t *testing.T) {
	// Port 1 is reserved and nothing listens on it, so the ping must fail
	// rather than the call hanging or appearing to succeed.
	_, err := postgres.Connect(context.Background(),
		"postgres://payme:payme@127.0.0.1:1/paymemock?sslmode=disable&connect_timeout=1")

	assert.ErrorContains(t, err, "ping database")
}

func TestE2EMigrateIsIdempotent(t *testing.T) {
	pool := testdb.New(t) // already migrated once

	err := postgres.Migrate(context.Background(), pool)

	assert.NoError(t, err, "running migrations again must be a no-op")
}

// A binary shipped without its migrations must fail loudly rather than run
// against an empty schema and report mysterious missing-table errors later.
func TestE2EMigrateRefusesAnEmptyMigrationSet(t *testing.T) {
	pool := testdb.New(t)

	err := postgres.MigrateFS(context.Background(), pool, fstest.MapFS{})

	assert.ErrorContains(t, err, "prepare migrations")
}

func TestE2EMigrateReportsAClosedPool(t *testing.T) {
	pool := testdb.New(t)
	pool.Close()

	err := postgres.Migrate(context.Background(), pool)

	assert.Error(t, err)
}

func TestE2EWithTxCommits(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	err := postgres.WithTx(ctx, pool, func(ctx context.Context) error {
		_, err := postgres.From(ctx, pool).Exec(ctx, `
			INSERT INTO control.configs (name, settings) VALUES ('committed', '{}'::jsonb)`)
		return err
	})

	require.NoError(t, err)
	assert.Equal(t, 1, countConfigs(t, pool, "committed"))
}

func TestE2EWithTxRollsBackOnError(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	wantErr := errors.New("use case failed")

	err := postgres.WithTx(ctx, pool, func(ctx context.Context) error {
		if _, execErr := postgres.From(ctx, pool).Exec(ctx, `
			INSERT INTO control.configs (name, settings) VALUES ('rolled-back', '{}'::jsonb)`); execErr != nil {
			return execErr
		}
		return wantErr
	})

	assert.ErrorIs(t, err, wantErr)
	assert.Zero(t, countConfigs(t, pool, "rolled-back"), "the insert must not survive")
}

// A panic must not leave a transaction open holding locks.
func TestE2EWithTxRollsBackOnPanic(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	assert.Panics(t, func() {
		_ = postgres.WithTx(ctx, pool, func(ctx context.Context) error {
			_, _ = postgres.From(ctx, pool).Exec(ctx, `
				INSERT INTO control.configs (name, settings) VALUES ('panicked', '{}'::jsonb)`)
			panic("something went badly wrong")
		})
	})

	assert.Zero(t, countConfigs(t, pool, "panicked"))
}

// Nesting joins the open transaction rather than starting a second one, so a
// use case spanning several repositories stays atomic.
func TestE2EWithTxNestsIntoTheOpenTransaction(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	err := postgres.WithTx(ctx, pool, func(outer context.Context) error {
		return postgres.WithTx(outer, pool, func(inner context.Context) error {
			_, execErr := postgres.From(inner, pool).Exec(inner, `
				INSERT INTO control.configs (name, settings) VALUES ('nested', '{}'::jsonb)`)
			return execErr
		})
	})

	require.NoError(t, err)
	assert.Equal(t, 1, countConfigs(t, pool, "nested"))
}

func TestE2ENestedFailureRollsBackTheWholeUseCase(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	wantErr := errors.New("inner step failed")

	err := postgres.WithTx(ctx, pool, func(outer context.Context) error {
		if _, execErr := postgres.From(outer, pool).Exec(outer, `
			INSERT INTO control.configs (name, settings) VALUES ('outer-step', '{}'::jsonb)`); execErr != nil {
			return execErr
		}
		return postgres.WithTx(outer, pool, func(context.Context) error { return wantErr })
	})

	assert.ErrorIs(t, err, wantErr)
	assert.Zero(t, countConfigs(t, pool, "outer-step"), "the outer step rolls back too")
}

// When the connection dies before the rollback can run, both the use case's
// failure and the rollback failure must reach the caller: hiding either would
// leave a transaction whose fate nobody can explain.
func TestE2EWithTxReportsBothTheFailureAndAFailedRollback(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	wantErr := errors.New("use case failed")

	err := postgres.WithTx(ctx, pool, func(inner context.Context) error {
		// Kill this transaction's own backend, so the rollback that follows
		// has no connection left to run on.
		_, _ = postgres.From(inner, pool).Exec(inner,
			`SELECT pg_terminate_backend(pg_backend_pid())`)
		return wantErr
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr, "the original failure must not be swallowed")
}

func TestE2EWithTxReportsAClosedPool(t *testing.T) {
	pool := testdb.New(t)
	pool.Close()

	err := postgres.WithTx(context.Background(), pool, func(context.Context) error { return nil })

	assert.ErrorContains(t, err, "begin transaction")
}

// Outside a transaction the pool serves queries directly; inside one the open
// transaction does, which is what keeps a use case consistent.
func TestE2EFromSelectsTheRightQuerier(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	t.Run("the pool outside a transaction", func(t *testing.T) {
		var got int
		require.NoError(t, postgres.From(ctx, pool).QueryRow(ctx, `SELECT 1`).Scan(&got))
		assert.Equal(t, 1, got)
	})

	t.Run("the transaction inside one", func(t *testing.T) {
		require.NoError(t, postgres.WithTx(ctx, pool, func(inner context.Context) error {
			q := postgres.From(inner, pool)

			var scanned int
			if err := q.QueryRow(inner, `SELECT 2`).Scan(&scanned); err != nil {
				return err
			}
			assert.Equal(t, 2, scanned)

			rows, err := q.Query(inner, `SELECT generate_series(1, 3)`)
			if err != nil {
				return err
			}
			defer rows.Close()

			var count int
			for rows.Next() {
				count++
			}
			assert.Equal(t, 3, count)

			tag, err := q.Exec(inner, `
				INSERT INTO control.configs (name, settings) VALUES ('exec-in-tx', '{}'::jsonb)`)
			if err != nil {
				return err
			}
			assert.NotEmpty(t, tag.String())
			assert.Equal(t, int64(1), tag.RowsAffected())

			return nil
		}))
	})
}

func TestE2EPoolQuerierQuery(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	rows, err := postgres.From(ctx, pool).Query(ctx, `SELECT generate_series(1, 2)`)

	require.NoError(t, err)
	defer rows.Close()

	var count int
	for rows.Next() {
		count++
	}
	assert.Equal(t, 2, count)
}

func TestE2EPoolQuerierExec(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	tag, err := postgres.From(ctx, pool).Exec(ctx, `
		INSERT INTO control.configs (name, settings) VALUES ('exec-on-pool', '{}'::jsonb)`)

	require.NoError(t, err)
	assert.NotEmpty(t, tag.String())
	assert.Equal(t, int64(1), tag.RowsAffected())
	assert.Equal(t, 1, countConfigs(t, pool, "exec-on-pool"))
}

func countConfigs(t *testing.T, pool *postgres.Pool, name string) int {
	t.Helper()

	var count int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM control.configs WHERE name = $1`, name).Scan(&count))
	return count
}

// ---------- the call store ----------

// A payout asked for twice is two payouts, and the caller cannot tell a lost
// response from a lost request. The store is what lets the second one be
// answered with the first one's answer, so it is read here against the real
// table with the real jsonb column and the real primary key.
func TestE2ECallStoreRemembersAndRecalls(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	sandbox := testdb.Seed(t, pool, "qa")
	store := postgres.NewCallStore(pool)

	key := httpx.CallKey{
		SandboxID: sandbox,
		Method:    "transactions.create",
		RequestID: "42",
		BodyHash:  "hash-of-the-body",
	}

	_, found, err := store.Recall(ctx, key, time.Hour)
	require.NoError(t, err)
	assert.False(t, found, "nothing was answered yet")

	answer := []byte(`{"result":{"receipt":{"_id":"one"}}}`)
	require.NoError(t, store.Remember(ctx, key, answer))

	got, found, err := store.Recall(ctx, key, time.Hour)
	require.NoError(t, err)
	require.True(t, found)
	assert.JSONEq(t, string(answer), string(got))
}

// The window is what makes a replay a replay rather than an answer from last
// week: past it, the call is the caller's own again.
func TestE2ECallStoreForgetsPastTheWindow(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	sandbox := testdb.Seed(t, pool, "qa")
	store := postgres.NewCallStore(pool)

	key := httpx.CallKey{SandboxID: sandbox, Method: "receipts.create", RequestID: "7", BodyHash: "h"}
	require.NoError(t, store.Remember(ctx, key, []byte(`{"result":{}}`)))

	_, err := pool.Exec(ctx, `
		UPDATE control.idempotent_calls SET at = now() - interval '2 hours'`)
	require.NoError(t, err)

	_, found, err := store.Recall(ctx, key, time.Hour)
	require.NoError(t, err)
	assert.False(t, found)
}

// An id reused for different parameters is a different call. Recalling it would
// answer the second with the first's receipt, which is worse than doing the
// work twice, and storing over it would lose the answer the first caller holds.
func TestE2ECallStoreKeepsADifferentBodyApart(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	sandbox := testdb.Seed(t, pool, "qa")
	store := postgres.NewCallStore(pool)

	first := httpx.CallKey{SandboxID: sandbox, Method: "transactions.create", RequestID: "42", BodyHash: "one"}
	require.NoError(t, store.Remember(ctx, first, []byte(`{"result":{"receipt":{"_id":"one"}}}`)))

	second := first
	second.BodyHash = "another"

	_, found, err := store.Recall(ctx, second, time.Hour)
	require.NoError(t, err)
	assert.False(t, found, "a different body is a different call")

	// Storing the second leaves the first's answer where it is, so the caller
	// still holding that receipt id is answered with it.
	require.NoError(t, store.Remember(ctx, second, []byte(`{"result":{"receipt":{"_id":"other"}}}`)))

	got, found, err := store.Recall(ctx, first, time.Hour)
	require.NoError(t, err)
	require.True(t, found)
	assert.Contains(t, string(got), `"one"`)
}

// A retry that arrives while the first is still running settles on one answer
// rather than two rows the primary key would refuse.
func TestE2ECallStoreOverwritesTheSameCall(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	sandbox := testdb.Seed(t, pool, "qa")
	store := postgres.NewCallStore(pool)

	key := httpx.CallKey{SandboxID: sandbox, Method: "cards.create", RequestID: "9", BodyHash: "h"}

	require.NoError(t, store.Remember(ctx, key, []byte(`{"result":{"card":{"token":"a"}}}`)))
	require.NoError(t, store.Remember(ctx, key, []byte(`{"result":{"card":{"token":"b"}}}`)))

	got, found, err := store.Recall(ctx, key, time.Hour)
	require.NoError(t, err)
	require.True(t, found)
	assert.Contains(t, string(got), `"b"`)

	var rows int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM control.idempotent_calls`).Scan(&rows))
	assert.Equal(t, 1, rows, "one call, one row")
}

// A database that went away is reported. Read as "no earlier answer" it would
// turn every retry of a payout into a second payout, which is the exact failure
// the store exists to prevent.
func TestE2ECallStoreReportsALostDatabase(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	sandbox := testdb.Seed(t, pool, "qa")
	store := postgres.NewCallStore(pool)
	key := httpx.CallKey{SandboxID: sandbox, Method: "transactions.create", RequestID: "1", BodyHash: "h"}

	pool.Close()

	_, _, err := store.Recall(ctx, key, time.Hour)
	assert.ErrorContains(t, err, "recall call")

	err = store.Remember(ctx, key, []byte(`{"result":{}}`))
	assert.ErrorContains(t, err, "remember call")
}
