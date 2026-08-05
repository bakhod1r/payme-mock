package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEndingSegments(t *testing.T) {
	t.Run("nothing to draw", func(t *testing.T) {
		assert.Nil(t, endingSummary{}.Segments(),
			"a chart of no payments is an empty rail that reads as a fault")
	})

	t.Run("shares add up", func(t *testing.T) {
		got := endingSummary{Settled: 5, InFlight: 3, Cancelled: 2, Total: 10}.Segments()

		require.Len(t, got, 3)
		assert.Equal(t, []string{"settled", "in progress", "cancelled"},
			[]string{got[0].Label, got[1].Label, got[2].Label})
		assert.InDelta(t, 50.0, got[0].Pct, 0.001)
		assert.InDelta(t, 30.0, got[1].Pct, 0.001)
		assert.InDelta(t, 20.0, got[2].Pct, 0.001)

		var total float64
		for _, seg := range got {
			total += seg.Pct
		}
		assert.InDelta(t, 100.0, total, 0.001)
	})

	t.Run("an ending nothing landed on is left out", func(t *testing.T) {
		got := endingSummary{Settled: 4, Total: 4}.Segments()

		require.Len(t, got, 1, "a zero-width segment still costs a gap and a legend entry")
		assert.Equal(t, "settled", got[0].Label)
		assert.InDelta(t, 100.0, got[0].Pct, 0.001)
	})

	t.Run("each ending keeps its own colour", func(t *testing.T) {
		got := endingSummary{Settled: 1, InFlight: 1, Cancelled: 1, Total: 3}.Segments()

		require.Len(t, got, 3)
		assert.Equal(t, chartSettled, got[0].Color)
		assert.Equal(t, chartInFlight, got[1].Color)
		assert.Equal(t, chartCancelled, got[2].Color)
	})
}

func TestTrafficBars(t *testing.T) {
	t.Run("no methods, no chart", func(t *testing.T) {
		assert.Nil(t, trafficSummary{}.Bars())
	})

	t.Run("measured against the busiest", func(t *testing.T) {
		got := trafficSummary{Methods: []methodCount{
			{Method: "CheckPerformTransaction", Calls: 20},
			{Method: "receipts.pay", Calls: 5},
		}}.Bars()

		require.Len(t, got, 2)
		assert.InDelta(t, 100.0, got[0].Pct, 0.001, "the busiest method fills the track")
		assert.InDelta(t, 25.0, got[1].Pct, 0.001)
		assert.Equal(t, 5, got[1].Count, "the count is shown beside the bar, not derived from it")
	})

	t.Run("counted but never called", func(t *testing.T) {
		assert.Nil(t, trafficSummary{Methods: []methodCount{{Method: "x"}}}.Bars(),
			"dividing by the largest is a division by zero when nothing was called")
	})
}

func TestCashboxActivity(t *testing.T) {
	t.Run("both directions settle into one segment", func(t *testing.T) {
		got := cashboxSummary{TopUps: 3, Withdrawals: 1, InFlight: 1}.Activity()

		require.Len(t, got, 2)
		assert.Equal(t, "settled", got[0].Label)
		assert.Equal(t, 4, got[0].Count, "a withdrawal is as settled as a top-up")
		assert.InDelta(t, 80.0, got[0].Pct, 0.001)
		assert.Equal(t, "in progress", got[1].Label)
	})

	t.Run("a cashbox nothing went through", func(t *testing.T) {
		assert.Nil(t, cashboxSummary{}.Activity())
	})
}
