package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// The card number and mask the documentation shows.
const (
	docCardNumber = "8600069195406311"
	docCardMask   = "860006******6311"
)

func newCard() *domain.Card {
	return &domain.Card{
		ID:         testCardID,
		SandboxID:  1,
		Token:      "NTg0YTg0ZDYyYWJiNWNhYTMxMDc5OTE0",
		NumberFull: docCardNumber,
		Expire:     "03/99",
		Verify:     true,
		Balance:    100_000_000,
	}
}

func TestMaskNumber(t *testing.T) {
	tests := []struct {
		name   string
		number string
		want   string
	}{
		{"the documented example", docCardNumber, docCardMask},
		{"a short number is left alone", "12345", "12345"},
		{"exactly ten digits hides nothing", "1234567890", "1234567890"},
		{"eleven digits hides one", "12345678901", "123456*8901"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, domain.MaskNumber(tt.number))
		})
	}
}

func TestFormatExpire(t *testing.T) {
	assert.Equal(t, "03/99", domain.FormatExpire("0399"))
	assert.Equal(t, "12/26", domain.FormatExpire("1226"))
	assert.Equal(t, "039", domain.FormatExpire("039"), "an unexpected length is passed through")
	assert.Equal(t, "", domain.FormatExpire(""))
}

func TestDetectSystem(t *testing.T) {
	assert.Equal(t, domain.CardUzcard, domain.DetectSystem("8600069195406311"))
	assert.Equal(t, domain.CardHumo, domain.DetectSystem("9860016101234567"))
	assert.Equal(t, domain.CardUzcard, domain.DetectSystem("1234"), "an unknown prefix defaults to Uzcard")
}

func TestCardNumberMaskAndSystem(t *testing.T) {
	c := newCard()

	assert.Equal(t, docCardMask, c.NumberMask())
	assert.Equal(t, domain.CardUzcard, c.System())
}

func TestSendVerifyCodeStartsTheWindow(t *testing.T) {
	c := newCard()

	c.SendVerifyCode("666666", now, domain.DefaultVerifyWaitMillis)

	assert.Equal(t, "666666", c.VerifyCode)
	assert.Equal(t, now, c.VerifyCodeSentAt)
	assert.Equal(t, int64(60000), c.VerifyWaitMillis)
}

func TestVerifyWith(t *testing.T) {
	t.Run("the right code inside the window verifies the card", func(t *testing.T) {
		c := newCard()
		c.Verify = false
		c.SendVerifyCode("666666", now, domain.DefaultVerifyWaitMillis)

		require.NoError(t, c.VerifyWith("666666", now+59_999))

		assert.True(t, c.Verify)
		assert.Empty(t, c.VerifyCode, "a used code must not stay usable")
	})

	t.Run("the wrong code is refused", func(t *testing.T) {
		c := newCard()
		c.Verify = false
		c.SendVerifyCode("666666", now, domain.DefaultVerifyWaitMillis)

		assert.ErrorIs(t, c.VerifyWith("000000", now), payerr.ErrCannotPerform)
		assert.False(t, c.Verify)
	})

	t.Run("no code was ever sent", func(t *testing.T) {
		c := newCard()
		c.Verify = false

		assert.ErrorIs(t, c.VerifyWith("666666", now), payerr.ErrCannotPerform)
	})

	// The documented `wait` is a real deadline, not decoration.
	t.Run("the right code past its window is refused", func(t *testing.T) {
		c := newCard()
		c.Verify = false
		c.SendVerifyCode("666666", now, domain.DefaultVerifyWaitMillis)

		assert.ErrorIs(t, c.VerifyWith("666666", now+60_001), payerr.ErrCannotPerform)
		assert.False(t, c.Verify)
	})

	t.Run("a code exactly at its deadline still works", func(t *testing.T) {
		c := newCard()
		c.Verify = false
		c.SendVerifyCode("666666", now, domain.DefaultVerifyWaitMillis)

		assert.NoError(t, c.VerifyWith("666666", now+60_000))
	})

	t.Run("a code with no window never expires", func(t *testing.T) {
		c := newCard()
		c.Verify = false
		c.SendVerifyCode("666666", now, 0)

		assert.NoError(t, c.VerifyWith("666666", now+999_999))
	})

	t.Run("a removed card cannot be verified", func(t *testing.T) {
		c := newCard()
		c.Removed = true
		c.SendVerifyCode("666666", now, domain.DefaultVerifyWaitMillis)

		assert.ErrorIs(t, c.VerifyWith("666666", now), payerr.ErrCannotPerform)
	})
}

func TestCardUsable(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*domain.Card)
		amount  int64
		wantErr bool
	}{
		{"a verified card with funds", func(*domain.Card) {}, amount, false},
		{"exactly the balance", func(c *domain.Card) { c.Balance = amount }, amount, false},
		{"an unverified card", func(c *domain.Card) { c.Verify = false }, amount, true},
		{"a removed card", func(c *domain.Card) { c.Removed = true }, amount, true},
		{"insufficient funds", func(c *domain.Card) { c.Balance = amount - 1 }, amount, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCard()
			tt.mutate(c)

			err := c.Usable(tt.amount)

			if tt.wantErr {
				assert.ErrorIs(t, err, payerr.ErrCannotPerform)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestChargeDeductsTheAmount(t *testing.T) {
	c := newCard()
	before := c.Balance

	c.Charge(amount)

	assert.Equal(t, before-amount, c.Balance)
}

func TestRefundRestoresTheBalance(t *testing.T) {
	c := newCard()
	before := c.Balance
	c.Charge(amount)

	c.Refund(amount)

	assert.Equal(t, before, c.Balance)
}

// The provider prints an expiry both ways: slashed on a card object, plain
// inside a receipt. A receipt reporting "03/99" would not match a client
// reading the documented "0399".
func TestPlainExpire(t *testing.T) {
	assert.Equal(t, "0399", domain.PlainExpire("03/99"))
	assert.Equal(t, "0399", domain.PlainExpire("0399"), "already plain is left alone")
	assert.Empty(t, domain.PlainExpire(""))
}
