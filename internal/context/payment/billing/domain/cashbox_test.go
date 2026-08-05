package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

func TestKindValid(t *testing.T) {
	for _, kind := range domain.Kinds {
		assert.True(t, kind.Valid(), kind)
	}

	assert.False(t, domain.Kind("").Valid())
	assert.False(t, domain.Kind("withdrawal").Valid())
}

// Only a topup register brings money in. Everything else pays out, which is
// the case that can run short.
func TestKindInbound(t *testing.T) {
	assert.True(t, domain.KindTopup.Inbound())
	assert.False(t, domain.KindDividend.Inbound())
	assert.False(t, domain.KindDeposit.Inbound())
}

func TestKindDescribe(t *testing.T) {
	tests := []struct {
		kind domain.Kind
		want string
	}{
		{domain.KindTopup, "payments add to the balance"},
		{domain.KindDividend, "dividend payouts take from the balance"},
		{domain.KindDeposit, "deposit returns take from the balance"},
		{domain.Kind("nonsense"), "unknown register kind"},
	}

	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			assert.Equal(t, tt.want, tt.kind.Describe())
		})
	}
}

func TestApplyAddsOnATopupRegister(t *testing.T) {
	account := &domain.Account{Balance: 100}

	require.NoError(t, account.Apply(domain.KindTopup, 500))

	assert.Equal(t, int64(600), account.Balance)
}

func TestApplyTakesOnAPayoutRegister(t *testing.T) {
	for _, kind := range []domain.Kind{domain.KindDividend, domain.KindDeposit} {
		t.Run(string(kind), func(t *testing.T) {
			account := &domain.Account{Balance: 500}

			require.NoError(t, account.Apply(kind, 200))

			assert.Equal(t, int64(300), account.Balance)
		})
	}
}

// A payout with nothing behind it is refused rather than clamped: a register
// that owes more than it holds would report a balance no reconciliation could
// explain.
func TestApplyRefusesAPayoutLargerThanTheBalance(t *testing.T) {
	account := &domain.Account{Balance: 100}

	err := account.Apply(domain.KindDividend, 101)

	assert.ErrorIs(t, err, domain.ErrInsufficientFunds)
	assert.Equal(t, int64(100), account.Balance, "a refused payout leaves the balance alone")
}

// Paying out exactly what is there is allowed; only going past it is not.
func TestApplyAllowsAPayoutOfTheWholeBalance(t *testing.T) {
	account := &domain.Account{Balance: 100}

	require.NoError(t, account.Apply(domain.KindDeposit, 100))

	assert.Zero(t, account.Balance)
}

// The protocol has no code for an empty register, so the shortfall is reported
// as an operation that cannot be carried out, with the reason in the message.
func TestInsufficientFundsIsReportedAsCannotPerform(t *testing.T) {
	err := (&domain.Account{}).Apply(domain.KindDividend, 1)

	protocolErr, ok := payerr.As(err)
	require.True(t, ok)
	assert.Equal(t, payerr.CodeCannotPerform, protocolErr.Code)
	assert.True(t, protocolErr.Message.Complete(), "all three translations are required")
	assert.Contains(t, protocolErr.Message.EN, "Insufficient funds")
}

func TestApplyRejectsAnUnknownKind(t *testing.T) {
	account := &domain.Account{Balance: 100}

	err := account.Apply(domain.Kind("nonsense"), 10)

	assert.ErrorContains(t, err, "unknown register kind")
	assert.Equal(t, int64(100), account.Balance)
}

// Reverse puts back exactly what Apply moved, whichever way that was.
func TestReverseUndoesApply(t *testing.T) {
	for _, kind := range domain.Kinds {
		t.Run(string(kind), func(t *testing.T) {
			account := &domain.Account{Balance: 1000}

			require.NoError(t, account.Apply(kind, 250))
			account.Reverse(kind, 250)

			assert.Equal(t, int64(1000), account.Balance)
		})
	}
}

// A reversal is never refused: the money moved once already, so putting it
// back cannot leave the register anywhere it has not just been.
func TestReverseIsNeverRefused(t *testing.T) {
	account := &domain.Account{Balance: 0}

	account.Reverse(domain.KindTopup, 500)

	assert.Equal(t, int64(-500), account.Balance)
}
