package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/checkout/domain"
)

func validItem() domain.Item {
	return domain.Item{
		Title:       "Coffee",
		Price:       25000,
		Count:       2,
		Code:        "00702001001000001", // ИКПУ
		PackageCode: "1512199",
		VatPercent:  12,
	}
}

func TestItemTotal(t *testing.T) {
	tests := []struct {
		name string
		item domain.Item
		want int64
	}{
		{"price times count", domain.Item{Price: 25000, Count: 2}, 50000},
		{"discount comes off the line", domain.Item{Price: 25000, Count: 2, Discount: 5000}, 45000},
		{"a single unit", domain.Item{Price: 25000, Count: 1}, 25000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.item.Total())
		})
	}
}

func TestDetailTotal(t *testing.T) {
	t.Run("sums the items", func(t *testing.T) {
		d := &domain.Detail{Items: []domain.Item{
			{Price: 25000, Count: 2},
			{Price: 10000, Count: 3},
		}}

		assert.Equal(t, int64(80000), d.Total())
	})

	t.Run("includes shipping", func(t *testing.T) {
		d := &domain.Detail{
			Items:    []domain.Item{{Price: 25000, Count: 2}},
			Shipping: &domain.Shipping{Title: "Delivery", Price: 15000},
		}

		assert.Equal(t, int64(65000), d.Total())
	})

	t.Run("an empty detail totals nothing", func(t *testing.T) {
		assert.Zero(t, (&domain.Detail{}).Total())
	})
}

func TestDetailValidate(t *testing.T) {
	t.Run("accepts a complete detail", func(t *testing.T) {
		d := &domain.Detail{ReceiptType: 0, Items: []domain.Item{validItem()}}

		assert.NoError(t, d.Validate())
	})

	rejects := []struct {
		name   string
		mutate func(*domain.Item)
		empty  bool
	}{
		{name: "no items at all", empty: true},
		{name: "an item with no title", mutate: func(i *domain.Item) { i.Title = "" }},
		{name: "an item with a negative price", mutate: func(i *domain.Item) { i.Price = -1 }},
		{name: "an item with no count", mutate: func(i *domain.Item) { i.Count = 0 }},
		{name: "an item with a negative count", mutate: func(i *domain.Item) { i.Count = -2 }},
		{name: "an item with no ИКПУ code", mutate: func(i *domain.Item) { i.Code = "" }},
	}

	for _, tt := range rejects {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			d := &domain.Detail{}
			if !tt.empty {
				item := validItem()
				tt.mutate(&item)
				d.Items = []domain.Item{item}
			}

			assert.ErrorIs(t, d.Validate(), domain.ErrMalformed)
		})
	}

	t.Run("a free item is allowed", func(t *testing.T) {
		item := validItem()
		item.Price = 0
		d := &domain.Detail{Items: []domain.Item{item}}

		assert.NoError(t, d.Validate())
	})
}

func TestDetailRoundTrip(t *testing.T) {
	original := &domain.Detail{
		ReceiptType: 0,
		Shipping:    &domain.Shipping{Title: "Delivery", Price: 15000},
		Items:       []domain.Item{validItem()},
	}

	encoded := domain.EncodeDetail(original)

	got, err := domain.DecodeDetail(encoded)

	require.NoError(t, err)
	assert.Equal(t, original, got)
}

func TestDecodeDetail(t *testing.T) {
	t.Run("rejects text that is not base64", func(t *testing.T) {
		_, err := domain.DecodeDetail("!!! not base64 !!!")

		assert.ErrorIs(t, err, domain.ErrMalformed)
	})

	t.Run("rejects base64 that is not JSON", func(t *testing.T) {
		_, err := domain.DecodeDetail("bm90IGpzb24=") // "not json"

		assert.ErrorIs(t, err, domain.ErrMalformed)
	})
}
