package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

const (
	now         = int64(1_399_114_284_039)
	amount      = int64(500000)
	testCardID  = int64(7)
	testReceipt = "2e0b1bc1f1eb50d487ba268d"
)

func newReceipt() *domain.Receipt {
	return domain.NewReceipt(1, testReceipt, "587f72c72cac0d162c722ae2", amount,
		map[string]string{"order_id": "197"}, now)
}

func TestNewReceiptDefaults(t *testing.T) {
	r := newReceipt()

	assert.Equal(t, domain.StateCreated, r.State)
	assert.Equal(t, domain.CurrencyUZS, r.Currency, "the provider reports UZS as 860")
	assert.Equal(t, amount, r.Amount)
	assert.Zero(t, r.PayTime)
	assert.Zero(t, r.CancelTime)
}

func TestReceiptStateValid(t *testing.T) {
	documented := []domain.ReceiptState{0, 1, 2, 3, 4, 5, 6, 20, 21, 30, 50}
	for _, s := range documented {
		assert.True(t, s.Valid(), "state %d is documented", s)
	}

	for _, s := range []domain.ReceiptState{7, 19, 22, 31, 49, 51, -1} {
		assert.False(t, s.Valid(), "state %d is not documented", s)
	}
}

func TestReceiptStateTerminal(t *testing.T) {
	assert.True(t, domain.StatePaid.Terminal())
	assert.True(t, domain.StateCancelled.Terminal())

	for _, s := range []domain.ReceiptState{
		domain.StateCreated, domain.StateChecking, domain.StateWithdrawn,
		domain.StateClosing, domain.StateHeld, domain.StateCancelQueued,
	} {
		assert.False(t, s.Terminal(), "state %d is not terminal", s)
	}
}

func TestReceiptStateInProgress(t *testing.T) {
	inProgress := []domain.ReceiptState{
		domain.StateChecking, domain.StateWithdrawn, domain.StateClosing,
		domain.StateCancelQueued, domain.StateCloseQueued,
	}
	for _, s := range inProgress {
		assert.True(t, s.InProgress(), "state %d is mid-flight", s)
	}

	for _, s := range []domain.ReceiptState{
		domain.StateCreated, domain.StatePaid, domain.StateCancelled,
		domain.StateHeld, domain.StatePaused, domain.StateHoldAccepted,
	} {
		assert.False(t, s.InProgress(), "state %d is not mid-flight", s)
	}
}

// A paying receipt must walk 0 → 1 → 2 → 3 → 4 one step at a time; jumping
// straight to paid is exactly what distinguishes a crude mock from the real
// provider.
func TestPaymentWalksEveryStateInOrder(t *testing.T) {
	r := newReceipt()
	require.NoError(t, r.BeginPay(testCardID, domain.CardUzcard, false, now))

	var walked []domain.ReceiptState
	walked = append(walked, r.State)

	for i := 0; i < 10 && r.Advance(now+int64(i)); i++ {
		walked = append(walked, r.State)
	}

	assert.Equal(t, []domain.ReceiptState{
		domain.StateChecking,
		domain.StateWithdrawn,
		domain.StateClosing,
		domain.StatePaid,
	}, walked)
}

func TestBeginPay(t *testing.T) {
	t.Run("records the card and moves off created", func(t *testing.T) {
		r := newReceipt()

		require.NoError(t, r.BeginPay(testCardID, domain.CardHumo, false, now+5))

		assert.Equal(t, domain.StateChecking, r.State)
		require.NotNil(t, r.CardID)
		assert.Equal(t, testCardID, *r.CardID)
		assert.Equal(t, domain.CardHumo, r.CardSystem)
		assert.Equal(t, now+5, r.PayTime)
	})

	t.Run("refuses a receipt that is already paying", func(t *testing.T) {
		r := newReceipt()
		require.NoError(t, r.BeginPay(testCardID, domain.CardUzcard, false, now))

		err := r.BeginPay(testCardID, domain.CardUzcard, false, now)

		assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	})

	t.Run("refuses a cancelled receipt", func(t *testing.T) {
		r := newReceipt()
		r.State = domain.StateCancelled

		assert.ErrorIs(t, r.BeginPay(testCardID, domain.CardUzcard, false, now), payerr.ErrCannotPerform)
	})
}

func TestAdvanceStopsAtPaid(t *testing.T) {
	r := newReceipt()
	require.NoError(t, r.BeginPay(testCardID, domain.CardUzcard, false, now))
	for r.Advance(now) { //nolint:revive // drain the walk
	}

	assert.Equal(t, domain.StatePaid, r.State)
	assert.False(t, r.Advance(now), "a paid receipt has nowhere left to go")
}

func TestNextPayState(t *testing.T) {
	tests := []struct {
		from     domain.ReceiptState
		wantNext domain.ReceiptState
		wantMore bool
	}{
		{domain.StateCreated, domain.StateChecking, true},
		{domain.StateChecking, domain.StateWithdrawn, true},
		{domain.StateWithdrawn, domain.StateClosing, true},
		{domain.StateClosing, domain.StatePaid, true},
		{domain.StatePaid, domain.StatePaid, false},
		{domain.StateCancelled, domain.StateCancelled, false},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			r := newReceipt()
			r.State = tt.from

			next, more := r.NextPayState()

			assert.Equal(t, tt.wantMore, more)
			assert.Equal(t, tt.wantNext, next)
		})
	}
}

// A held payment stops one step short of paid and waits for confirmation.
func TestHeldPaymentStopsBeforePaid(t *testing.T) {
	r := newReceipt()
	require.NoError(t, r.BeginPay(testCardID, domain.CardUzcard, true, now))

	for r.Advance(now) { //nolint:revive // drain the walk
	}

	assert.Equal(t, domain.StateHeld, r.State)
	assert.True(t, r.Hold)
}

func TestConfirmHold(t *testing.T) {
	t.Run("completes a held payment", func(t *testing.T) {
		r := newReceipt()
		require.NoError(t, r.BeginPay(testCardID, domain.CardUzcard, true, now))
		for r.Advance(now) { //nolint:revive // drain the walk
		}

		require.NoError(t, r.ConfirmHold(now+900))

		assert.Equal(t, domain.StatePaid, r.State)
		assert.Equal(t, now+900, r.PayTime)
		assert.False(t, r.Hold)
	})

	t.Run("refuses a receipt that is not held", func(t *testing.T) {
		r := newReceipt()

		assert.ErrorIs(t, r.ConfirmHold(now), payerr.ErrCannotPerform)
	})
}

// Cancelling a paid receipt queues it; only the background step completes it.
func TestCancelQueuesBeforeCompleting(t *testing.T) {
	r := newReceipt()
	require.NoError(t, r.BeginPay(testCardID, domain.CardUzcard, false, now))
	for r.Advance(now) { //nolint:revive // drain the walk
	}
	require.Equal(t, domain.StatePaid, r.State)

	require.NoError(t, r.Cancel(now+1000))
	assert.Equal(t, domain.StateCancelQueued, r.State, "cancellation is queued, not immediate")

	assert.True(t, r.Advance(now+2000))
	assert.Equal(t, domain.StateCancelled, r.State)
}

func TestCancel(t *testing.T) {
	tests := []struct {
		name      string
		state     domain.ReceiptState
		wantState domain.ReceiptState
		wantErr   error
	}{
		{"an unpaid receipt cancels outright", domain.StateCreated, domain.StateCancelled, nil},
		{"a paying receipt is queued", domain.StateChecking, domain.StateCancelQueued, nil},
		{"a withdrawn receipt is queued", domain.StateWithdrawn, domain.StateCancelQueued, nil},
		{"a paid receipt is queued", domain.StatePaid, domain.StateCancelQueued, nil},
		{"a held receipt is queued", domain.StateHeld, domain.StateCancelQueued, nil},
		{"an already queued receipt is unchanged", domain.StateCancelQueued, domain.StateCancelQueued, nil},
		{"an already cancelled receipt is unchanged", domain.StateCancelled, domain.StateCancelled, nil},
		{"a paused receipt cannot be cancelled", domain.StatePaused, domain.StatePaused, payerr.ErrCannotPerform},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newReceipt()
			r.State = tt.state

			err := r.Cancel(now)

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.wantState, r.State)
		})
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	r := newReceipt()
	r.State = domain.StatePaid
	require.NoError(t, r.Cancel(now))
	firstCancelTime := r.CancelTime

	require.NoError(t, r.Cancel(now+50_000))

	assert.Equal(t, firstCancelTime, r.CancelTime, "a repeated cancel must not move the time")
}

func TestHoldExpired(t *testing.T) {
	tests := []struct {
		name   string
		hold   bool
		expire int64
		at     int64
		want   bool
	}{
		{"a hold past its window", true, now + 1000, now + 1001, true},
		{"a hold inside its window", true, now + 1000, now + 999, false},
		{"a hold exactly at its window", true, now + 1000, now + 1000, false},
		{"no hold never expires", false, now + 1000, now + 99999, false},
		{"a hold without a window never expires", true, 0, now + 99999, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newReceipt()
			r.Hold, r.HoldExpire = tt.hold, tt.expire

			assert.Equal(t, tt.want, r.HoldExpired(tt.at))
		})
	}
}

func TestAdvanceOnAStaticStateReportsNoChange(t *testing.T) {
	r := newReceipt()
	r.State = domain.StatePaused

	assert.False(t, r.Advance(now))
	assert.Equal(t, domain.StatePaused, r.State)
}
