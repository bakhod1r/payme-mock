// Package infrastructure implements the merchant ports against PostgreSQL.
// Every query is scoped to the sandbox carried in the request context, so one
// stand can never read or write another's data.
package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bakhod1r/payme-mock/internal/context/payment/merchant/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// uniqueViolation is the SQLSTATE PostgreSQL raises when a unique index
// rejects a row. It is how a concurrent duplicate is detected.
const uniqueViolation = "23505"

// TransactionRepository stores the transaction aggregate.
type TransactionRepository struct {
	pool *postgres.Pool
}

// NewTransactionRepository wires the repository to a pool.
func NewTransactionRepository(pool *postgres.Pool) *TransactionRepository {
	return &TransactionRepository{pool: pool}
}

const transactionColumns = `
	id, sandbox_id, payme_id, order_id, account_id, account, amount, state,
	reason, payme_time, create_time, perform_time, cancel_time, receivers`

// ByPaymeID loads a transaction by the identifier Payme assigned it.
func (r *TransactionRepository) ByPaymeID(ctx context.Context, paymeID string) (*domain.Transaction, error) {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return nil, err
	}

	// The row is locked for the caller's transaction, so two concurrent
	// deliveries of the same request serialise instead of racing through the
	// state machine.
	row := postgres.From(ctx, r.pool).QueryRow(ctx, `
		SELECT `+transactionColumns+`
		FROM merchant.transactions
		WHERE sandbox_id = $1 AND payme_id = $2
		FOR UPDATE`, sandboxID, paymeID)

	return scanTransaction(row)
}

// Create stores a new transaction.
//
// A unique violation means another delivery of the same request won the race;
// the caller is told the row already exists so it can replay the stored
// response rather than reporting a failure.
func (r *TransactionRepository) Create(ctx context.Context, tx *domain.Transaction) error {
	// The account is a map of strings and receivers are plain structs, so
	// encoding cannot fail.
	account, _ := json.Marshal(tx.Account)
	receivers := marshalReceivers(tx.Receivers)

	err := postgres.From(ctx, r.pool).QueryRow(ctx, `
		INSERT INTO merchant.transactions
			(sandbox_id, payme_id, order_id, account_id, account, amount, state,
			 reason, payme_time, create_time, perform_time, cancel_time, receivers)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id`,
		tx.SandboxID, tx.PaymeID, nullableID(tx.OrderID), tx.AccountID, account,
		tx.Amount, tx.State, nullableReason(tx.Reason), tx.PaymeTime,
		tx.CreateTime, tx.PerformTime, tx.CancelTime, receivers,
	).Scan(&tx.ID)

	if isUniqueViolation(err) {
		return domain.ErrDuplicate
	}
	if err != nil {
		return fmt.Errorf("insert transaction: %w", err)
	}

	return nil
}

// Update persists a state change.
func (r *TransactionRepository) Update(ctx context.Context, tx *domain.Transaction) error {
	receivers := marshalReceivers(tx.Receivers)

	tag, err := postgres.From(ctx, r.pool).Exec(ctx, `
		UPDATE merchant.transactions
		SET state = $1, reason = $2, perform_time = $3, cancel_time = $4,
		    receivers = $5, updated_at = now()
		WHERE id = $6`,
		tx.State, nullableReason(tx.Reason), tx.PerformTime, tx.CancelTime, receivers, tx.ID)
	if err != nil {
		return fmt.Errorf("update transaction: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}

	return nil
}

// Statement lists transactions created within [from, to] inclusive, oldest
// first, which is the order the protocol requires for reconciliation.
func (r *TransactionRepository) Statement(ctx context.Context, from, to int64) ([]*domain.Transaction, error) {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := postgres.From(ctx, r.pool).Query(ctx, `
		SELECT `+transactionColumns+`
		FROM merchant.transactions
		WHERE sandbox_id = $1 AND create_time BETWEEN $2 AND $3
		ORDER BY create_time, id`, sandboxID, from, to)
	if err != nil {
		return nil, fmt.Errorf("select statement: %w", err)
	}

	// CollectRows drains, closes and reports a stream that failed partway, so
	// a connection lost mid-result cannot be mistaken for a short statement.
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*domain.Transaction, error) {
		return scanTransaction(row)
	})
	if err != nil {
		return nil, fmt.Errorf("read statement: %w", err)
	}

	return out, nil
}

// ActiveByOrder returns the order's live transaction, if it has one.
func (r *TransactionRepository) ActiveByOrder(ctx context.Context, orderID int64) (*domain.Transaction, error) {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return nil, err
	}

	row := postgres.From(ctx, r.pool).QueryRow(ctx, `
		SELECT `+transactionColumns+`
		FROM merchant.transactions
		WHERE sandbox_id = $1 AND order_id = $2 AND state IN (1, 2)`, sandboxID, orderID)

	return scanTransaction(row)
}

// scanner is satisfied by both a single row and a row from a result set.
type scanner interface {
	Scan(dest ...any) error
}

func scanTransaction(row scanner) (*domain.Transaction, error) {
	var (
		tx        domain.Transaction
		orderID   *int64
		reason    *int16
		account   []byte
		receivers []byte
	)

	err := row.Scan(
		&tx.ID, &tx.SandboxID, &tx.PaymeID, &orderID, &tx.AccountID, &account,
		&tx.Amount, &tx.State, &reason, &tx.PaymeTime, &tx.CreateTime,
		&tx.PerformTime, &tx.CancelTime, &receivers,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan transaction: %w", err)
	}

	if orderID != nil {
		tx.OrderID = *orderID
	}
	if reason != nil {
		r := domain.Reason(*reason)
		tx.Reason = &r
	}
	if err := json.Unmarshal(account, &tx.Account); err != nil {
		return nil, fmt.Errorf("decode account: %w", err)
	}
	if len(receivers) > 0 {
		if err := json.Unmarshal(receivers, &tx.Receivers); err != nil {
			return nil, fmt.Errorf("decode receivers: %w", err)
		}
	}

	return &tx, nil
}

// marshalReceivers encodes the optional payment split. Receivers are plain
// structs, so encoding cannot fail.
func marshalReceivers(receivers []domain.Receiver) []byte {
	if len(receivers) == 0 {
		return nil
	}
	raw, _ := json.Marshal(receivers)
	return raw
}

func nullableID(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}

func nullableReason(reason *domain.Reason) *int16 {
	if reason == nil {
		return nil
	}
	v := int16(*reason)
	return &v
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}
