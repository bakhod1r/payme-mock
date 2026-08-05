package main

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every form on the console names a row in its address, and every one of them
// can be sent an address naming nothing: a bookmark to a stand somebody
// deleted, a page left open while another operator cleared up. None of them may
// act on the row they cannot find, and none may fall over on the way.
func TestE2EEveryFormRefusesAnAddressNamingNoRow(t *testing.T) {
	s := newStand(t)

	paths := []string{
		"/sandboxes/nope",
		"/sandboxes/nope/reset",
		"/sandboxes/nope/delete",
		"/sandboxes/nope/orders",
		"/sandboxes/nope/access",
		"/orders/nope/delete",
		"/access/nope/delete",
		"/accounts/nope",
		"/accounts/nope/delete",
		"/accounts/nope/balance",
		"/accounts/nope/block",
		"/transactions/nope",
		"/transactions/nope/delete",
		"/traffic/nope/delete",
		"/rules/nope",
		"/rules/nope/toggle",
		"/rules/nope/delete",
		"/cards/nope",
		"/cards/nope/delete",
		"/cards/nope/block",
		"/cards/nope/cashbox",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			w := s.post(t, path, url.Values{})

			assert.Equal(t, http.StatusOK, w.Code)

			body := w.Body.String()
			assert.True(t,
				strings.Contains(body, "That row is gone") ||
					strings.Contains(body, "Unknown"),
				"the screen says the row is not there: %s", firstError(body))
		})
	}
}

// A body the form parser cannot read is the sender's mistake. The screen says
// so and stays a screen, rather than answering with a page about a row.
func TestE2EFormsReportAMalformedSubmission(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "malformed", "topup", 100000)
	id := strconv.FormatInt(sandbox.ID, 10)

	for _, path := range []string{
		"/sandboxes/" + id,
		"/accounts/" + strconv.FormatInt(sandbox.AccountID, 10) + "/balance",
		"/cards/seed",
	} {
		t.Run(path, func(t *testing.T) {
			w := s.postRaw(t, path, "%zz")

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, w.Body.String(), "Malformed form")
		})
	}
}

// A profile is chosen from a list, so a value that is not one of them is a
// broken client rather than a choice, and the stand is left on the profile it
// had.
func TestE2ESandboxRefusesAProfileThatIsNotOne(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "profile-bad", "topup", 100000)

	w := s.post(t, "/sandboxes/"+strconv.FormatInt(sandbox.ID, 10), url.Values{
		"name":      {"still here"},
		"kind":      {"topup"},
		"config_id": {"not a number"},
	})

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Unknown profile")
}

// ---------- what the store refuses, where no screen can reach it ----------

// A card the store is asked to write has to be a card. The screens check the
// same things, but the store is what the rest of the console trusts, so it
// checks them itself.
func TestE2EStoreRefusesACardThatIsNotOne(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "store-cards", "topup", 100000)
	ctx := context.Background()

	good := newCard{SandboxID: sandbox.ID, Number: uzcard, Expire: "0399", Outcome: "success"}

	tests := map[string]struct {
		card   newCard
		expect string
	}{
		"a balance below zero": {
			card:   func() newCard { c := good; c.Balance = -1; return c }(),
			expect: "balance",
		},
		"a month that is not one": {
			card:   func() newCard { c := good; c.Expire = "0099"; return c }(),
			expect: "MMYY",
		},
		"a stand that is not there": {
			card:   func() newCard { c := good; c.SandboxID = 999999; return c }(),
			expect: "insert card",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := s.store.CreateCard(ctx, tt.card)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expect)
		})
	}
}

// A card whose row was written but whose token could not be recorded is not a
// card: the API finds a card by the token the caller sends, so nothing could
// ever reach it.
func TestE2EStoreReportsACardTokenItCouldNotRecord(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "store-token", "topup", 100000)
	ctx := context.Background()

	_, err := s.store.pool.Exec(ctx, `DROP TABLE mock.card_tokens`)
	require.NoError(t, err)

	_, err = s.store.CreateCard(ctx, newCard{
		SandboxID: sandbox.ID, Number: uzcard, Expire: "0399", Outcome: "success",
	})

	assert.ErrorContains(t, err, "issue card token")
}

// Editing a card is editing what it does, and what it does has to make sense
// before a row is touched.
func TestE2EStoreRefusesAnEditThatMakesNoSense(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "store-edit", "topup", 100000)
	card := s.riggedCard(t, sandbox, uzcard, "success")
	ctx := context.Background()

	tests := map[string]struct {
		card   newCard
		expect string
	}{
		"a behaviour that is not one": {newCard{Outcome: "nonsense"}, "unknown card behaviour"},
		"a balance below zero":        {newCard{Outcome: "success", Balance: -1}, "balance"},
		"a stall that runs backwards": {newCard{Outcome: "success", DelayMs: -1}, "backwards"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := s.store.UpdateCard(ctx, card, tt.card)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expect)
		})
	}
}

// A card that is not there is not a card that was changed: acting on a row that
// has gone must be reported rather than counted as done.
func TestE2EStoreReportsACardThatIsNotThere(t *testing.T) {
	s := newStand(t)
	ctx := context.Background()

	assert.ErrorIs(t, s.store.SetCardBlocked(ctx, 999999, true), errNotFound)
	assert.ErrorIs(t, s.store.UpdateCard(ctx, 999999, newCard{Outcome: "success"}), errNotFound)
	assert.ErrorIs(t, s.store.DeleteCard(ctx, 999999), errNotFound)
}

// A number is sixteen digits and nothing else: a letter in the middle is not a
// digit that failed the check, it is not a number at all.
func TestLuhnRefusesWhatIsNotDigits(t *testing.T) {
	assert.False(t, luhnValid("86000691954063x1"))
	assert.True(t, luhnValid(uzcard))
}

// An expiry is a month that exists. The year is not checked against today: a
// card that expired last year is exactly what someone rehearsing a refusal
// wants to enter.
func TestExpiryIsAMonthThatExists(t *testing.T) {
	assert.True(t, validExpire("0399"))
	assert.True(t, validExpire("1226"))
	assert.False(t, validExpire("0026"), "there is no month zero")
	assert.False(t, validExpire("1326"), "there is no thirteenth month")
	assert.False(t, validExpire("039"))
	assert.False(t, validExpire("ab99"))
}

// firstError pulls the message a screen is showing out of the page, so a failed
// assertion reads as the refusal rather than as a wall of markup.
func firstError(body string) string {
	start := strings.Index(body, `class="err"`)
	if start < 0 {
		return "no message on the page"
	}

	rest := body[start:]
	end := strings.Index(rest, "</div>")
	if end < 0 {
		end = len(rest)
	}

	return rest[:end]
}
