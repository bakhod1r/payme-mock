package infrastructure_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/billing/infrastructure"
	merchant "github.com/bakhod1r/payme-mock/internal/context/payment/merchant/domain"
	subscribe "github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// payoutTime is when a rehearsed payout was asked for; settling it a moment
// later is what a real one does.
const (
	payoutTime = int64(1750000000000)
	settleTime = int64(1750000000500)
)

// payout is one payout as the ledger is told about it.
func payout(id string, amount int64) subscribe.Payout {
	return subscribe.Payout{
		TransactionID: id,
		Amount:        amount,
		Account:       map[string]string{"login": seedPhone},
		CreateTime:    payoutTime,
		PayTime:       settleTime,
	}
}

// transactionState reads back what the register's books hold for one payout.
func transactionState(t *testing.T, s *stand, id string) (state int, performTime, amount int64) {
	t.Helper()

	err := s.pool.QueryRow(context.Background(), `
		SELECT state, perform_time, amount FROM merchant.transactions
		WHERE sandbox_id = $1 AND payme_id = $2`, s.sandboxID, id).
		Scan(&state, &performTime, &amount)
	require.NoError(t, err)

	return state, performTime, amount
}

// payoutRegister makes the stand one that pays money out, which is the only
// kind a payout belongs on: the register's kind is what decides the direction of
// every move, and a top-up register takes money in.
// It is also given money to pay out with: the seeded payer starts at nothing,
// and a register with no funds refuses every payout, which is a different case
// from the ones these cover.
func payoutRegister(t *testing.T, s *stand) {
	t.Helper()

	_, err := s.pool.Exec(context.Background(),
		`UPDATE control.sandboxes SET kind = 'dividend' WHERE id = $1`, s.sandboxID)
	require.NoError(t, err)

	_, err = s.pool.Exec(context.Background(),
		`UPDATE merchant.accounts SET balance = $1 WHERE id = $2`, registerFunds, s.accountID)
	require.NoError(t, err)
}

// registerFunds is what a payout register is given to work with.
const registerFunds = int64(1000000)

// balance is what the register holds now.
func balance(t *testing.T, s *stand) int64 {
	t.Helper()

	var out int64
	require.NoError(t, s.pool.QueryRow(context.Background(),
		`SELECT balance FROM merchant.accounts WHERE id = $1`, s.accountID).Scan(&out))

	return out
}

// A payout asks nothing of a merchant, so no Merchant API chain writes it. The
// register's books would then show a payout register that never paid anything.
func TestE2EOpenPayoutWritesTheRegistersOwnTransaction(t *testing.T) {
	s := newStand(t)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	require.NoError(t, ledger.OpenPayout(s.ctx, payout("payout-1", 120000)))

	state, performTime, written := transactionState(t, s, "payout-1")
	assert.Equal(t, 1, state, "an opened payout is created, not performed")
	assert.Zero(t, performTime)
	assert.Equal(t, int64(120000), written)
}

// Nothing has been paid yet, so nothing has left the register.
func TestE2EOpenPayoutMovesNoMoney(t *testing.T) {
	s := newStand(t)
	ledger := infrastructure.NewCashboxLedger(s.pool)
	before := balance(t, s)

	require.NoError(t, ledger.OpenPayout(s.ctx, payout("payout-open", 120000)))

	assert.Equal(t, before, balance(t, s))
}

// The caller cannot tell a lost response from a lost payout, so it retries; a
// retry must not leave two payments where one was made.
func TestE2EOpeningTheSamePayoutTwiceWritesOneRow(t *testing.T) {
	s := newStand(t)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	require.NoError(t, ledger.OpenPayout(s.ctx, payout("payout-twice", 120000)))
	require.NoError(t, ledger.OpenPayout(s.ctx, payout("payout-twice", 120000)))

	var rows int
	require.NoError(t, s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM merchant.transactions WHERE payme_id = $1`,
		"payout-twice").Scan(&rows))
	assert.Equal(t, 1, rows)
}

func TestE2ESettlePayoutPerformsTheTransactionAndMovesTheBalance(t *testing.T) {
	s := newStand(t)
	payoutRegister(t, s)
	ledger := infrastructure.NewCashboxLedger(s.pool)
	before := balance(t, s)

	require.NoError(t, ledger.OpenPayout(s.ctx, payout("payout-settled", 120000)))
	require.NoError(t, ledger.SettlePayout(s.ctx, payout("payout-settled", 120000)))

	state, performTime, _ := transactionState(t, s, "payout-settled")
	assert.Equal(t, 2, state)
	assert.Equal(t, settleTime, performTime)
	assert.Equal(t, before-120000, balance(t, s), "a payout leaves the register")
}

// A payout opened by a stand running before the ledger existed still settles,
// and its settlement must not move a balance with no payment to point at.
func TestE2ESettlePayoutWritesATransactionItNeverOpened(t *testing.T) {
	s := newStand(t)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	require.NoError(t, ledger.SettlePayout(s.ctx, payout("payout-unopened", 90000)))

	state, performTime, _ := transactionState(t, s, "payout-unopened")
	assert.Equal(t, 2, state)
	assert.Equal(t, settleTime, performTime)
}

// The balance move and the payment it belongs to are one act, so the history
// row names the transaction rather than leaving a figure nobody can explain.
func TestE2ESettlePayoutRecordsWhyTheBalanceMoved(t *testing.T) {
	s := newStand(t)
	payoutRegister(t, s)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	require.NoError(t, ledger.SettlePayout(s.ctx, payout("payout-history", 90000)))

	var (
		note  string
		delta int64
		txn   *int64
	)
	require.NoError(t, s.pool.QueryRow(context.Background(), `
		SELECT note, delta, transaction_id FROM merchant.balance_events
		WHERE account_id = $1 ORDER BY id DESC LIMIT 1`, s.accountID).
		Scan(&note, &delta, &txn))

	assert.Equal(t, "settled by a payout", note)
	assert.Equal(t, int64(-90000), delta)
	require.NotNil(t, txn, "the move names the payment it belongs to")
}

// A register without the funds is refused rather than driven negative: that
// refusal is the failure an integration most needs to rehearse.
func TestE2ESettlePayoutRefusesMoreThanTheRegisterHolds(t *testing.T) {
	s := newStand(t)
	payoutRegister(t, s)
	ledger := infrastructure.NewCashboxLedger(s.pool)
	before := balance(t, s)

	err := ledger.SettlePayout(s.ctx, payout("payout-too-big", before+1))

	require.Error(t, err)
	assert.Equal(t, before, balance(t, s), "a refused payout leaves the balance alone")
}

func TestE2EPayoutLedgerNeedsASandbox(t *testing.T) {
	s := newStand(t)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	assert.Error(t, ledger.OpenPayout(context.Background(), payout("payout-nowhere", 1000)))
	assert.Error(t, ledger.SettlePayout(context.Background(), payout("payout-nowhere", 1000)))
}

// A stand whose payer has been deleted has nowhere to hold the money, which is
// a missing row rather than a refused payout.
func TestE2EPayoutLedgerReportsAStandWithNoPayer(t *testing.T) {
	s := newStand(t)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	_, err := s.pool.Exec(context.Background(),
		`DELETE FROM merchant.accounts WHERE sandbox_id = $1`, s.sandboxID)
	require.NoError(t, err)

	empty := sandboxctx.With(context.Background(), sandboxctx.Sandbox{ID: s.sandboxID})

	assert.ErrorIs(t, ledger.OpenPayout(empty, payout("payout-orphan", 1000)), merchant.ErrNotFound)
	assert.ErrorIs(t, ledger.SettlePayout(empty, payout("payout-orphan", 1000)), merchant.ErrNotFound)
}

// A payout is the register's own money leaving, so every one of these refusals
// has to be a refusal: answered as success, the stand would report a payout the
// books never took.
func TestE2EPayoutNeedsAStand(t *testing.T) {
	s := newStand(t)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	assert.Error(t, ledger.OpenPayout(context.Background(), payout("payout-unscoped", 1000)))
	assert.Error(t, ledger.SettlePayout(context.Background(), payout("payout-unscoped", 1000)))
}

// A payout naming nobody is still a payout: it is stored with an empty account
// rather than with none, because the column is what every payment on the stand
// is read through and a null there reads as a payment made for nobody.
func TestE2EPayoutWithNoAccountIsStoredAsAnEmptyOne(t *testing.T) {
	s := newStand(t)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	anonymous := payout("payout-anonymous", 1000)
	anonymous.Account = nil

	require.NoError(t, ledger.OpenPayout(s.ctx, anonymous))

	var stored string
	require.NoError(t, s.pool.QueryRow(s.ctx,
		`SELECT account::text FROM merchant.transactions WHERE payme_id = $1`,
		"payout-anonymous").Scan(&stored))
	assert.JSONEq(t, `{}`, stored)
}

// A stopped register pays nobody out, and it says so at the opening rather than
// at the settlement: refusing later would leave the payout open and the receipt
// walking towards a move that cannot happen.
func TestE2EPayoutIsRefusedByAStoppedRegister(t *testing.T) {
	s := newStand(t)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	_, err := s.pool.Exec(s.ctx, `UPDATE merchant.accounts SET blocked = TRUE WHERE id = $1`, s.accountID)
	require.NoError(t, err)

	assert.Error(t, ledger.OpenPayout(s.ctx, payout("payout-stopped", 1000)))
	assert.Error(t, ledger.SettlePayout(s.ctx, payout("payout-stopped", 1000)))

	var rows int
	require.NoError(t, s.pool.QueryRow(s.ctx,
		`SELECT count(*) FROM merchant.transactions WHERE payme_id = 'payout-stopped'`).Scan(&rows))
	assert.Zero(t, rows, "a refused payout leaves no payment behind")
}

// A stand with no payer of its own has nothing to pay out of, which is reported
// as the missing thing it is rather than as a payout of zero.
func TestE2EPayoutNeedsAPayer(t *testing.T) {
	s := newStand(t)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	_, err := s.pool.Exec(s.ctx, `DELETE FROM merchant.transactions WHERE sandbox_id = $1`, s.sandboxID)
	require.NoError(t, err)
	_, err = s.pool.Exec(s.ctx, `DELETE FROM merchant.orders WHERE sandbox_id = $1`, s.sandboxID)
	require.NoError(t, err)
	_, err = s.pool.Exec(s.ctx, `DELETE FROM merchant.accounts WHERE sandbox_id = $1`, s.sandboxID)
	require.NoError(t, err)

	assert.ErrorIs(t, ledger.OpenPayout(s.ctx, payout("payout-payerless", 1000)), merchant.ErrNotFound)
}

// A database that went away while the payout was being written is reported.
func TestE2EPayoutReportsALostDatabase(t *testing.T) {
	s := newStand(t)
	ledger := infrastructure.NewCashboxLedger(s.pool)
	s.pool.Close()

	assert.Error(t, ledger.OpenPayout(s.ctx, payout("payout-lost", 1000)))
	assert.Error(t, ledger.SettlePayout(s.ctx, payout("payout-lost", 1000)))
}

// The register's own payer is the row its balance column speaks for, and it is
// locked as it is read so two payments settling at once serialise rather than
// both reading the same figure.
func TestE2EAccountRegisterIsTheStandsOwnPayer(t *testing.T) {
	s := newStand(t)

	acc, err := s.accounts.Register(s.ctx)

	require.NoError(t, err)
	assert.Equal(t, s.accountID, acc.ID)
	assert.Equal(t, s.sandboxID, acc.SandboxID)
	assert.Zero(t, acc.Balance, "a stand starts with an empty register")
}

func TestE2EAccountRegisterNeedsAStand(t *testing.T) {
	s := newStand(t)

	_, err := s.accounts.Register(context.Background())

	assert.Error(t, err)
}

// A stand whose payer is gone is reported as missing rather than answered with
// a zero balance nothing could be settled against.
func TestE2EAccountRegisterReportsAStandWithNoPayer(t *testing.T) {
	s := newStand(t)

	_, err := s.pool.Exec(s.ctx, `DELETE FROM merchant.transactions WHERE sandbox_id = $1`, s.sandboxID)
	require.NoError(t, err)
	_, err = s.pool.Exec(s.ctx, `DELETE FROM merchant.orders WHERE sandbox_id = $1`, s.sandboxID)
	require.NoError(t, err)
	_, err = s.pool.Exec(s.ctx, `DELETE FROM merchant.accounts WHERE sandbox_id = $1`, s.sandboxID)
	require.NoError(t, err)

	_, err = s.accounts.Register(s.ctx)

	assert.ErrorIs(t, err, merchant.ErrNotFound)
}

// Every write inside a payout is part of the same act, so a failure in any of
// them is the payout failing: half a payout — money moved with no payment, or a
// payment with no history — is worse than none.
func TestE2EPayoutIsAllOrNothing(t *testing.T) {
	t.Run("the payment cannot be written", func(t *testing.T) {
		s := newStand(t)
		ledger := infrastructure.NewCashboxLedger(s.pool)

		_, err := s.pool.Exec(s.ctx, `DROP TABLE merchant.transactions CASCADE`)
		require.NoError(t, err)

		assert.ErrorContains(t, ledger.OpenPayout(s.ctx, payout("payout-a", 1000)), "record payout")
		assert.ErrorContains(t, ledger.SettlePayout(s.ctx, payout("payout-a", 1000)), "perform payout")
	})

	t.Run("the balance cannot be moved", func(t *testing.T) {
		s := newStand(t)
		ledger := infrastructure.NewCashboxLedger(s.pool)

		_, err := s.pool.Exec(s.ctx, `
			CREATE FUNCTION refuse() RETURNS TRIGGER LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'the balance is not writable'; END $$;
			CREATE TRIGGER refuse_balance BEFORE UPDATE ON merchant.accounts
			FOR EACH ROW EXECUTE FUNCTION refuse();`)
		require.NoError(t, err)

		assert.ErrorContains(t, ledger.SettlePayout(s.ctx, payout("payout-b", 1000)),
			"move register balance")
	})

	t.Run("the history cannot be written", func(t *testing.T) {
		s := newStand(t)
		ledger := infrastructure.NewCashboxLedger(s.pool)

		_, err := s.pool.Exec(s.ctx, `DROP TABLE merchant.balance_events`)
		require.NoError(t, err)

		assert.ErrorContains(t, ledger.SettlePayout(s.ctx, payout("payout-c", 1000)),
			"record register balance move")
	})
}

// A payer row that cannot be read is the stand's failure and is reported as
// one. Read as "no such payer" it would look like a stand nobody set up, which
// is the one thing an operator would not go looking at the database for.
func TestE2EPayoutReportsAnUnreadablePayer(t *testing.T) {
	s := newStand(t)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	_, err := s.pool.Exec(s.ctx, `ALTER TABLE merchant.accounts ALTER COLUMN balance TYPE text`)
	require.NoError(t, err)
	_, err = s.pool.Exec(s.ctx, `UPDATE merchant.accounts SET balance = 'not a number'`)
	require.NoError(t, err)

	err = ledger.OpenPayout(s.ctx, payout("payout-unreadable", 1000))

	assert.ErrorContains(t, err, "lock register balance")
}

// The figure is what an integration watches for an empty register, so it is
// read with the direction that makes it readable: on a payout register a
// payment lowers it, on a top-up register it raises it.
func TestE2EBalanceReportsTheRegister(t *testing.T) {
	s := newStand(t)
	payoutRegister(t, s)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	cashbox, err := ledger.Balance(s.ctx)

	require.NoError(t, err)
	assert.Equal(t, registerFunds, cashbox.Balance)
	assert.Equal(t, subscribe.CurrencyUZS, cashbox.Currency)
	assert.Equal(t, "dividend", cashbox.Kind)
	assert.False(t, cashbox.Blocked)
}

// A settled payout is money gone, and the watched figure has to say so.
func TestE2EBalanceFallsAsPayoutsSettle(t *testing.T) {
	s := newStand(t)
	payoutRegister(t, s)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	require.NoError(t, ledger.SettlePayout(s.ctx, payout("payout-watched", 250000)))

	cashbox, err := ledger.Balance(s.ctx)

	require.NoError(t, err)
	assert.Equal(t, registerFunds-250000, cashbox.Balance)
}

// A stopped register holds money and will still pay nobody, so the figure alone
// would say everything is fine.
func TestE2EBalanceReportsAStoppedRegister(t *testing.T) {
	s := newStand(t)
	payoutRegister(t, s)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	_, err := s.pool.Exec(context.Background(),
		`UPDATE merchant.accounts SET blocked = TRUE WHERE id = $1`, s.accountID)
	require.NoError(t, err)

	cashbox, err := ledger.Balance(s.ctx)

	require.NoError(t, err)
	assert.True(t, cashbox.Blocked)
}

func TestE2EBalanceNeedsASandbox(t *testing.T) {
	s := newStand(t)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	_, err := ledger.Balance(context.Background())

	assert.Error(t, err)
}

func TestE2EBalanceReportsAStandWithNoPayer(t *testing.T) {
	s := newStand(t)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	_, err := s.pool.Exec(context.Background(),
		`DELETE FROM merchant.accounts WHERE sandbox_id = $1`, s.sandboxID)
	require.NoError(t, err)

	_, err = ledger.Balance(s.ctx)

	assert.ErrorIs(t, err, merchant.ErrNotFound)
}

// A figure that could not be read is reported rather than answered as zero: a
// register showing nothing and a register holding nothing are the difference
// between "stop paying out" and "carry on".
func TestE2EBalanceReportsALostDatabase(t *testing.T) {
	s := newStand(t)
	ledger := infrastructure.NewCashboxLedger(s.pool)

	s.pool.Close()

	_, err := ledger.Balance(s.ctx)

	assert.ErrorContains(t, err, "read register balance")
}
