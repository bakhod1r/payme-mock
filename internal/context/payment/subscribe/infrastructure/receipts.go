package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// ReceiptRepository stores the receipt aggregate.
type ReceiptRepository struct {
	pool *postgres.Pool
}

// NewReceiptRepository wires the repository to a pool.
func NewReceiptRepository(pool *postgres.Pool) *ReceiptRepository {
	return &ReceiptRepository{pool: pool}
}

const receiptColumns = `
	id, sandbox_id, receipt_id, merchant_id, amount, currency, commission,
	state, type, payout, hold, hold_expire, card_system, account, detail,
	description, card_id, payer, create_time, pay_time, cancel_time,
	coalesce(merchant_txn, '')`

// ByReceiptID loads a receipt by the identifier the provider assigned it.
//
// The row is locked for the caller's transaction, so a payment and the
// background step that advances it cannot both move the same receipt.
func (r *ReceiptRepository) ByReceiptID(ctx context.Context, receiptID string) (*domain.Receipt, error) {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return nil, err
	}

	row := postgres.From(ctx, r.pool).QueryRow(ctx, `
		SELECT `+receiptColumns+`
		FROM mock.receipts
		WHERE sandbox_id = $1 AND receipt_id = $2
		FOR UPDATE`, sandboxID, receiptID)

	return scanReceipt(row)
}

// Create stores a new receipt.
func (r *ReceiptRepository) Create(ctx context.Context, rec *domain.Receipt) error {
	// The account is a map of strings, so encoding it cannot fail; detail and
	// payer are free-form and arrive from the caller, so they can.
	account, _ := json.Marshal(rec.Account)

	detail, err := marshalOptional(rec.Detail)
	if err != nil {
		return fmt.Errorf("encode detail: %w", err)
	}

	payer, err := marshalOptional(rec.Payer)
	if err != nil {
		return fmt.Errorf("encode payer: %w", err)
	}

	err = postgres.From(ctx, r.pool).QueryRow(ctx, `
		INSERT INTO mock.receipts
			(sandbox_id, receipt_id, merchant_id, amount, currency, commission,
			 state, type, payout, hold, hold_expire, card_system, account,
			 detail, description, card_id, payer, create_time, pay_time,
			 cancel_time, merchant_txn)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		        $15, $16, $17, $18, $19, $20, nullif($21, ''))
		RETURNING id`,
		rec.SandboxID, rec.ReceiptID, rec.MerchantID, rec.Amount, rec.Currency,
		rec.Commission, rec.State, rec.Type, rec.Payout, rec.Hold, rec.HoldExpire,
		nullableSystem(rec.CardSystem), account, detail, rec.Description,
		rec.CardID, payer, rec.CreateTime, rec.PayTime, rec.CancelTime,
		rec.MerchantTxn,
	).Scan(&rec.ID)
	if err != nil {
		return fmt.Errorf("insert receipt: %w", err)
	}

	return nil
}

// Update persists a step of the state walk.
func (r *ReceiptRepository) Update(ctx context.Context, rec *domain.Receipt) error {
	payer, err := marshalOptional(rec.Payer)
	if err != nil {
		return fmt.Errorf("encode payer: %w", err)
	}

	tag, err := postgres.From(ctx, r.pool).Exec(ctx, `
		UPDATE mock.receipts
		SET state = $1, hold = $2, hold_expire = $3, card_system = $4,
		    card_id = $5, payer = $6, pay_time = $7, cancel_time = $8,
		    commission = $9, merchant_txn = nullif($10, ''), updated_at = now()
		WHERE id = $11`,
		rec.State, rec.Hold, rec.HoldExpire, nullableSystem(rec.CardSystem),
		rec.CardID, payer, rec.PayTime, rec.CancelTime, rec.Commission,
		rec.MerchantTxn, rec.ID)
	if err != nil {
		return fmt.Errorf("update receipt: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}

	return nil
}

// List returns receipts created within [from, to] inclusive, newest first,
// which is the order receipts.get_all reports.
func (r *ReceiptRepository) List(ctx context.Context, from, to int64, count, offset int) ([]*domain.Receipt, error) {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := postgres.From(ctx, r.pool).Query(ctx, `
		SELECT `+receiptColumns+`
		FROM mock.receipts
		WHERE sandbox_id = $1 AND create_time BETWEEN $2 AND $3
		ORDER BY create_time DESC, id DESC
		LIMIT $4 OFFSET $5`, sandboxID, from, to, count, offset)
	if err != nil {
		return nil, fmt.Errorf("select receipts: %w", err)
	}

	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*domain.Receipt, error) {
		return scanReceipt(row)
	})
	if err != nil {
		return nil, fmt.Errorf("read receipts: %w", err)
	}

	return out, nil
}

func scanReceipt(row scanner) (*domain.Receipt, error) {
	var (
		rec     domain.Receipt
		system  *string
		account []byte
		detail  []byte
		payer   []byte
	)

	err := row.Scan(
		&rec.ID, &rec.SandboxID, &rec.ReceiptID, &rec.MerchantID, &rec.Amount,
		&rec.Currency, &rec.Commission, &rec.State, &rec.Type, &rec.Payout,
		&rec.Hold, &rec.HoldExpire, &system, &account, &detail, &rec.Description,
		&rec.CardID, &payer, &rec.CreateTime, &rec.PayTime, &rec.CancelTime,
		&rec.MerchantTxn,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan receipt: %w", err)
	}

	if system != nil {
		rec.CardSystem = domain.CardSystem(*system)
	}
	if err := json.Unmarshal(account, &rec.Account); err != nil {
		return nil, fmt.Errorf("decode account: %w", err)
	}
	if len(detail) > 0 {
		if err := json.Unmarshal(detail, &rec.Detail); err != nil {
			return nil, fmt.Errorf("decode detail: %w", err)
		}
	}
	if len(payer) > 0 {
		if err := json.Unmarshal(payer, &rec.Payer); err != nil {
			return nil, fmt.Errorf("decode payer: %w", err)
		}
	}

	return &rec, nil
}

// marshalOptional encodes a free-form object, storing an absent one as NULL.
// Both detail and payer arrive from the caller, so encoding can fail on a
// value JSON cannot represent.
func marshalOptional(v map[string]any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// nullableSystem stores an undetermined card network as NULL, which the
// column's check constraint requires.
func nullableSystem(s domain.CardSystem) *string {
	if s == "" {
		return nil
	}
	v := string(s)
	return &v
}
