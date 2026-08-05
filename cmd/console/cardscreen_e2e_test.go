package main

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A rigged card is what a stand is for: the API can only tokenize a card that
// works, so every refusal an integration has to survive is put on the stand
// from here. These drive that screen the way an operator does.

const (
	uzcard = "8600069195406311"
	humo   = "9860123456789015"
)

// addCard puts a card on a stand through the form that adds one and returns its
// row identifier.
func (s *stand) riggedCard(t *testing.T, sandbox sandboxRow, number, outcome string) int64 {
	t.Helper()

	w := s.post(t, "/cards", url.Values{
		"sandbox_id":  {strconv.FormatInt(sandbox.ID, 10)},
		"number":      {number},
		"expire":      {"0399"},
		"outcome":     {outcome},
		"balance":     {"100000000"},
		"phone":       {"998901234567"},
		"verify":      {"1"},
		"recurrent":   {"1"},
		"sms_enabled": {"1"},
	})
	require.Equal(t, http.StatusSeeOther, w.Code, w.Body.String())

	var id int64
	require.NoError(t, s.store.pool.QueryRow(context.Background(),
		`SELECT id FROM mock.cards WHERE number_full = $1`, number).Scan(&id))

	return id
}

func TestE2ECardIsAddedRiggedAndShown(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "cards-add", "topup", 100000)

	id := s.riggedCard(t, sandbox, uzcard, "blocked")

	page := s.get(t, "/cards/"+strconv.FormatInt(id, 10))
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()

	assert.Contains(t, body, "860006******6311", "the number is masked the way the provider returns it")
	assert.Contains(t, body, "666666", "a card added here takes the stand's shared code")
	assert.Contains(t, body, "blocked")

	list := s.get(t, "/cards")
	assert.Contains(t, list.Body.String(), "860006******6311")
}

// Everything the form refuses, and why: a number no card carries, an expiry
// that spells no month, a stand that is not named, and a number the merchant
// already holds.
func TestE2ECardFormRefusesWhatIsNotACard(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "cards-refuse", "topup", 100000)
	stand := strconv.FormatInt(sandbox.ID, 10)

	tests := []struct {
		name   string
		form   url.Values
		expect string
	}{
		{
			name:   "a number that is not sixteen digits",
			form:   url.Values{"sandbox_id": {stand}, "number": {"8600"}, "expire": {"0399"}, "outcome": {"success"}},
			expect: "sixteen digits",
		},
		{
			name:   "an expiry that spells no month",
			form:   url.Values{"sandbox_id": {stand}, "number": {uzcard}, "expire": {"1399"}, "outcome": {"success"}},
			expect: "MMYY",
		},
		{
			name:   "no stand at all",
			form:   url.Values{"sandbox_id": {"0"}, "number": {uzcard}, "expire": {"0399"}, "outcome": {"success"}},
			expect: "pick the sandbox",
		},
		{
			name:   "a behaviour that is not one",
			form:   url.Values{"sandbox_id": {stand}, "number": {uzcard}, "expire": {"0399"}, "outcome": {"nonsense"}},
			expect: "unknown card behaviour",
		},
		{
			name: "a balance below zero",
			form: url.Values{"sandbox_id": {stand}, "number": {uzcard}, "expire": {"0399"},
				"outcome": {"success"}, "balance": {"-1"}},
			expect: "balance",
		},
		{
			name: "a number no card carries",
			form: url.Values{"sandbox_id": {stand}, "number": {"8600000000000001"}, "expire": {"0399"},
				"outcome": {"success"}},
			expect: "Luhn",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := s.post(t, "/cards", tt.form)

			require.Equal(t, http.StatusOK, w.Code, "the form comes back with the reason on it")
			assert.Contains(t, w.Body.String(), tt.expect)
		})
	}
}

// A card is one card: one balance, one behaviour, one verification. The same
// number added twice would split all three between two rows that stand for the
// same piece of plastic.
func TestE2ECardTheMerchantAlreadyHoldsIsRefused(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "cards-held", "topup", 100000)
	s.riggedCard(t, sandbox, uzcard, "success")

	w := s.post(t, "/cards", url.Values{
		"sandbox_id": {strconv.FormatInt(sandbox.ID, 10)},
		"number":     {uzcard},
		"expire":     {"0399"},
		"outcome":    {"success"},
	})

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "already holds that card")
}

// A form that cannot be read is the sender's mistake, not a card the operator
// typed wrong.
func TestE2ECardFormReportsAMalformedSubmission(t *testing.T) {
	s := newStand(t)

	w := s.postRaw(t, "/cards", "%zz")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Malformed form")
}

// What an operator may change about a card is what it does, not what it is: the
// number and the token stay, so a token an integration already stored keeps
// standing for this card.
func TestE2ECardBehaviourIsEdited(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "cards-edit", "topup", 100000)
	id := s.riggedCard(t, sandbox, uzcard, "success")
	card := strconv.FormatInt(id, 10)

	w := s.post(t, "/cards/"+card, url.Values{
		"outcome": {"insufficient_funds"},
		"balance": {"1"},
		"delay":   {"2.5"},
		"back":    {"/cards/" + card},
	})
	path, message := location(t, w)
	assert.Equal(t, "/cards/"+card, path)
	assert.Contains(t, message, "updated")

	page := s.get(t, "/cards/"+card)
	assert.Contains(t, page.Body.String(), "insufficient funds")
	assert.Contains(t, page.Body.String(), "2.5 seconds")
}

// A card an integration tokenized for itself is not the console's to rewrite:
// it can be stopped, the way a bank stops a real card, and nothing else.
func TestE2ECardOfTheRegisterIsNotTheConsolesToRewrite(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "cards-api", "topup", 100000)

	var id int64
	require.NoError(t, s.store.pool.QueryRow(context.Background(), `
		INSERT INTO mock.cards (sandbox_id, token, number_full, expire, verify, balance, source)
		VALUES ($1, 'api-token', $2, '03/99', TRUE, 1000, 'api')
		RETURNING id`, sandbox.ID, humo).Scan(&id))

	card := strconv.FormatInt(id, 10)

	edited := s.post(t, "/cards/"+card, url.Values{"outcome": {"blocked"}, "balance": {"1"}})
	require.Equal(t, http.StatusOK, edited.Code)
	assert.Contains(t, edited.Body.String(), "belongs to the register")

	deleted := s.post(t, "/cards/"+card+"/delete", nil)
	require.Equal(t, http.StatusOK, deleted.Code)
	assert.Contains(t, deleted.Body.String(), "belongs to the register")

	// Stopping it is allowed: that is what a bank does to a real card.
	blocked := s.post(t, "/cards/"+card+"/block", url.Values{"blocked": {"1"}})
	path, _ := location(t, blocked)
	assert.Equal(t, "/cards", path)
}

// A card the bank stopped refuses everything; releasing it puts it back to
// working, which is the pair an integration rehearses against.
func TestE2ECardIsStoppedAndReleased(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "cards-block", "topup", 100000)
	id := s.riggedCard(t, sandbox, uzcard, "success")
	card := strconv.FormatInt(id, 10)

	stopped := s.post(t, "/cards/"+card+"/block",
		url.Values{"blocked": {"1"}, "back": {"/cards/" + card}})
	path, message := location(t, stopped)
	assert.Equal(t, "/cards/"+card, path)
	assert.Contains(t, message, "blocked")

	page := s.get(t, "/cards/"+card)
	assert.Contains(t, page.Body.String(), "blocked")

	released := s.post(t, "/cards/"+card+"/block", url.Values{"back": {"/cards/" + card}})
	_, back := location(t, released)
	assert.Contains(t, back, "released")

	after := s.get(t, "/cards/"+card)
	assert.Contains(t, after.Body.String(), "success")
}

// Deleting a mock card is deleting it: a token an integration stored stops
// working, which is the point.
func TestE2ECardIsDeleted(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "cards-delete", "topup", 100000)
	id := s.riggedCard(t, sandbox, uzcard, "success")

	w := s.post(t, "/cards/"+strconv.FormatInt(id, 10)+"/delete", nil)
	path, message := location(t, w)

	assert.Equal(t, "/cards", path)
	assert.Contains(t, message, "deleted")

	var left int
	require.NoError(t, s.store.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM mock.cards WHERE id = $1`, id).Scan(&left))
	assert.Zero(t, left)
}

// A card belongs to the merchant, so every register of it can charge the card —
// unless the merchant stops taking it at one till, which is not the same as a
// bank blocking it everywhere.
func TestE2ECardIsTakenOffOneCashboxAndPutBack(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "cards-cashbox", "topup", 100000)
	id := s.riggedCard(t, sandbox, uzcard, "success")
	card := strconv.FormatInt(id, 10)

	off := s.post(t, "/cards/"+card+"/cashbox", url.Values{
		"sandbox_id": {strconv.FormatInt(sandbox.ID, 10)},
		"blocked":    {"1"},
		"back":       {"/cards/" + card},
	})
	path, message := location(t, off)
	assert.Equal(t, "/cards/"+card, path)
	assert.Contains(t, message, "off that cashbox")

	page := s.get(t, "/cards/"+card)
	assert.Contains(t, page.Body.String(), "off this cashbox")

	back := s.post(t, "/cards/"+card+"/cashbox", url.Values{
		"sandbox_id": {strconv.FormatInt(sandbox.ID, 10)},
		"back":       {"/cards/" + card},
	})
	_, restored := location(t, back)
	assert.Contains(t, restored, "taken back")
}

// A cashbox that is not a number is not a cashbox, and the screen says so
// rather than writing a row against nothing.
func TestE2ECardCashboxFormRefusesAnUnknownCashbox(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "cards-cashbox-bad", "topup", 100000)
	id := s.riggedCard(t, sandbox, uzcard, "success")

	w := s.post(t, "/cards/"+strconv.FormatInt(id, 10)+"/cashbox",
		url.Values{"sandbox_id": {"not a number"}})

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Unknown cashbox")
}

// The provider publishes seven numbers for its own sandbox, each rigged to one
// failure an integration has to survive. Adding them by hand is seven forms; the
// button is one.
func TestE2EProviderTestCardsAreSeeded(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "cards-seed", "topup", 100000)

	w := s.post(t, "/cards/seed", url.Values{"sandbox_id": {strconv.FormatInt(sandbox.ID, 10)}})
	path, message := location(t, w)

	assert.Equal(t, "/cards", path)
	assert.Contains(t, message, "Added")

	var count int
	require.NoError(t, s.store.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM mock.cards WHERE sandbox_id = $1`, sandbox.ID).Scan(&count))
	assert.Equal(t, len(paymeTestCards()), count)

	// A number the stand already has is left alone, so the button can be
	// pressed twice without splitting a card in two.
	again := s.post(t, "/cards/seed", url.Values{"sandbox_id": {strconv.FormatInt(sandbox.ID, 10)}})
	_, second := location(t, again)
	assert.Contains(t, second, "already has the whole set")

	require.NoError(t, s.store.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM mock.cards WHERE sandbox_id = $1`, sandbox.ID).Scan(&count))
	assert.Equal(t, len(paymeTestCards()), count, "no number was added twice")
}

// Seeding needs a stand to seed into, and says so rather than writing cards
// nobody can reach.
func TestE2ESeedingNeedsAStand(t *testing.T) {
	s := newStand(t)

	for _, tt := range []struct {
		form   url.Values
		expect string
	}{
		{url.Values{"sandbox_id": {"not a number"}}, "Pick the sandbox"},
		{url.Values{"sandbox_id": {"999999"}}, "sandbox"},
	} {
		w := s.post(t, "/cards/seed", tt.form)

		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), tt.expect)
	}
}

// The screen is split in two because the halves answer different questions:
// what was rigged, and what the register actually holds.
func TestE2ECardsScreenShowsBothHalves(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "cards-tabs", "topup", 100000)
	s.riggedCard(t, sandbox, uzcard, "success")

	_, err := s.store.pool.Exec(context.Background(), `
		INSERT INTO mock.cards (sandbox_id, token, number_full, expire, verify, balance, source, registered_at)
		VALUES ($1, 'held-token', $2, '12/26', TRUE, 1000, 'api', 1750000000000)`, sandbox.ID, humo)
	require.NoError(t, err)

	mocks := s.get(t, "/cards?tab=mocks")
	require.Equal(t, http.StatusOK, mocks.Code)
	assert.Contains(t, mocks.Body.String(), "860006******6311")

	cashbox := s.get(t, "/cards?tab=cashbox")
	require.Equal(t, http.StatusOK, cashbox.Code)
	assert.Contains(t, cashbox.Body.String(), "986012******9015")
}

// The filter is how a screen of a hundred cards answers "which of these
// refuses", so every way of narrowing it has to hold.
func TestE2ECardsScreenFilters(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "cards-filter", "topup", 100000)
	s.riggedCard(t, sandbox, uzcard, "blocked")
	s.riggedCard(t, sandbox, humo, "success")

	tests := []struct {
		query   string
		shows   string
		hides   string
		comment string
	}{
		{query: "?outcome=failing", shows: "860006", hides: "986012", comment: "anything that refuses"},
		{query: "?outcome=blocked", shows: "860006", hides: "986012", comment: "one behaviour"},
		{query: "?sandbox=cards-filter", shows: "860006", comment: "one stand"},
		{query: "?q=9860", shows: "986012", hides: "860006", comment: "a number"},
		{query: "?sort=largest", shows: "860006", comment: "the biggest balance first"},
		{query: "?sort=oldest", shows: "860006", comment: "the oldest first"},
	}

	for _, tt := range tests {
		t.Run(tt.comment, func(t *testing.T) {
			w := s.get(t, "/cards"+tt.query)

			require.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, w.Body.String(), tt.shows)
			if tt.hides != "" {
				assert.NotContains(t, w.Body.String(), tt.hides)
			}
		})
	}
}

// A card that is not there is not an error page: the address is wrong, which is
// what a bookmark to a deleted card is.
func TestE2ECardPageOfACardThatIsNotThere(t *testing.T) {
	s := newStand(t)

	assert.Equal(t, http.StatusNotFound, s.get(t, "/cards/999999").Code)
	assert.Equal(t, http.StatusNotFound, s.get(t, "/cards/not-a-number").Code)
}

// Acting on a card that is not there is refused the same way, whatever the
// action was.
func TestE2ECardActionsNeedACardThatExists(t *testing.T) {
	s := newStand(t)

	for _, path := range []string{
		"/cards/nope/delete", "/cards/nope/block", "/cards/nope", "/cards/nope/cashbox",
	} {
		w := s.post(t, path, url.Values{})

		assert.Equal(t, http.StatusOK, w.Code, path)
		assert.Contains(t, w.Body.String(), "That row is gone", path)
	}
}
