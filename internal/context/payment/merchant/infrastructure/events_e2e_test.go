package infrastructure_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/merchant/domain"
	"github.com/bakhod1r/payme-mock/internal/context/payment/merchant/infrastructure"
)

func TestE2EEventRecorderAppendsAStep(t *testing.T) {
	s := newStand(t)
	tx := s.newTransaction(paymeID)
	require.NoError(t, s.repo.Create(s.ctx, tx))

	from := domain.StateCreated
	to := domain.StatePerformed

	err := infrastructure.NewEventRecorder(s.pool).Record(s.ctx, domain.Event{
		TransactionID: tx.ID,
		Method:        "PerformTransaction",
		FromState:     &from,
		ToState:       &to,
	})

	require.NoError(t, err)

	var (
		method    string
		fromState *int16
		toState   *int16
		hit       bool
		errorCode *int
	)
	require.NoError(t, s.pool.QueryRow(context.Background(), `
		SELECT method, from_state, to_state, idempotent_hit, error_code
		FROM merchant.transaction_events
		WHERE transaction_id = $1`, tx.ID).
		Scan(&method, &fromState, &toState, &hit, &errorCode))

	assert.Equal(t, "PerformTransaction", method)
	assert.Equal(t, int16(domain.StateCreated), *fromState)
	assert.Equal(t, int16(domain.StatePerformed), *toState)
	assert.False(t, hit)
	assert.Nil(t, errorCode)
}

// A rejected call has no state change to record, so both states stay NULL and
// the row exists only to show the attempt happened.
func TestE2EEventRecorderStoresAFailureWithoutStates(t *testing.T) {
	s := newStand(t)
	tx := s.newTransaction(paymeID)
	require.NoError(t, s.repo.Create(s.ctx, tx))

	code := -31008

	err := infrastructure.NewEventRecorder(s.pool).Record(s.ctx, domain.Event{
		TransactionID: tx.ID,
		Method:        "PerformTransaction",
		IdempotentHit: true,
		ErrorCode:     &code,
	})

	require.NoError(t, err)

	var (
		fromState *int16
		toState   *int16
		hit       bool
		errorCode *int
	)
	require.NoError(t, s.pool.QueryRow(context.Background(), `
		SELECT from_state, to_state, idempotent_hit, error_code
		FROM merchant.transaction_events
		WHERE transaction_id = $1`, tx.ID).
		Scan(&fromState, &toState, &hit, &errorCode))

	assert.Nil(t, fromState)
	assert.Nil(t, toState)
	assert.True(t, hit)
	assert.Equal(t, code, *errorCode)
}

// The audit trail may not point at a transaction that does not exist, so the
// foreign key rejects the row rather than leaving an orphan behind.
func TestE2EEventRecorderRejectsAnUnknownTransaction(t *testing.T) {
	s := newStand(t)

	err := infrastructure.NewEventRecorder(s.pool).Record(s.ctx, domain.Event{
		TransactionID: 999999,
		Method:        "PerformTransaction",
	})

	assert.ErrorContains(t, err, "insert transaction event")
}
