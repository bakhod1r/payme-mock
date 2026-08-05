package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	billing "github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"

	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
)

// errNotFound reports an edit or a delete aimed at a row that is not there.
//
// The console shows it as a sentence on the screen rather than a failure: a
// row someone else removed while this page was open is not a fault.
var errNotFound = errors.New("that is gone")

// orderRow is an order as the orders screen shows it.
type orderRow struct {
	ID          int64
	Sandbox     string
	AccountID   int64
	Payer       string
	Amount      int64
	Status      string
	Description string
	// Locked reports an order a transaction already holds, which may not be
	// edited without the payment and the order disagreeing.
	Locked bool
}

// UpdateSandbox renames a sandbox, switches its profile, sets which way its
// register moves money and names the merchant it belongs to.
func (s *store) UpdateSandbox(ctx context.Context, id int64, name string,
	configID *int64, kind, group, merchantName string,
) error {
	if !billing.Kind(kind).Valid() {
		return fmt.Errorf("unknown register kind %q", kind)
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE control.sandboxes
		SET name = $1, active_config_id = $2, kind = $3,
		    merchant_group = nullif($4, ''),
		    merchant_name = nullif($5, '')
		WHERE id = $6`, name, configID, kind, strings.TrimSpace(group),
		strings.TrimSpace(merchantName), id)
	if err != nil {
		return fmt.Errorf("update sandbox: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return errNotFound
	}

	return nil
}

// ResetSandbox clears a stand's data while keeping its credentials, so an
// integration under test can start over without repointing at a new endpoint.
//
// The payer survives with its balance zeroed rather than being deleted: a
// stand with no payer cannot answer a single call.
func (s *store) ResetSandbox(ctx context.Context, id int64) error {
	return postgres.WithTx(ctx, s.pool, func(inner context.Context) error {
		var exists bool
		if err := postgres.From(inner, s.pool).QueryRow(inner,
			`SELECT EXISTS (SELECT 1 FROM control.sandboxes WHERE id = $1)`, id).Scan(&exists); err != nil {
			return fmt.Errorf("check sandbox: %w", err)
		}
		if !exists {
			return errNotFound
		}

		statements := []string{
			// Transactions reference orders, so they go first.
			`DELETE FROM merchant.transactions WHERE sandbox_id = $1`,
			`DELETE FROM merchant.orders WHERE sandbox_id = $1`,
			`DELETE FROM control.request_log WHERE sandbox_id = $1`,
			`UPDATE merchant.accounts SET balance = 0, blocked = FALSE WHERE sandbox_id = $1`,
		}

		for _, statement := range statements {
			if _, err := postgres.From(inner, s.pool).Exec(inner, statement, id); err != nil {
				return fmt.Errorf("reset sandbox: %w", err)
			}
		}

		return nil
	})
}

// UpdateAccount edits a payer's details, leaving the balance to its own
// operations so a rename cannot quietly change what the register holds.
func (s *store) UpdateAccount(ctx context.Context, id int64, name, phone string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE merchant.accounts SET name = $1, phone = $2 WHERE id = $3`,
		name, nullableText(phone), id)
	if err != nil {
		return fmt.Errorf("update payer: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return errNotFound
	}

	return nil
}

// DeleteAccount removes a payer and, through the schema's cascades, their
// orders and payments.
func (s *store) DeleteAccount(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM merchant.accounts WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete payer: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return errNotFound
	}

	return nil
}

// Orders lists the orders of every stand, newest first.
func (s *store) Orders(ctx context.Context, query string, limit int) ([]orderRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, s.slug, o.account_id, a.name, o.amount, o.status,
		       o.description,
		       EXISTS (SELECT 1 FROM merchant.transactions t
		               WHERE t.order_id = o.id AND t.state IN (1, 2))
		FROM merchant.orders o
		JOIN control.sandboxes s ON s.id = o.sandbox_id
		JOIN merchant.accounts a ON a.id = o.account_id
		WHERE $1 = '' OR o.id::text = $1 OR a.name ILIKE '%' || $1 || '%'
		   OR o.description ILIKE '%' || $1 || '%' OR s.slug ILIKE '%' || $1 || '%'
		ORDER BY o.id DESC
		LIMIT $2`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("select orders: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (orderRow, error) {
		var out orderRow
		err := row.Scan(&out.ID, &out.Sandbox, &out.AccountID, &out.Payer,
			&out.Amount, &out.Status, &out.Description, &out.Locked)
		return out, err
	})
}

// OrdersForSandbox lists one stand's orders, newest first.
func (s *store) OrdersForSandbox(ctx context.Context, sandboxID int64, limit int) ([]orderRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, s.slug, o.account_id, a.name, o.amount, o.status,
		       o.description,
		       EXISTS (SELECT 1 FROM merchant.transactions t
		               WHERE t.order_id = o.id AND t.state IN (1, 2))
		FROM merchant.orders o
		JOIN control.sandboxes s ON s.id = o.sandbox_id
		JOIN merchant.accounts a ON a.id = o.account_id
		WHERE o.sandbox_id = $1
		ORDER BY o.id DESC
		LIMIT $2`, sandboxID, limit)
	if err != nil {
		return nil, fmt.Errorf("select sandbox orders: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (orderRow, error) {
		var out orderRow
		err := row.Scan(&out.ID, &out.Sandbox, &out.AccountID, &out.Payer,
			&out.Amount, &out.Status, &out.Description, &out.Locked)
		return out, err
	})
}

// SandboxAccountID returns the payer a stand bills, which is the one created
// with it.
func (s *store) SandboxAccountID(ctx context.Context, sandboxID int64) (int64, error) {
	var id int64

	err := s.pool.QueryRow(ctx, `
		SELECT id FROM merchant.accounts
		WHERE sandbox_id = $1 ORDER BY id LIMIT 1`, sandboxID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("select sandbox payer: %w", err)
	}

	return id, nil
}

// CreateOrder adds an order for a payer. The sandbox is taken from the payer
// rather than asked for, so the two can never disagree.
func (s *store) CreateOrder(ctx context.Context, accountID, amount int64, description string) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO merchant.orders (sandbox_id, account_id, amount, description)
		SELECT sandbox_id, id, $2, $3 FROM merchant.accounts WHERE id = $1`,
		accountID, amount, description)
	if err != nil {
		return fmt.Errorf("insert order: %w", err)
	}

	// No rows inserted means the payer was gone by the time the select ran.
	if tag.RowsAffected() == 0 {
		return errNotFound
	}

	return nil
}

// DeleteOrder removes an order.
func (s *store) DeleteOrder(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM merchant.orders WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete order: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return errNotFound
	}

	return nil
}

// UpdateTransactionState forces a payment into another state.
//
// This is not what the protocol does — a real transaction only moves through
// the state machine — but rehearsing what happens after a payment was cancelled
// upstream needs a way to put one there.
func (s *store) UpdateTransactionState(ctx context.Context, id int64, state int) error {
	if state != 1 && state != 2 && state != -1 && state != -2 {
		return fmt.Errorf("unknown transaction state %d", state)
	}

	// The parameter is cast once and compared through that cast: used bare it
	// would be deduced as smallint in one place and integer in another, which
	// PostgreSQL rejects outright.
	tag, err := s.pool.Exec(ctx, `
		UPDATE merchant.transactions
		SET state = $1::smallint,
		    perform_time = CASE WHEN $1::smallint = 2 AND perform_time = 0
		                        THEN extract(epoch FROM now())::bigint * 1000
		                        ELSE perform_time END,
		    cancel_time  = CASE WHEN $1::smallint < 0 AND cancel_time = 0
		                        THEN extract(epoch FROM now())::bigint * 1000
		                        ELSE cancel_time END,
		    updated_at = now()
		WHERE id = $2`, state, id)
	if err != nil {
		return fmt.Errorf("update transaction: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return errNotFound
	}

	return nil
}

// DeleteTransaction removes a payment and its audit trail.
func (s *store) DeleteTransaction(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM merchant.transactions WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete transaction: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return errNotFound
	}

	return nil
}

// DeleteTrafficEntry removes one recorded call.
func (s *store) DeleteTrafficEntry(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM control.request_log WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete traffic entry: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return errNotFound
	}

	return nil
}

// UpdateRule replaces what a rule does, keeping the method it is attached to.
func (s *store) UpdateRule(ctx context.Context, id int64, r newRule) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE control.fault_rules
		SET name = $1, method = $2, action = $3, delay_ms = $4, error_code = $5,
		    error_message = $6, probability = $7, times_left = $8, updated_at = now()
		WHERE id = $9`,
		ruleName(r), r.Method, string(r.Action), int(r.DelaySeconds*1000),
		nullableCode(r.ErrorCode), catalogMessage(r.ErrorCode), r.Probability,
		r.TimesLeft, id)
	if err != nil {
		return fmt.Errorf("update fault rule: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return errNotFound
	}

	return nil
}

// balanceEvent is one recorded balance move.
type balanceEvent struct {
	At      string
	Source  string
	Delta   int64
	Before  int64
	After   int64
	Note    string
	Payment string
	// Incoming reports which way the money went, so the screen can say it
	// without the reader parsing a sign.
	Incoming bool
}

// BalanceHistory returns a stand's balance moves, newest first.
func (s *store) BalanceHistory(ctx context.Context, sandboxID int64, limit int) ([]balanceEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+stamp("e.at")+`, e.source, e.delta,
		       e.balance_before, e.balance_after, e.note, coalesce(t.payme_id, '')
		FROM merchant.balance_events e
		LEFT JOIN merchant.transactions t ON t.id = e.transaction_id
		WHERE e.sandbox_id = $1
		ORDER BY e.at DESC, e.id DESC
		LIMIT $2`, sandboxID, limit)
	if err != nil {
		return nil, fmt.Errorf("select balance history: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (balanceEvent, error) {
		var out balanceEvent
		if err := row.Scan(&out.At, &out.Source, &out.Delta, &out.Before,
			&out.After, &out.Note, &out.Payment); err != nil {
			return balanceEvent{}, err
		}

		out.Incoming = out.Delta >= 0

		return out, nil
	})
}

// SandboxByID returns one stand, which is what its own page is built from.
func (s *store) SandboxByID(ctx context.Context, id int64, gatewayBase string) (sandboxRow, error) {
	// The list query already assembles everything the page shows, so the row is
	// picked out of it rather than kept in a second statement that would drift.
	sandboxes, err := s.Sandboxes(ctx, gatewayBase)
	if err != nil {
		return sandboxRow{}, err
	}

	for _, sandbox := range sandboxes {
		if sandbox.ID == id {
			return sandbox, nil
		}
	}

	return sandboxRow{}, errNotFound
}
