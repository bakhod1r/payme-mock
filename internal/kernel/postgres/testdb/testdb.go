// Package testdb starts a real PostgreSQL for tests that must exercise SQL:
// unique indexes, row locks and concurrency behave nothing like an in-memory
// fake, and those are exactly the guarantees the payment flow relies on.
package testdb

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
)

// New starts a migrated PostgreSQL container and returns a pool onto it. The
// container is torn down when the test finishes.
//
// Tests calling this are skipped under -short, so the fast unit suite stays
// free of Docker.
func New(t *testing.T) *postgres.Pool {
	t.Helper()

	pool, _ := NewWithURL(t)

	return pool
}

// NewWithURL is New with the address the container answers on, for a test that
// has to hand the database to something that opens its own pool — a service
// booted the way its own main boots it, for one.
func NewWithURL(t *testing.T) (*postgres.Pool, string) {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping: needs Docker")
	}

	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("paymemock"),
		tcpostgres.WithUsername("payme"),
		tcpostgres.WithPassword("payme"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}

	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminate postgres: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	pool, err := postgres.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	return pool, dsn
}

// Seed inserts a sandbox and returns its identifier, since every table is
// scoped to one and nothing can be written without it.
func Seed(t *testing.T, pool *postgres.Pool, slug string) int64 {
	t.Helper()

	ctx := context.Background()

	var configID int64
	err := pool.QueryRow(ctx, `
		INSERT INTO control.configs (name, description, settings, builtin)
		VALUES ($1, 'test profile', '{}'::jsonb, false)
		RETURNING id`, "config-"+slug).Scan(&configID)
	if err != nil {
		t.Fatalf("seed config: %v", err)
	}

	var sandboxID int64
	err = pool.QueryRow(ctx, `
		INSERT INTO control.sandboxes (slug, name, merchant_id, key, test_key, active_config_id)
		VALUES ($1, $1, $2, 'live-key', 'test-key', $3)
		RETURNING id`, slug, "merchant-"+slug, configID).Scan(&sandboxID)
	if err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}

	return sandboxID
}

// SeedAccountAndOrder inserts a payer and a payable order, returning both ids.
func SeedAccountAndOrder(t *testing.T, pool *postgres.Pool, sandboxID, amount int64) (accountID, orderID int64) {
	t.Helper()

	ctx := context.Background()

	err := pool.QueryRow(ctx, `
		INSERT INTO merchant.accounts (sandbox_id, phone, name)
		VALUES ($1, '901234567', 'Test payer')
		RETURNING id`, sandboxID).Scan(&accountID)
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}

	err = pool.QueryRow(ctx, `
		INSERT INTO merchant.orders (sandbox_id, account_id, amount, status)
		VALUES ($1, $2, $3, 'new')
		RETURNING id`, sandboxID, accountID, amount).Scan(&orderID)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}

	return accountID, orderID
}
