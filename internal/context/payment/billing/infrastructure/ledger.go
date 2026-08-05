package infrastructure

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
	merchant "github.com/bakhod1r/payme-mock/internal/context/payment/merchant/domain"
	subscribe "github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// CashboxLedger records money the provider handed out rather than took in.
//
// A payment charged to a card travels the Merchant API — check, create,
// perform — and the merchant side writes the transaction and applies the
// balance at the end of that chain. A payout has no such chain: the provider is
// not asking a merchant for anything, it is paying a card the merchant already
// decided to pay. Nothing would then write either half, so a dividend register
// paid out all day showed the figure it started with and an empty ledger.
type CashboxLedger struct {
	pool *postgres.Pool
}

// NewCashboxLedger wires the ledger to a pool.
func NewCashboxLedger(pool *postgres.Pool) *CashboxLedger {
	return &CashboxLedger{pool: pool}
}

// The states a payout's transaction is written in. They are the merchant
// protocol's own, because the row is read on the same screen as a payment's and
// a private numbering would make that screen lie.
const (
	payoutCreated   = 1
	payoutPerformed = 2
)

// accountJSON is the account object as the column takes it.
//
// A payout with no account is stored as an empty object rather than as NULL:
// the column is what every payment on the stand is read through, and a row
// whose account is missing reads as a payment made for nobody.
func accountJSON(account map[string]string) map[string]string {
	if account == nil {
		return map[string]string{}
	}

	return account
}

// OpenPayout writes the payout the register has asked for, before any money
// moves.
//
// A repeat of the same payout is left as it stands rather than written twice:
// the caller cannot tell a lost response from a lost payout, so it retries.
func (l *CashboxLedger) OpenPayout(ctx context.Context, payout subscribe.Payout) error {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return err
	}

	return postgres.WithTx(ctx, l.pool, func(inner context.Context) error {
		payer, _, err := l.lockPayer(inner, sandboxID)
		if err != nil {
			return err
		}

		// A stopped register pays nobody out. Refusing at the settlement alone
		// would leave the payout opened and the receipt walking towards a move
		// that cannot happen.
		if err := payer.Usable(); err != nil {
			return err
		}

		// A payout settles no order: nothing was bought. The column is nullable
		// for exactly this, and the one-active-transaction-per-order index
		// excludes null orders, so payouts never contend with each other.
		if _, err := postgres.From(inner, l.pool).Exec(inner, `
			INSERT INTO merchant.transactions
				(sandbox_id, payme_id, order_id, account_id, account, amount,
				 state, payme_time, create_time)
			VALUES ($1, $2, NULL, $3, $4::jsonb, $5, $6, $7, $7)
			ON CONFLICT (sandbox_id, payme_id) DO NOTHING`,
			sandboxID, payout.TransactionID, payer.ID, accountJSON(payout.Account),
			payout.Amount, payoutCreated, payout.CreateTime); err != nil {
			return fmt.Errorf("record payout: %w", err)
		}

		return nil
	})
}

// SettlePayout takes a completed payout out of the register's balance and marks
// its transaction performed.
//
// The whole move is one transaction with the balance row locked, so two payouts
// settling at once cannot both read the same balance and lose one of the two
// moves. A register without the funds is refused rather than driven negative:
// that refusal is the failure an integration most needs to rehearse, and the
// domain already words it for the payer's side.
func (l *CashboxLedger) SettlePayout(ctx context.Context, payout subscribe.Payout) error {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return err
	}

	return postgres.WithTx(ctx, l.pool, func(inner context.Context) error {
		payer, kind, err := l.lockPayer(inner, sandboxID)
		if err != nil {
			return err
		}

		before := payer.Balance

		if err := payer.Apply(domain.Kind(kind), payout.Amount); err != nil {
			return err
		}

		if _, err := postgres.From(inner, l.pool).Exec(inner, `
			UPDATE merchant.accounts SET balance = $1 WHERE id = $2`,
			payer.Balance, payer.ID); err != nil {
			return fmt.Errorf("move register balance: %w", err)
		}

		// The transaction is written here as well as on opening, because a
		// payout may have been opened by a stand running before this ledger
		// existed; without the insert its settlement would move a balance with
		// no payment to point at.
		var transactionID int64
		if err := postgres.From(inner, l.pool).QueryRow(inner, `
			INSERT INTO merchant.transactions
				(sandbox_id, payme_id, order_id, account_id, account, amount,
				 state, payme_time, create_time, perform_time)
			VALUES ($1, $2, NULL, $3, $4::jsonb, $5, $6, $7, $7, $8)
			ON CONFLICT (sandbox_id, payme_id) DO UPDATE
			SET state = $6, perform_time = $8, updated_at = now()
			RETURNING id`,
			sandboxID, payout.TransactionID, payer.ID, accountJSON(payout.Account),
			payout.Amount, payoutPerformed, payout.CreateTime,
			payout.PayTime).Scan(&transactionID); err != nil {
			return fmt.Errorf("perform payout: %w", err)
		}

		// The history row carries what the move was for, and which payment it
		// belongs to. A balance nobody can explain is worse than no history at
		// all, and "a payout settled" is what distinguishes this move from a
		// payment's.
		if _, err := postgres.From(inner, l.pool).Exec(inner, `
			INSERT INTO merchant.balance_events
				(sandbox_id, account_id, transaction_id, source, delta,
				 balance_before, balance_after, note)
			VALUES ($1, $2, $3, 'payment', $4, $5, $6, 'settled by a payout')`,
			sandboxID, payer.ID, transactionID, payer.Balance-before, before,
			payer.Balance); err != nil {
			return fmt.Errorf("record register balance move: %w", err)
		}

		return nil
	})
}

// Balance reports what the register holds now.
//
// Nothing is locked: the figure is read to be watched, not to be moved against,
// and a read that blocked a settling payout would be a monitor getting in the
// way of the money it monitors.
func (l *CashboxLedger) Balance(ctx context.Context) (subscribe.Cashbox, error) {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return subscribe.Cashbox{}, err
	}

	var out subscribe.Cashbox

	err = l.pool.QueryRow(ctx, `
		SELECT s.kind, a.balance, a.blocked
		FROM control.sandboxes s
		JOIN merchant.accounts a ON a.sandbox_id = s.id
		WHERE s.id = $1
		ORDER BY a.id
		LIMIT 1`, sandboxID).Scan(&out.Kind, &out.Balance, &out.Blocked)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return subscribe.Cashbox{}, merchant.ErrNotFound
		}
		return subscribe.Cashbox{}, fmt.Errorf("read register balance: %w", err)
	}

	out.Currency = subscribe.CurrencyUZS

	return out, nil
}

// lockPayer returns the register's payer with its row locked, and the kind of
// register it belongs to.
//
// The register's kind decides the direction of a move, and its payer is the row
// that holds the money. A stand is created with exactly one payer; if an
// operator added more, the first is the one the console's balance column speaks
// for, so it is the one used here too.
func (l *CashboxLedger) lockPayer(ctx context.Context, sandboxID int64) (*domain.Account, string, error) {
	var (
		kind    string
		account domain.Account
	)

	row := postgres.From(ctx, l.pool).QueryRow(ctx, `
		SELECT s.kind, a.id, a.sandbox_id, coalesce(a.phone, ''),
		       coalesce(a.login, ''), a.name, a.balance, a.blocked
		FROM control.sandboxes s
		JOIN merchant.accounts a ON a.sandbox_id = s.id
		WHERE s.id = $1
		ORDER BY a.id
		LIMIT 1
		FOR UPDATE OF a`, sandboxID)

	if err := row.Scan(&kind, &account.ID, &account.SandboxID, &account.Phone,
		&account.Login, &account.Name, &account.Balance, &account.Blocked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", merchant.ErrNotFound
		}
		return nil, "", fmt.Errorf("lock register balance: %w", err)
	}

	return &account, kind, nil
}
