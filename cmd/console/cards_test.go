package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	subscribe "github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
)

func TestValidCardNumber(t *testing.T) {
	tests := []struct {
		name   string
		number string
		want   bool
	}{
		{"an Uzcard number", "8600069195406311", true},
		{"a Humo number", "9860016101234567", true},
		// The provider's own set includes numbers on no Uzbek network, so the
		// network is not what makes a number valid here.
		{"a number on no Uzbek network", "4444445987459073", true},
		{"too short", "860006919540631", false},
		{"too long", "86000691954063110", false},
		{"letters", "860006919540631x", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, validCardNumber(tt.number))
		})
	}
}

func TestValidExpire(t *testing.T) {
	assert.True(t, validExpire("0399"))
	assert.True(t, validExpire("1226"))
	assert.False(t, validExpire("1326"), "month thirteen does not exist")
	assert.False(t, validExpire("0026"), "month zero does not exist")
	assert.False(t, validExpire("399"))
	assert.False(t, validExpire("03/9"))
	assert.False(t, validExpire(""))
}

// A card past its date is exactly what someone rehearsing a refusal enters, so
// an old year is accepted rather than corrected.
func TestValidExpireAcceptsPastYears(t *testing.T) {
	assert.True(t, validExpire("0101"))
}

func TestParseCardForm(t *testing.T) {
	card, err := parseCardForm(cardRequest(url.Values{
		"sandbox_id": {"7"},
		"number":     {"8600 0691 9540 6311"},
		"expire":     {"03/99"},
		"balance":    {"250000"},
		"phone":      {"998901234567"},
		"outcome":    {string(subscribe.OutcomeInsufficientFunds)},
		"verify":     {"1"},
	}))
	require.NoError(t, err)

	// The number is written with spaces and the expiry with a slash, which is
	// how they are printed on a card; neither shape reaches the column.
	assert.Equal(t, int64(7), card.SandboxID)
	assert.Equal(t, "8600069195406311", card.Number)
	assert.Equal(t, "0399", card.Expire)
	assert.Equal(t, int64(250000), card.Balance)
	assert.Equal(t, subscribe.OutcomeInsufficientFunds, card.Outcome)
	assert.True(t, card.Verify)
	assert.False(t, card.Recurrent, "an unticked box is not sent at all")
}

func TestParseCardFormDefaultsToSuccess(t *testing.T) {
	card, err := parseCardForm(cardRequest(url.Values{
		"sandbox_id": {"1"},
		"number":     {"9860016101234567"},
		"expire":     {"1226"},
	}))
	require.NoError(t, err)

	assert.Equal(t, subscribe.OutcomeSuccess, card.Outcome)
	assert.Equal(t, int64(0), card.Balance, "an empty balance is zero, not a failure")
}

func TestParseCardFormRejects(t *testing.T) {
	tests := []struct {
		name  string
		form  url.Values
		wants string
	}{
		{
			"no sandbox",
			url.Values{"number": {"8600069195406311"}, "expire": {"0399"}},
			"pick the sandbox",
		},
		{
			"a number of the wrong length",
			url.Values{"sandbox_id": {"1"}, "number": {"42783100123456"}, "expire": {"0399"}},
			"sixteen digits",
		},
		{
			"an impossible month",
			url.Values{"sandbox_id": {"1"}, "number": {"8600069195406311"}, "expire": {"1399"}},
			"MMYY",
		},
		{
			"a negative balance",
			url.Values{"sandbox_id": {"1"}, "number": {"8600069195406311"}, "expire": {"0399"}, "balance": {"-1"}},
			"zero or more",
		},
		{
			"a behaviour that does not exist",
			url.Values{"sandbox_id": {"1"}, "number": {"8600069195406311"}, "expire": {"0399"}, "outcome": {"nonsense"}},
			"unknown card behaviour",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCardForm(cardRequest(tt.form))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wants)
		})
	}
}

// The list marks a rigged card without the template comparing strings.
func TestCardRowDecorate(t *testing.T) {
	row := cardRow{Number: "8600069195406311", Outcome: string(subscribe.OutcomeBlocked)}
	row.decorate()

	assert.Equal(t, "860006******6311", row.Mask)
	assert.Equal(t, "uzcard", row.System)
	assert.Equal(t, "blocked", row.Behaviour)
	assert.True(t, row.Fails)

	working := cardRow{Number: "9860016101234567", Outcome: string(subscribe.OutcomeSuccess)}
	working.decorate()

	assert.Equal(t, "humo", working.System)
	assert.False(t, working.Fails)
}

// A list is only readable if it says which cards an operator put there and
// which an integration tokenized for itself.
func TestCardRowSource(t *testing.T) {
	added := cardRow{Number: "8600069195406311", Outcome: string(subscribe.OutcomeSuccess), Source: sourceConsole}
	added.decorate()
	assert.True(t, added.Added)

	tokenized := cardRow{Number: "8600069195406311", Outcome: string(subscribe.OutcomeSuccess), Source: sourceAPI}
	tokenized.decorate()
	assert.False(t, tokenized.Added)
}

// The screen opens on the half an operator came for; the other half is one
// click away and anything unrecognized is not a third tab.
func TestCardTab(t *testing.T) {
	tests := []struct {
		query   string
		wantTab string
	}{
		{"", tabMocks},
		{"?tab=mocks", tabMocks},
		{"?tab=cashbox", tabCashbox},
		{"?tab=nonsense", tabMocks},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			tab := cardTab(httptest.NewRequest(http.MethodGet, "/cards"+tt.query, nil))
			assert.Equal(t, tt.wantTab, tab)
		})
	}
}

func TestCardOutcomesCoverTheDomain(t *testing.T) {
	options := cardOutcomes()
	require.Len(t, options, len(subscribe.Outcomes))

	assert.Equal(t, string(subscribe.OutcomeSuccess), options[0].Value,
		"the working card is offered first")
	assert.False(t, options[0].Failing)

	for _, option := range options[1:] {
		assert.True(t, option.Failing, option.Value)
	}
}

func cardRequest(form url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/cards", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}
