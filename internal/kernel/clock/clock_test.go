package clock_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
)

func TestSystemNow(t *testing.T) {
	c := clock.New()

	before := time.Now()
	got := c.Now()
	after := time.Now()

	assert.False(t, got.Before(before))
	assert.False(t, got.After(after))
}

func TestSystemNowMillis(t *testing.T) {
	c := clock.New()

	before := clock.ToMillis(time.Now())
	got := c.NowMillis()
	after := clock.ToMillis(time.Now())

	assert.GreaterOrEqual(t, got, before)
	assert.LessOrEqual(t, got, after)
}

// A stall is what the stand rehearses a timeout with, so the wait has to be
// real — and it has to end early when the caller has already hung up, because
// finishing a ten second stall for a client that is gone holds a connection
// open for nothing.
func TestSystemSleep(t *testing.T) {
	started := time.Now()
	clock.New().Sleep(context.Background(), 20*time.Millisecond)
	assert.GreaterOrEqual(t, time.Since(started), 15*time.Millisecond,
		"a stall that returns at once is not a stall")

	t.Run("a cancelled caller ends the wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		started := time.Now()
		clock.New().Sleep(ctx, time.Hour)

		assert.Less(t, time.Since(started), time.Second,
			"the wait ends with the caller, not with the timer")
	})

	t.Run("nothing to wait for", func(t *testing.T) {
		started := time.Now()
		clock.New().Sleep(context.Background(), 0)
		clock.New().Sleep(context.Background(), -time.Second)

		assert.Less(t, time.Since(started), 50*time.Millisecond)
	})
}

// The fake moves its own clock instead of blocking, so a test can rehearse a
// ten second stall without spending ten seconds on it.
func TestFakeSleep(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0).UTC())

	fake.Sleep(context.Background(), 10*time.Second)
	assert.Equal(t, int64(10000), fake.NowMillis())

	fake.Sleep(context.Background(), 0)
	fake.Sleep(context.Background(), -time.Minute)
	assert.Equal(t, int64(10000), fake.NowMillis(),
		"a wait for no time moves no time")
}

func TestFakeAdvance(t *testing.T) {
	start := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	f := clock.NewFake(start)

	require.Equal(t, start, f.Now())

	f.Advance(12 * time.Hour)

	assert.Equal(t, start.Add(12*time.Hour), f.Now())
	assert.Equal(t, clock.ToMillis(start.Add(12*time.Hour)), f.NowMillis())
}

func TestFakeSet(t *testing.T) {
	f := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	target := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)

	f.Set(target)

	assert.Equal(t, target, f.Now())
}

func TestMillisRoundTrip(t *testing.T) {
	// The doc's own example timestamp.
	const docTimestamp int64 = 1399114284039

	got := clock.ToMillis(clock.FromMillis(docTimestamp))

	assert.Equal(t, docTimestamp, got)
}

func TestFromMillisIsUTC(t *testing.T) {
	got := clock.FromMillis(1399114284039)

	assert.Equal(t, time.UTC, got.Location())
}
