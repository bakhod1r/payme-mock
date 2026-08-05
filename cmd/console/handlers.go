package main

import (
	"errors"
	"net/http"
	"strconv"
)

// The console's edit and delete handlers all follow the same shape: read the
// row's id from the path, act, and either redirect on success or draw the page
// again with the reason it did not work.

func (a *app) editSandbox(w http.ResponseWriter, r *http.Request, user string) {
	id, ok := a.pathID(w, r, user, a.renderSandboxes)
	if !ok {
		return
	}

	var configID *int64
	if raw := r.PostFormValue("config_id"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			a.renderSandboxes(w, r, user, "Unknown profile.")
			return
		}
		configID = &parsed
	}

	name := r.PostFormValue("name")
	if name == "" {
		a.renderSandboxes(w, r, user, "A sandbox needs a name.")
		return
	}

	if err := a.store.UpdateSandbox(r.Context(), id, name, configID,
		r.PostFormValue("kind"), r.PostFormValue("merchant_group"),
		r.PostFormValue("merchant_name")); err != nil {
		a.finish(w, r, user, "/sandboxes", "update sandbox", err, a.renderSandboxes)
		return
	}

	a.log.Info("sandbox updated", "id", id, "by", user)
	done(w, r, backTo(r, "/sandboxes"), "Sandbox updated.")
}

func (a *app) resetSandbox(w http.ResponseWriter, r *http.Request, user string) {
	id, ok := a.pathID(w, r, user, a.renderSandboxes)
	if !ok {
		return
	}

	if err := a.store.ResetSandbox(r.Context(), id); err != nil {
		a.finish(w, r, user, "/sandboxes", "reset sandbox", err, a.renderSandboxes)
		return
	}

	a.log.Info("sandbox reset", "id", id, "by", user)
	done(w, r, backTo(r, "/sandboxes"), "Sandbox data cleared. Credentials kept.")
}

func (a *app) editAccount(w http.ResponseWriter, r *http.Request, user string) {
	id, ok := a.pathID(w, r, user, a.renderSandboxes)
	if !ok {
		return
	}

	name := r.PostFormValue("name")
	if name == "" {
		a.renderSandboxes(w, r, user, "A payer needs a name.")
		return
	}

	if err := a.store.UpdateAccount(r.Context(), id, name, r.PostFormValue("phone")); err != nil {
		a.finish(w, r, user, "/", "update payer", err, a.renderSandboxes)
		return
	}

	a.log.Info("payer updated", "id", id, "by", user)
	done(w, r, "/", "Payer updated.")
}

func (a *app) deleteAccount(w http.ResponseWriter, r *http.Request, user string) {
	id, ok := a.pathID(w, r, user, a.renderSandboxes)
	if !ok {
		return
	}

	if err := a.store.DeleteAccount(r.Context(), id); err != nil {
		a.finish(w, r, user, "/", "delete payer", err, a.renderSandboxes)
		return
	}

	a.log.Info("payer deleted", "id", id, "by", user)
	done(w, r, "/", "Payer deleted.")
}

// An order is made and unmade from the stand it belongs to. It has no screen
// of its own: nobody manages orders, they are only what a payment settles
// against, and a list of them answered a question no one was asking.

func (a *app) createOrder(w http.ResponseWriter, r *http.Request, user string) {
	id, ok := a.pathID(w, r, user, a.renderSandboxes)
	if !ok {
		return
	}

	back := "/sandboxes/" + strconv.FormatInt(id, 10)

	amount, err := strconv.ParseInt(r.PostFormValue("amount"), 10, 64)
	if err != nil || amount <= 0 {
		a.showSandboxWith(w, r, user, id, "Amount must be more than zero.")
		return
	}

	// The payer is the stand's own, so the form does not ask which: a stand
	// has one, and picking it from a list of one is a step for nothing.
	account, err := a.store.SandboxAccountID(r.Context(), id)
	if err != nil {
		a.showSandboxWith(w, r, user, id, "This stand has no payer to bill.")
		return
	}

	if err := a.store.CreateOrder(r.Context(), account, amount, r.PostFormValue("description")); err != nil {
		a.showSandboxWith(w, r, user, id, err.Error())
		return
	}

	a.log.Info("order created", "sandbox", id, "amount", amount, "by", user)
	done(w, r, back, "Order created.")
}

func (a *app) deleteOrder(w http.ResponseWriter, r *http.Request, user string) {
	id, ok := a.pathID(w, r, user, a.renderSandboxes)
	if !ok {
		return
	}

	if err := a.store.DeleteOrder(r.Context(), id); err != nil {
		a.finish(w, r, user, "/", "delete order", err, a.renderSandboxes)
		return
	}

	a.log.Info("order deleted", "id", id, "by", user)
	done(w, r, backTo(r, "/"), "Order deleted.")
}

func (a *app) editTransaction(w http.ResponseWriter, r *http.Request, user string) {
	id, ok := a.pathID(w, r, user, a.renderTransactions)
	if !ok {
		return
	}

	state, err := strconv.Atoi(r.PostFormValue("state"))
	if err != nil {
		a.renderTransactions(w, r, user, "Pick a state.")
		return
	}

	if err := a.store.UpdateTransactionState(r.Context(), id, state); err != nil {
		a.finish(w, r, user, "/payments", "update transaction", err, a.renderPayments)
		return
	}

	a.log.Info("transaction state forced", "id", id, "state", state, "by", user)
	done(w, r, backTo(r, "/payments"), "Transaction state changed.")
}

func (a *app) deleteTransaction(w http.ResponseWriter, r *http.Request, user string) {
	id, ok := a.pathID(w, r, user, a.renderTransactions)
	if !ok {
		return
	}

	if err := a.store.DeleteTransaction(r.Context(), id); err != nil {
		a.finish(w, r, user, "/payments", "delete transaction", err, a.renderPayments)
		return
	}

	a.log.Info("transaction deleted", "id", id, "by", user)
	done(w, r, "/payments", "Transaction deleted.")
}

func (a *app) editRule(w http.ResponseWriter, r *http.Request, user string) {
	id, ok := a.pathID(w, r, user, a.renderRules)
	if !ok {
		return
	}

	rule, err := parseRuleForm(r)
	if err != nil {
		a.renderRules(w, r, user, err.Error())
		return
	}

	if err := a.store.UpdateRule(r.Context(), id, rule); err != nil {
		a.finish(w, r, user, "/rules", "update rule", err, a.renderRules)
		return
	}

	a.log.Info("fault rule updated", "id", id, "by", user)
	done(w, r, backTo(r, "/rules"), "Rule updated.")
}

func (a *app) deleteTrafficEntry(w http.ResponseWriter, r *http.Request, user string) {
	id, ok := a.pathID(w, r, user, a.renderTraffic)
	if !ok {
		return
	}

	if err := a.store.DeleteTrafficEntry(r.Context(), id); err != nil {
		a.finish(w, r, user, "/traffic", "delete traffic entry", err, a.renderTraffic)
		return
	}

	done(w, r, "/traffic", "Entry deleted.")
}

// renderer draws a page again with a message on it.
type renderer func(w http.ResponseWriter, r *http.Request, user, message string)

// pathID reads the row's identifier and parses the form, drawing the page
// again when either is malformed.
func (a *app) pathID(w http.ResponseWriter, r *http.Request, user string, again renderer) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		again(w, r, user, "That row is gone.")
		return 0, false
	}

	if err := r.ParseForm(); err != nil {
		again(w, r, user, "Malformed form.")
		return 0, false
	}

	return id, true
}

// finish reports the outcome of an edit: a row that vanished is a sentence on
// the screen, anything else is a failure the operator cannot act on.
func (a *app) finish(w http.ResponseWriter, r *http.Request, user, _ string, what string, err error, again renderer) {
	if errors.Is(err, errNotFound) {
		again(w, r, user, "That row is gone.")
		return
	}

	a.fail(w, what, err)
}

func orZero(raw string) string {
	if raw == "" {
		return "0"
	}
	return raw
}

// showSandbox is a stand's own page: everything about it, and how its balance
// got to where it is.
func (a *app) showSandbox(w http.ResponseWriter, r *http.Request, user string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	a.showSandboxWith(w, r, user, id, "")
}

// showSandboxWith draws a stand's page, optionally with the reason an action
// taken from it did not work. The actions live on this page, so their failures
// belong on it too rather than on the list the operator did not come from.
func (a *app) showSandboxWith(w http.ResponseWriter, r *http.Request, user string, id int64, message string) {
	sandbox, err := a.store.SandboxByID(r.Context(), id, a.cfg.GatewayBaseURL)
	if err != nil {
		if errors.Is(err, errNotFound) {
			http.NotFound(w, r)
			return
		}
		a.fail(w, "load sandbox", err)
		return
	}

	history, err := a.store.BalanceHistory(r.Context(), id, listLimit)
	if err != nil {
		a.fail(w, "load balance history", err)
		return
	}

	profiles, err := a.store.Profiles(r.Context())
	if err != nil {
		a.fail(w, "list profiles", err)
		return
	}

	rules, err := a.store.IPRules(r.Context(), id)
	if err != nil {
		a.fail(w, "list ip rules", err)
		return
	}

	orders, err := a.store.OrdersForSandbox(r.Context(), id, listLimit)
	if err != nil {
		a.fail(w, "list sandbox orders", err)
		return
	}

	a.render(w, "sandbox", view{
		Title: sandbox.Slug, Nav: "sandboxes", User: user, Notice: notice(r),
		Error: message, Sandbox: sandbox, History: history, Profiles: profiles,
		Kinds: registerKinds(), IPRules: rules, Orders: orders,
	})
}
