package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

func TestOrderCheckAmount(t *testing.T) {
	o := &domain.Order{Amount: 500000}

	t.Run("exact match is accepted", func(t *testing.T) {
		assert.NoError(t, o.CheckAmount(500000))
	})

	t.Run("too little is rejected", func(t *testing.T) {
		assert.ErrorIs(t, o.CheckAmount(499999), payerr.ErrInvalidAmount)
	})

	t.Run("too much is rejected", func(t *testing.T) {
		assert.ErrorIs(t, o.CheckAmount(500001), payerr.ErrInvalidAmount)
	})

	t.Run("zero is rejected", func(t *testing.T) {
		assert.ErrorIs(t, o.CheckAmount(0), payerr.ErrInvalidAmount)
	})
}

func TestOrderPayable(t *testing.T) {
	tests := []struct {
		status domain.OrderStatus
		want   bool
	}{
		{domain.StatusNew, true},
		{domain.StatusProcessing, true},
		{domain.StatusPaid, false},
		{domain.StatusCancelled, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			o := &domain.Order{Status: tt.status}

			assert.Equal(t, tt.want, o.Payable())
		})
	}
}

func TestOrderStatusTransitions(t *testing.T) {
	t.Run("mark processing", func(t *testing.T) {
		o := &domain.Order{Status: domain.StatusNew}

		o.MarkProcessing()

		assert.Equal(t, domain.StatusProcessing, o.Status)
	})

	t.Run("mark paid", func(t *testing.T) {
		o := &domain.Order{Status: domain.StatusProcessing}

		o.MarkPaid()

		assert.Equal(t, domain.StatusPaid, o.Status)
		assert.False(t, o.Payable())
	})

	t.Run("mark cancelled", func(t *testing.T) {
		o := &domain.Order{Status: domain.StatusProcessing}

		o.MarkCancelled()

		assert.Equal(t, domain.StatusCancelled, o.Status)
		assert.False(t, o.Payable())
	})
}
