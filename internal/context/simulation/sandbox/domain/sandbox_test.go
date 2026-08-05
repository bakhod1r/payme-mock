package domain_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/simulation/sandbox/domain"
)

// seqGen produces predictable credentials so tests can assert on them.
type seqGen struct{ n int }

func (g *seqGen) MerchantID() string {
	g.n++
	return fmt.Sprintf("587f72c72cac0d162c722a%02d", g.n)
}

func (g *seqGen) Key() string {
	g.n++
	return fmt.Sprintf("key-%d", g.n)
}

func TestNew(t *testing.T) {
	t.Run("generates distinct production and sandbox keys", func(t *testing.T) {
		got, err := domain.New("qa", "QA stand", &seqGen{})

		require.NoError(t, err)
		assert.Equal(t, "qa", got.Slug)
		assert.Equal(t, "QA stand", got.Name)
		assert.NotEmpty(t, got.MerchantID)
		assert.NotEmpty(t, got.Key)
		assert.NotEmpty(t, got.TestKey)
		assert.NotEqual(t, got.Key, got.TestKey, "the two keys must differ")
	})

	t.Run("falls back to the slug when no name is given", func(t *testing.T) {
		got, err := domain.New("demo", "   ", &seqGen{})

		require.NoError(t, err)
		assert.Equal(t, "demo", got.Name)
	})

	t.Run("normalises the slug", func(t *testing.T) {
		got, err := domain.New("  QA-Stand  ", "x", &seqGen{})

		require.NoError(t, err)
		assert.Equal(t, "qa-stand", got.Slug)
	})

	accepted := []string{"qa", "dev", "flaky-test", "a1", "sandbox-2026", "x0"}
	for _, slug := range accepted {
		t.Run("accepts "+slug, func(t *testing.T) {
			_, err := domain.New(slug, "", &seqGen{})

			assert.NoError(t, err)
		})
	}

	rejected := []struct {
		name string
		slug string
	}{
		{"empty", ""},
		{"a single character", "a"},
		{"a leading hyphen", "-qa"},
		{"a trailing hyphen", "qa-"},
		{"a slash, which would break the endpoint path", "qa/prod"},
		{"a space", "qa stand"},
		{"an underscore", "qa_stand"},
		{"too long", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}

	for _, tt := range rejected {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			_, err := domain.New(tt.slug, "", &seqGen{})

			assert.ErrorIs(t, err, domain.ErrInvalid)
		})
	}
}

// Each sandbox gets its own endpoint path, which is what keeps one gateway
// serving many stands without them colliding.
func TestEndpointURLs(t *testing.T) {
	s, err := domain.New("qa", "", &seqGen{})
	require.NoError(t, err)

	const base = "https://merchant.localhost:8443"

	assert.Equal(t, base+"/s/qa/payme/merchant", s.EndpointURL(base))
	assert.Equal(t, base+"/s/qa/api", s.SubscribeURL(base))
}

func TestEndpointURLTrimsATrailingSlash(t *testing.T) {
	s, err := domain.New("qa", "", &seqGen{})
	require.NoError(t, err)

	assert.Equal(t, s.EndpointURL("https://x"), s.EndpointURL("https://x/"))
	assert.Equal(t, s.SubscribeURL("https://x"), s.SubscribeURL("https://x/"))
}

func TestKeyFor(t *testing.T) {
	s, err := domain.New("qa", "", &seqGen{})
	require.NoError(t, err)

	assert.Equal(t, s.TestKey, s.KeyFor(true))
	assert.Equal(t, s.Key, s.KeyFor(false))
}

// Two sandboxes never share credentials, which is what keeps their traffic
// from being accepted by the wrong stand.
func TestSandboxesGetDistinctCredentials(t *testing.T) {
	gen := &seqGen{}

	first, err := domain.New("qa", "", gen)
	require.NoError(t, err)
	second, err := domain.New("dev", "", gen)
	require.NoError(t, err)

	assert.NotEqual(t, first.MerchantID, second.MerchantID)
	assert.NotEqual(t, first.Key, second.Key)
	assert.NotEqual(t, first.TestKey, second.TestKey)
}
