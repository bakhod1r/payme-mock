// Package infrastructure implements the billing ports against PostgreSQL.
// Every query is scoped to the sandbox carried in the request context, so one
// stand can never read or write another's accounts and orders.
package infrastructure

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
	merchant "github.com/bakhod1r/payme-mock/internal/context/payment/merchant/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// accountFields are the account object keys a payer may be looked up by,
// mapped to the condition that resolves each one.
//
// The field name reaches this layer from the configuration profile, so it is
// mapped through this table rather than interpolated: a profile is
// operator-supplied data and must never become part of a statement.
//
// The default profile identifies payers by order_id, which is not a column on
// the payer at all but the order they are paying, so that key resolves through
// a subquery instead.
var accountFields = map[string]string{
	"phone": `phone = $2`,
	"login": `login = $2`,
	"order_id": `id = (SELECT account_id FROM merchant.orders
	                   WHERE sandbox_id = $1 AND id::text = $2)`,
}

// AccountRepository resolves the `account` object into a known payer.
type AccountRepository struct {
	pool *postgres.Pool
}

// NewAccountRepository wires the repository to a pool.
func NewAccountRepository(pool *postgres.Pool) *AccountRepository {
	return &AccountRepository{pool: pool}
}

// ByField looks a payer up by one account field.
//
// An unknown field is reported as a missing account rather than an internal
// failure: from the protocol's side a profile naming a field this merchant
// does not keep is the same as no payer matching it.
func (r *AccountRepository) ByField(ctx context.Context, field, value string) (*domain.Account, error) {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return nil, err
	}

	condition, ok := accountFields[field]
	if !ok {
		return nil, merchant.ErrNotFound
	}

	row := postgres.From(ctx, r.pool).QueryRow(ctx, `
		SELECT id, sandbox_id, coalesce(phone, ''), coalesce(login, ''), name, balance, blocked
		FROM merchant.accounts
		WHERE sandbox_id = $1 AND `+condition, sandboxID, value)

	return scanAccount(row)
}

func scanAccount(row scanner) (*domain.Account, error) {
	var acc domain.Account

	err := row.Scan(&acc.ID, &acc.SandboxID, &acc.Phone, &acc.Login,
		&acc.Name, &acc.Balance, &acc.Blocked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, merchant.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan account: %w", err)
	}

	return &acc, nil
}

// ByID loads a payer for a balance move.
//
// The row is locked for the caller's transaction, so two payments settling at
// once serialise instead of both reading the same balance and one of the two
// moves being lost.
func (r *AccountRepository) ByID(ctx context.Context, id int64) (*domain.Account, error) {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return nil, err
	}

	row := postgres.From(ctx, r.pool).QueryRow(ctx, `
		SELECT id, sandbox_id, coalesce(phone, ''), coalesce(login, ''), name, balance, blocked
		FROM merchant.accounts
		WHERE sandbox_id = $1 AND id = $2
		FOR UPDATE`, sandboxID, id)

	return scanAccount(row)
}

// Register loads the stand's own payer: the one created with the sandbox, which
// is the row its balance column speaks for.
//
// The row is locked for the caller's transaction, so two payments settling at
// once serialise instead of both reading the same balance.
func (r *AccountRepository) Register(ctx context.Context) (*domain.Account, error) {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return nil, err
	}

	row := postgres.From(ctx, r.pool).QueryRow(ctx, `
		SELECT id, sandbox_id, coalesce(phone, ''), coalesce(login, ''), name,
		       balance, blocked
		FROM merchant.accounts
		WHERE sandbox_id = $1
		ORDER BY id
		LIMIT 1
		FOR UPDATE`, sandboxID)

	return scanAccount(row)
}

// UpdateBalance stores a moved balance and records the move.
//
// The history row is written in the same statement as the update, from the
// pre-image the update reads: a move recorded separately could be lost while
// the balance still changed, and a balance nobody can explain is worse than no
// history at all.
func (r *AccountRepository) UpdateBalance(ctx context.Context, id, balance int64) error {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return err
	}

	tag, err := postgres.From(ctx, r.pool).Exec(ctx, `
		WITH before AS (
			SELECT id, balance FROM merchant.accounts
			WHERE sandbox_id = $2 AND id = $3
		), moved AS (
			UPDATE merchant.accounts SET balance = $1
			WHERE sandbox_id = $2 AND id = $3
			RETURNING id, balance
		)
		INSERT INTO merchant.balance_events
			(sandbox_id, account_id, source, delta, balance_before, balance_after, note)
		SELECT $2, moved.id, 'payment', moved.balance - before.balance,
		       before.balance, moved.balance, 'settled by a payment'
		FROM moved JOIN before ON before.id = moved.id`,
		balance, sandboxID, id)
	if err != nil {
		return fmt.Errorf("update balance: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return merchant.ErrNotFound
	}

	return nil
}

// OrderRepository loads and stores orders.
type OrderRepository struct {
	pool *postgres.Pool
}

// NewOrderRepository wires the repository to a pool.
func NewOrderRepository(pool *postgres.Pool) *OrderRepository {
	return &OrderRepository{pool: pool}
}

const orderColumns = `id, sandbox_id, account_id, amount, status, description`

// ByID loads a single order.
func (r *OrderRepository) ByID(ctx context.Context, id int64) (*domain.Order, error) {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return nil, err
	}

	// The row is locked for the caller's transaction, so two deliveries
	// settling the same order serialise instead of both marking it paid.
	row := postgres.From(ctx, r.pool).QueryRow(ctx, `
		SELECT `+orderColumns+`
		FROM merchant.orders
		WHERE sandbox_id = $1 AND id = $2
		FOR UPDATE`, sandboxID, id)

	return scanOrder(row)
}

// ByAccount lists an account's orders, oldest first, which is the order the
// service walks looking for the one still payable.
func (r *OrderRepository) ByAccount(ctx context.Context, accountID int64) ([]*domain.Order, error) {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := postgres.From(ctx, r.pool).Query(ctx, `
		SELECT `+orderColumns+`
		FROM merchant.orders
		WHERE sandbox_id = $1 AND account_id = $2
		ORDER BY id`, sandboxID, accountID)
	if err != nil {
		return nil, fmt.Errorf("select orders: %w", err)
	}

	// CollectRows drains, closes and reports a stream that failed partway, so
	// a connection lost mid-result cannot be mistaken for a short list.
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*domain.Order, error) {
		return scanOrder(row)
	})
	if err != nil {
		return nil, fmt.Errorf("read orders: %w", err)
	}

	return out, nil
}

// Update persists a status change.
func (r *OrderRepository) Update(ctx context.Context, o *domain.Order) error {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return err
	}

	tag, err := postgres.From(ctx, r.pool).Exec(ctx, `
		UPDATE merchant.orders
		SET status = $1, updated_at = now()
		WHERE sandbox_id = $2 AND id = $3`, string(o.Status), sandboxID, o.ID)
	if err != nil {
		return fmt.Errorf("update order: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return merchant.ErrNotFound
	}

	return nil
}

// scanner is satisfied by both a single row and a row from a result set.
type scanner interface {
	Scan(dest ...any) error
}

func scanOrder(row scanner) (*domain.Order, error) {
	var (
		order  domain.Order
		status string
	)

	err := row.Scan(&order.ID, &order.SandboxID, &order.AccountID,
		&order.Amount, &status, &order.Description)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, merchant.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan order: %w", err)
	}

	order.Status = domain.OrderStatus(status)

	return &order, nil
}

// WalkInRepository registers payers the merchant has never seen.
type WalkInRepository struct {
	pool *postgres.Pool
}

// NewWalkInRepository wires the repository to a pool.
func NewWalkInRepository(pool *postgres.Pool) *WalkInRepository {
	return &WalkInRepository{pool: pool}
}

// Register returns the order a walk-in payer pays, creating the payer and the
// order the first time either is asked for.
//
// The payer is stored under the value the account arrived with, and an order
// already payable for the same amount is reused: CheckPerformTransaction and
// the CreateTransaction that follows it ask for the same thing, and each must
// get the same order rather than leaving a trail of unpaid duplicates.
func (r *WalkInRepository) Register(ctx context.Context, value string, amount int64) (*domain.Order, error) {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return nil, err
	}

	// ON CONFLICT DO UPDATE rather than DO NOTHING: a concurrent insert of the
	// same payer must still return the row, and DO NOTHING returns none.
	var accountID int64
	err = postgres.From(ctx, r.pool).QueryRow(ctx, `
		INSERT INTO merchant.accounts (sandbox_id, login, name)
		VALUES ($1, $2, $2)
		ON CONFLICT (sandbox_id, login) WHERE login IS NOT NULL
		DO UPDATE SET login = EXCLUDED.login
		RETURNING id`, sandboxID, value).Scan(&accountID)
	if err != nil {
		return nil, fmt.Errorf("register walk-in account: %w", err)
	}

	row := postgres.From(ctx, r.pool).QueryRow(ctx, `
		WITH existing AS (
			SELECT `+orderColumns+`
			FROM merchant.orders
			WHERE sandbox_id = $1 AND account_id = $2 AND amount = $3
			  AND status IN ('new', 'processing')
			ORDER BY id
			LIMIT 1
		), created AS (
			INSERT INTO merchant.orders (sandbox_id, account_id, amount, description)
			SELECT $1, $2, $3, 'registered on arrival'
			WHERE NOT EXISTS (SELECT 1 FROM existing)
			RETURNING `+orderColumns+`
		)
		SELECT * FROM existing
		UNION ALL
		SELECT * FROM created`, sandboxID, accountID, amount)

	return scanOrder(row)
}
