package errors_test

import (
	stderrors "errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

func TestIsAccountCode(t *testing.T) {
	tests := []struct {
		name string
		code payerr.Code
		want bool
	}{
		{"lower bound", -31099, true},
		{"upper bound", -31050, true},
		{"middle", -31075, true},
		{"just below range", -31100, false},
		{"just above range", -31049, false},
		{"cannot perform", payerr.CodeCannotPerform, false},
		{"unauthorized", payerr.CodeUnauthorized, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.code.IsAccountCode())
		})
	}
}

func TestMessageLocalize(t *testing.T) {
	msg := payerr.NewMessage("русский", "o'zbek", "english")

	tests := []struct {
		lang string
		want string
	}{
		{"ru", "русский"},
		{"uz", "o'zbek"},
		{"en", "english"},
		{"", "русский"},   // default
		{"fr", "русский"}, // unknown falls back
	}

	for _, tt := range tests {
		t.Run(tt.lang, func(t *testing.T) {
			assert.Equal(t, tt.want, msg.Localize(tt.lang))
		})
	}
}

func TestMessageComplete(t *testing.T) {
	tests := []struct {
		name string
		msg  payerr.Message
		want bool
	}{
		{"all present", payerr.NewMessage("a", "b", "c"), true},
		{"ru missing", payerr.NewMessage("", "b", "c"), false},
		{"uz missing", payerr.NewMessage("a", "", "c"), false},
		{"en missing", payerr.NewMessage("a", "b", ""), false},
		{"all missing", payerr.Message{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.msg.Complete())
		})
	}
}

func TestProtocolErrorError(t *testing.T) {
	t.Run("without data", func(t *testing.T) {
		err := payerr.New(payerr.CodeCannotPerform, payerr.NewMessage("нельзя", "mumkin emas", "cannot"), "")

		assert.Equal(t, "payme error -31008: нельзя", err.Error())
	})

	t.Run("with data", func(t *testing.T) {
		err := payerr.New(payerr.CodeAccountMax, payerr.NewMessage("не найден", "topilmadi", "not found"), "phone")

		assert.Equal(t, "payme error -31050: не найден (phone)", err.Error())
	})
}

func TestProtocolErrorIs(t *testing.T) {
	t.Run("same code matches", func(t *testing.T) {
		err := fmt.Errorf("wrapped: %w", payerr.ErrTransactionNotFound)

		assert.True(t, stderrors.Is(err, payerr.ErrTransactionNotFound))
	})

	t.Run("different code does not match", func(t *testing.T) {
		assert.False(t, stderrors.Is(payerr.ErrCannotPerform, payerr.ErrTransactionNotFound))
	})

	t.Run("non-protocol error does not match", func(t *testing.T) {
		// The target is deliberately an error nothing can match, which is what
		// the assertion is about: a protocol error must not report itself as
		// some unrelated one.
		//nolint:staticcheck // SA1032: the unmatchable target is the point
		assert.False(t, stderrors.Is(payerr.ErrCannotPerform, stderrors.New("plain")))
	})
}

func TestWithDataDoesNotMutateCatalogEntry(t *testing.T) {
	got := payerr.ErrAccountNotFound.WithData("phone")

	assert.Equal(t, "phone", got.Data)
	assert.Empty(t, payerr.ErrAccountNotFound.Data, "catalog entry must stay pristine")
	assert.Equal(t, payerr.ErrAccountNotFound.Code, got.Code)
}

func TestWithMessageDoesNotMutateCatalogEntry(t *testing.T) {
	custom := payerr.NewMessage("свой", "o'ziniki", "custom")

	got := payerr.ErrInvalidAmount.WithMessage(custom)

	assert.Equal(t, custom, got.Message)
	assert.NotEqual(t, custom, payerr.ErrInvalidAmount.Message)
}

func TestAs(t *testing.T) {
	t.Run("finds wrapped protocol error", func(t *testing.T) {
		err := fmt.Errorf("layer: %w", payerr.ErrInvalidAmount)

		got, ok := payerr.As(err)

		require.True(t, ok)
		assert.Equal(t, payerr.CodeInvalidAmount, got.Code)
	})

	t.Run("reports miss for plain error", func(t *testing.T) {
		got, ok := payerr.As(stderrors.New("plain"))

		assert.False(t, ok)
		assert.Nil(t, got)
	})
}

// The protocol requires every error response to carry all three translations.
// This guards the whole catalog at once.
func TestCatalogIsFullyLocalized(t *testing.T) {
	require.NotEmpty(t, payerr.Catalog)

	for _, e := range payerr.Catalog {
		t.Run(fmt.Sprint(e.Code), func(t *testing.T) {
			assert.True(t, e.Message.Complete(), "error %d is missing a translation", e.Code)
		})
	}
}

func TestCatalogHasNoDuplicateCodes(t *testing.T) {
	seen := make(map[payerr.Code]bool, len(payerr.Catalog))

	for _, e := range payerr.Catalog {
		assert.False(t, seen[e.Code], "duplicate code %d in catalog", e.Code)
		seen[e.Code] = true
	}
}

func TestByCode(t *testing.T) {
	t.Run("known code", func(t *testing.T) {
		got, ok := payerr.ByCode(payerr.CodeUnauthorized)

		require.True(t, ok)
		assert.Equal(t, payerr.ErrUnauthorized, got)
	})

	t.Run("unknown code", func(t *testing.T) {
		got, ok := payerr.ByCode(-99999)

		assert.False(t, ok)
		assert.Nil(t, got)
	})
}
