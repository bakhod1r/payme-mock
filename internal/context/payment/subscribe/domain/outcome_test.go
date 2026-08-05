package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// A rigged card is only useful if it fails the way the protocol allows: a
// documented code, and all three translations. Every refusal of the card
// itself is -31008; a system error is not a refusal at all and says so.
func TestOutcomeErr(t *testing.T) {
	assert.NoError(t, domain.OutcomeSuccess.Err(), "a working card has nothing to answer with")

	want := map[domain.Outcome]payerr.Code{
		domain.OutcomeInsufficientFunds: payerr.CodeCannotPerform,
		domain.OutcomeBlocked:           payerr.CodeCannotPerform,
		domain.OutcomeExpired:           payerr.CodeCannotPerform,
		domain.OutcomeVerifyFailed:      payerr.CodeCannotPerform,
		domain.OutcomeSystemError:       payerr.CodeTransport,
	}

	for _, outcome := range domain.Outcomes {
		if outcome == domain.OutcomeSuccess {
			continue
		}

		t.Run(string(outcome), func(t *testing.T) {
			err := outcome.Err()
			require.Error(t, err)

			protocol, ok := payerr.As(err)
			require.True(t, ok)

			code, known := want[outcome]
			require.True(t, known, "a new outcome needs its code stated here")
			assert.Equal(t, code, protocol.Code)
			assert.True(t, protocol.Message.Complete(), "all three languages are mandatory")
		})
	}
}

func TestOutcomeValid(t *testing.T) {
	for _, outcome := range domain.Outcomes {
		assert.True(t, outcome.Valid(), string(outcome))
		assert.NotEmpty(t, outcome.Label())
	}

	assert.False(t, domain.Outcome("").Valid())
	assert.False(t, domain.Outcome("nonsense").Valid())
}

// A card rigged to refuse pays no attention to its balance: that is the point
// of rigging one, since arranging a balance to fall short of an amount the
// integration chooses is not always possible.
func TestUsableRefusesRiggedCard(t *testing.T) {
	tests := []struct {
		outcome domain.Outcome
		verify  bool
	}{
		{domain.OutcomeInsufficientFunds, true},
		{domain.OutcomeBlocked, true},
		{domain.OutcomeExpired, true},
	}

	for _, tt := range tests {
		t.Run(string(tt.outcome), func(t *testing.T) {
			card := newCard()
			card.Outcome = tt.outcome
			card.Verify = tt.verify

			err := card.Usable(1)
			require.ErrorIs(t, err, payerr.ErrCannotPerform)
			assert.Equal(t, tt.outcome.Err().Error(), err.Error(),
				"the refusal names its own reason")
		})
	}
}

func TestUsableAllowsSuccessCard(t *testing.T) {
	card := newCard()
	card.Outcome = domain.OutcomeSuccess

	assert.NoError(t, card.Usable(1))
}

// A stopped or expired card is refused before an OTP is spent on it.
func TestOperable(t *testing.T) {
	for _, outcome := range []domain.Outcome{domain.OutcomeBlocked, domain.OutcomeExpired} {
		card := newCard()
		card.Outcome = outcome

		require.ErrorIs(t, card.Operable(), payerr.ErrCannotPerform, string(outcome))
	}

	working := newCard()
	working.Outcome = domain.OutcomeInsufficientFunds
	assert.NoError(t, working.Operable(), "a card short of money is still a live card")

	removed := newCard()
	removed.Removed = true
	assert.ErrorIs(t, removed.Operable(), payerr.ErrCannotPerform)
}

// The verify_failed card takes the right code and rejects it, which is the only
// way to reach the branch an integration runs when a bank refuses confirmation.
func TestVerifyWithRiggedCard(t *testing.T) {
	card := newCard()
	card.Outcome = domain.OutcomeVerifyFailed
	card.Verify = false
	card.SendVerifyCode("666666", 1000, domain.DefaultVerifyWaitMillis)

	err := card.VerifyWith("666666", 1000)
	require.ErrorIs(t, err, payerr.ErrCannotPerform)
	assert.False(t, card.Verify, "a card that cannot be verified never becomes usable")
	assert.ErrorIs(t, card.Usable(1), payerr.ErrCannotPerform)
}

func TestVerifyWithSuccessCard(t *testing.T) {
	card := newCard()
	card.Verify = false
	card.SendVerifyCode("666666", 1000, domain.DefaultVerifyWaitMillis)

	require.NoError(t, card.VerifyWith("666666", 1000))
	assert.True(t, card.Verify)
}

// Money arriving is not money leaving: a card that cannot cover a payment can
// still take a payout, while a stopped one cannot. Operable is what a payout
// checks, so this is the same rule seen from the other direction.
func TestOperableAllowsACardShortOfMoney(t *testing.T) {
	short := newCard()
	short.Outcome = domain.OutcomeInsufficientFunds
	assert.NoError(t, short.Operable())

	blocked := newCard()
	blocked.Outcome = domain.OutcomeBlocked
	assert.ErrorIs(t, blocked.Operable(), payerr.ErrCannotPerform)
}

// A stand holds many rigged cards and one shared code cannot tell them apart,
// so a card an operator added takes the code its own expiry spells out.
func TestExpiryCode(t *testing.T) {
	assert.Equal(t, "122626", domain.ExpiryCode("12/26"))
	assert.Equal(t, "122626", domain.ExpiryCode("1226"))
	assert.Equal(t, "039999", domain.ExpiryCode("03/99"))
	assert.Empty(t, domain.ExpiryCode("039"), "an expiry the mock cannot read spells no code")
	assert.Empty(t, domain.ExpiryCode(""))
}

// Where a card came from decides its OTP: an operator's card takes the code the
// operator knows, an integration's card takes the one its own expiry spells out.
func TestExpectedVerifyCode(t *testing.T) {
	added := newCard()
	added.Source = domain.SourceConsole
	added.Expire = "12/26"
	assert.Equal(t, "666666", added.ExpectedVerifyCode("666666"),
		"a card an operator added takes the stand's shared code")

	tokenized := newCard()
	tokenized.Source = domain.SourceAPI
	tokenized.Expire = "12/26"
	assert.Equal(t, "122626", tokenized.ExpectedVerifyCode("666666"),
		"a card an integration tokenized takes its own expiry")

	// With no shared code configured there is nothing to fall back to, so even
	// an operator's card is left on its expiry rather than on no code at all.
	noShared := newCard()
	noShared.Source = domain.SourceConsole
	noShared.Expire = "12/26"
	assert.Equal(t, "122626", noShared.ExpectedVerifyCode(""))

	// An expiry nothing can be derived from leaves the card on the shared code
	// rather than on no code at all, which could never be verified.
	odd := newCard()
	odd.Expire = "nonsense"
	assert.Equal(t, "666666", odd.ExpectedVerifyCode("666666"))
}

// Either code confirms a card: the stand's shared 666666, which an operator
// types from memory without looking anything up, and the card's own — its
// expiry, which an integration holding the number can derive. A stand that
// accepted only one of the two made confirming a card a question of which rule
// this particular card fell under, which is not a question a stand should ask.
func TestVerifyTakesTheSharedCodeAndTheCardsOwn(t *testing.T) {
	for _, code := range []string{"666666", "123030"} {
		card := newCard()
		card.Source = domain.SourceAPI
		card.Expire = "12/30"
		card.SendVerifyCode("123030", 0, 60000)

		require.NoError(t, card.VerifyWith(code, 0), code)
		assert.True(t, card.Verify, code)
	}

	// Anything else is still refused.
	wrong := newCard()
	wrong.SendVerifyCode("123030", 0, 60000)
	assert.Error(t, wrong.VerifyWith("000000", 0))
}

// A card whose owner never signed up for SMS cannot be sent a code at all,
// which is a different failure from a code that turns out to be wrong.
func TestSMSReachable(t *testing.T) {
	reachable := newCard()
	reachable.SMSEnabled = true
	assert.NoError(t, reachable.SMSReachable())

	unreachable := newCard()
	unreachable.SMSEnabled = false

	err := unreachable.SMSReachable()
	require.Error(t, err)

	protocol, ok := payerr.As(err)
	require.True(t, ok)
	assert.Equal(t, payerr.CodeCannotPerform, protocol.Code)
	assert.True(t, protocol.Message.Complete())
}

// A frozen card pays and is paid without the figure moving, so one card can
// drive a run of rehearsal payments untended.
func TestFrozenBalanceNeverMoves(t *testing.T) {
	card := newCard()
	card.Frozen = true
	start := card.Balance

	card.Charge(10_000)
	card.Refund(10_000)
	card.Receive(10_000)

	assert.Equal(t, start, card.Balance)
}

func TestUnfrozenBalanceMoves(t *testing.T) {
	card := newCard()
	start := card.Balance

	card.Charge(10_000)
	assert.Equal(t, start-10_000, card.Balance)

	card.Refund(10_000)
	assert.Equal(t, start, card.Balance)

	card.Receive(5_000)
	assert.Equal(t, start+5_000, card.Balance)
}

// A system error is not the provider refusing the card; it is the provider
// never getting far enough to look, which is a different code.
func TestSystemErrorAnswersTransport(t *testing.T) {
	protocol, ok := payerr.As(domain.OutcomeSystemError.Err())
	require.True(t, ok)
	assert.Equal(t, payerr.CodeTransport, protocol.Code)

	card := newCard()
	card.Outcome = domain.OutcomeSystemError
	assert.ErrorIs(t, card.Operable(), payerr.ErrTransport,
		"a card that errors refuses every operation, not only the payment")
}
