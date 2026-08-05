package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/merchant/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// The confirmation window from the plan: 12 hours in milliseconds.
const timeoutMillis int64 = 43_200_000

const baseTime int64 = 1_399_114_284_039

func newCreated() *domain.Transaction {
	return domain.NewTransaction(
		1, "5305e3bab097f420a62ced0b", 197, 42,
		map[string]string{"phone": "901234567"},
		500000, baseTime, baseTime,
	)
}

func TestNewTransactionStartsCreated(t *testing.T) {
	tx := newCreated()

	assert.Equal(t, domain.StateCreated, tx.State)
	assert.Equal(t, baseTime, tx.CreateTime)
	assert.Zero(t, tx.PerformTime)
	assert.Zero(t, tx.CancelTime)
	assert.Nil(t, tx.Reason)
	assert.Equal(t, int64(500000), tx.Amount)
	assert.Equal(t, "5305e3bab097f420a62ced0b", tx.PaymeID)
}

func TestStateValid(t *testing.T) {
	tests := []struct {
		state domain.State
		want  bool
	}{
		{domain.StateCreated, true},
		{domain.StatePerformed, true},
		{domain.StateCancelled, true},
		{domain.StateCancelledAfterDo, true},
		{domain.State(0), false},
		{domain.State(3), false},
		{domain.State(-3), false},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.state.Valid(), "state %d", tt.state)
	}
}

func TestStateIsFinal(t *testing.T) {
	assert.False(t, domain.StateCreated.IsFinal())
	assert.False(t, domain.StatePerformed.IsFinal())
	assert.True(t, domain.StateCancelled.IsFinal())
	assert.True(t, domain.StateCancelledAfterDo.IsFinal())
}

func TestStateIsActive(t *testing.T) {
	assert.True(t, domain.StateCreated.IsActive())
	assert.True(t, domain.StatePerformed.IsActive())
	assert.False(t, domain.StateCancelled.IsActive())
	assert.False(t, domain.StateCancelledAfterDo.IsActive())
}

// Every (state, method) pair the protocol can present, allowed and rejected.
func TestStateMachineTransitionMatrix(t *testing.T) {
	tests := []struct {
		name      string
		from      domain.State
		method    string
		wantState domain.State
		wantErr   error
	}{
		{"perform a created transaction", domain.StateCreated, "perform", domain.StatePerformed, nil},
		{"perform a performed transaction is an idempotent replay", domain.StatePerformed, "perform", domain.StatePerformed, nil},
		{"perform a cancelled transaction is refused", domain.StateCancelled, "perform", domain.StateCancelled, payerr.ErrCannotPerform},
		{"perform a refunded transaction is refused", domain.StateCancelledAfterDo, "perform", domain.StateCancelledAfterDo, payerr.ErrCannotPerform},

		{"cancel a created transaction yields -1", domain.StateCreated, "cancel", domain.StateCancelled, nil},
		{"cancel a performed transaction yields -2", domain.StatePerformed, "cancel", domain.StateCancelledAfterDo, nil},
		{"cancel a cancelled transaction is an idempotent replay", domain.StateCancelled, "cancel", domain.StateCancelled, nil},
		{"cancel a refunded transaction is an idempotent replay", domain.StateCancelledAfterDo, "cancel", domain.StateCancelledAfterDo, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := newCreated()
			tx.State = tt.from

			var err error
			switch tt.method {
			case "perform":
				err = tx.Perform(baseTime+1000, timeoutMillis)
			case "cancel":
				err = tx.Cancel(1, baseTime+1000)
			}

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.wantState, tx.State)
		})
	}
}

func TestPerformRecordsTime(t *testing.T) {
	tx := newCreated()
	const performedAt = baseTime + 963

	require.NoError(t, tx.Perform(performedAt, timeoutMillis))

	assert.Equal(t, performedAt, tx.PerformTime)
}

func TestPerformReplayKeepsOriginalTime(t *testing.T) {
	tx := newCreated()
	const firstCall = baseTime + 963
	require.NoError(t, tx.Perform(firstCall, timeoutMillis))

	require.NoError(t, tx.Perform(firstCall+50_000, timeoutMillis))

	assert.Equal(t, firstCall, tx.PerformTime, "a replay must not move the perform time")
}

func TestCancelRecordsReasonAndTime(t *testing.T) {
	tx := newCreated()
	const cancelledAt = baseTime + 5000

	require.NoError(t, tx.Cancel(3, cancelledAt))

	assert.Equal(t, cancelledAt, tx.CancelTime)
	require.NotNil(t, tx.Reason)
	assert.Equal(t, domain.Reason(3), *tx.Reason)
}

func TestCancelReplayKeepsOriginalReasonAndTime(t *testing.T) {
	tx := newCreated()
	const firstCall = baseTime + 5000
	require.NoError(t, tx.Cancel(3, firstCall))

	require.NoError(t, tx.Cancel(5, firstCall+9000))

	assert.Equal(t, firstCall, tx.CancelTime)
	assert.Equal(t, domain.Reason(3), *tx.Reason, "a replay must not rewrite the reason")
}

func TestExpired(t *testing.T) {
	tests := []struct {
		name  string
		state domain.State
		now   int64
		want  bool
	}{
		{"inside the window", domain.StateCreated, baseTime + timeoutMillis - 1, false},
		{"exactly at the window", domain.StateCreated, baseTime + timeoutMillis, false},
		{"one millisecond past the window", domain.StateCreated, baseTime + timeoutMillis + 1, true},
		{"performed never expires", domain.StatePerformed, baseTime + timeoutMillis*10, false},
		{"cancelled never expires", domain.StateCancelled, baseTime + timeoutMillis*10, false},
		{"refunded never expires", domain.StateCancelledAfterDo, baseTime + timeoutMillis*10, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := newCreated()
			tx.State = tt.state

			assert.Equal(t, tt.want, tx.Expired(tt.now, timeoutMillis))
		})
	}
}

// An expired transaction must be refused and auto-cancelled in the same step,
// so a later CheckTransaction reports it as cancelled by timeout.
func TestPerformOnExpiredTransactionRefusesAndAutoCancels(t *testing.T) {
	tx := newCreated()
	const late = baseTime + timeoutMillis + 1

	err := tx.Perform(late, timeoutMillis)

	assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	assert.Equal(t, domain.StateCancelled, tx.State)
	assert.Equal(t, late, tx.CancelTime)
	require.NotNil(t, tx.Reason)
	assert.Equal(t, domain.ReasonTimeout, *tx.Reason)
	assert.Zero(t, tx.PerformTime, "an expired transaction was never performed")
}

func TestPerformOnUnknownStateIsRefused(t *testing.T) {
	tx := newCreated()
	tx.State = domain.State(99)

	err := tx.Perform(baseTime+1, timeoutMillis)

	assert.ErrorIs(t, err, payerr.ErrCannotPerform)
}

func TestCancelOnUnknownStateIsRefused(t *testing.T) {
	tx := newCreated()
	tx.State = domain.State(99)

	err := tx.Cancel(1, baseTime+1)

	assert.ErrorIs(t, err, payerr.ErrCannotPerform)
}
