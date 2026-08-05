package main

import (
	"errors"
	"net/http"
	"strconv"
)

// Every list links out to one row's own page. The page carries the whole
// record and the actions that change it, so a table stays a table: something
// to scan, not a grid of buttons.

func (a *app) showTransaction(w http.ResponseWriter, r *http.Request, user string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	transaction, err := a.store.TransactionByID(r.Context(), id)
	if errors.Is(err, errNoRow) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, "load transaction", err)
		return
	}

	a.render(w, "transaction", view{
		Title: transaction.PaymeID, Nav: "transactions", User: user,
		Notice: notice(r), Transaction: transaction, States: transactionStates(),
	})
}

func (a *app) showRule(w http.ResponseWriter, r *http.Request, user string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	rule, err := a.store.RuleByID(r.Context(), id)
	if errors.Is(err, errNoRow) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, "load rule", err)
		return
	}

	catalog, err := a.store.Errors(r.Context())
	if err != nil {
		a.fail(w, "list errors", err)
		return
	}

	a.render(w, "rule", view{
		Title: rule.Method, Nav: "rules", User: user, Notice: notice(r),
		Rule: rule, Groups: buildMethodGroups(catalog),
	})
}

// showTrafficCurl answers one logged call as the command that would make it
// again, in plain text, so a button anywhere can copy it.
func (a *app) showTrafficCurl(w http.ResponseWriter, r *http.Request, _ string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	entry, err := a.store.TrafficByID(r.Context(), id)
	if errors.Is(err, errNoRow) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, "load traffic entry", err)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(curlOf(a.cfg.StandBaseURL, entry)))
}

func (a *app) showTrafficEntry(w http.ResponseWriter, r *http.Request, user string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	entry, err := a.store.TrafficByID(r.Context(), id)
	if errors.Is(err, errNoRow) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, "load traffic entry", err)
		return
	}

	a.render(w, "entry", view{
		Title: entry.Method, Nav: "traffic", User: user, Notice: notice(r),
		Entry: entry, Curl: curlOf(a.cfg.StandBaseURL, entry),
	})
}
