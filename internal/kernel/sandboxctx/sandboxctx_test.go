package sandboxctx_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

func TestWithAndGet(t *testing.T) {
	want := sandboxctx.Sandbox{
		ID: 7, Slug: "qa", MerchantID: "587f72c72cac0d162c722ae2",
		Key: "live-key", TestKey: "test-key", ConfigID: 3,
	}

	got, ok := sandboxctx.Get(sandboxctx.With(context.Background(), want))

	require.True(t, ok)
	assert.Equal(t, want, got)
}

func TestGetOnAnUnscopedContext(t *testing.T) {
	got, ok := sandboxctx.Get(context.Background())

	assert.False(t, ok)
	assert.Zero(t, got)
}

func TestFrom(t *testing.T) {
	ctx := sandboxctx.With(context.Background(), sandboxctx.Sandbox{ID: 42})

	got, err := sandboxctx.From(ctx)

	require.NoError(t, err)
	assert.Equal(t, int64(42), got)
}

// A repository must never run a query without a sandbox: doing so would read
// across every stand at once. The failure is loud rather than silent.
func TestFromRefusesAnUnscopedContext(t *testing.T) {
	got, err := sandboxctx.From(context.Background())

	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)
	assert.Zero(t, got)
}

func TestWithReplacesAnEarlierSandbox(t *testing.T) {
	ctx := sandboxctx.With(context.Background(), sandboxctx.Sandbox{ID: 1, Slug: "dev"})
	ctx = sandboxctx.With(ctx, sandboxctx.Sandbox{ID: 2, Slug: "qa"})

	got, ok := sandboxctx.Get(ctx)

	require.True(t, ok)
	assert.Equal(t, "qa", got.Slug)
}
