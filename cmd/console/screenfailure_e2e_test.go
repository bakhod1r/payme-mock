package main

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

// What a screen does when the database is gone is not decoration: an operator
// acts on what the console shows them. A page that came back empty, or a form
// that redirected as though it had written something, would have them believe a
// stand is in a state nothing put it in.

// gone is a console whose database went away after the rows were made, which is
// every screen's worst case and the only one they all share.
func gone(t *testing.T) *stand {
	t.Helper()

	s := newStand(t)
	sandbox := s.newSandbox(t, "gone", "topup", 100000)
	s.riggedCard(t, sandbox, uzcard, "success")
	s.store.pool.Close()

	return s
}

// A screen that cannot read says so, in a page that still looks like the
// console: whoever is reading it has to be able to get to another screen.
func TestE2EEveryScreenSaysSoWhenTheDatabaseIsGone(t *testing.T) {
	s := gone(t)

	for _, path := range []string{
		"/", "/dashboard", "/sandboxes", "/sandboxes/1", "/cards", "/cards/1",
		"/payments", "/transactions", "/receipts", "/transactions/1", "/receipts/1",
		"/rules", "/rules/1", "/traffic", "/traffic/1", "/traffic/1/curl",
	} {
		t.Run(path, func(t *testing.T) {
			w := s.get(t, path)

			assert.Equal(t, http.StatusInternalServerError, w.Code,
				"a screen that cannot read must not answer as though it had")
		})
	}
}

// A form that could not write must not redirect as though it had: the operator
// would go back to a list expecting a row that is not there.
func TestE2EEveryFormSaysSoWhenTheDatabaseIsGone(t *testing.T) {
	s := gone(t)

	one := strconv.FormatInt(1, 10)

	forms := map[string]url.Values{
		"/sandboxes":                       {"slug": {"another"}, "name": {"another"}, "kind": {"topup"}},
		"/sandboxes/" + one:                {"name": {"renamed"}, "kind": {"topup"}},
		"/sandboxes/" + one + "/reset":     {},
		"/sandboxes/" + one + "/delete":    {},
		"/sandboxes/" + one + "/orders":    {"amount": {"1000"}},
		"/sandboxes/" + one + "/access":    {"cidr": {"10.0.0.0/8"}},
		"/orders/" + one + "/delete":       {},
		"/access/" + one + "/delete":       {},
		"/accounts/" + one:                 {"name": {"payer"}, "phone": {"998901234567"}},
		"/accounts/" + one + "/delete":     {},
		"/accounts/" + one + "/balance":    {"op": {"add"}, "amount": {"1000"}},
		"/accounts/" + one + "/block":      {"blocked": {"1"}},
		"/transactions/" + one:             {"state": {"2"}},
		"/transactions/" + one + "/delete": {},
		"/traffic/" + one + "/delete":      {},
		"/rules":                           {"service": {"merchant"}, "method": {"*"}, "action": {"delay"}, "delay": {"1"}},
		"/rules/" + one:                    {"service": {"merchant"}, "method": {"*"}, "action": {"delay"}, "delay": {"1"}},
		"/rules/" + one + "/toggle":        {},
		"/rules/" + one + "/delete":        {},
		"/cards":                           {"sandbox_id": {one}, "number": {humo}, "expire": {"0399"}, "outcome": {"success"}},
		"/cards/" + one:                    {"outcome": {"blocked"}, "balance": {"1"}},
		"/cards/" + one + "/delete":        {},
		"/cards/" + one + "/block":         {"blocked": {"1"}},
		"/cards/" + one + "/cashbox":       {"sandbox_id": {one}, "blocked": {"1"}},
		"/cards/seed":                      {"sandbox_id": {one}},
	}

	for path, form := range forms {
		t.Run(path, func(t *testing.T) {
			w := s.post(t, path, form)

			assert.NotEqual(t, http.StatusSeeOther, w.Code,
				"a write that could not happen must not look like one that did")
		})
	}
}
