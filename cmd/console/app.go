package main

import (
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	billing "github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
	sandboxdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/sandbox/domain"
	"github.com/bakhod1r/payme-mock/web"
)

// localUser is who the console credits an action to when it came from this
// machine and no login was asked for.
const localUser = "localhost"

// listLimit caps the transaction screen, which is a recent view rather than an
// archive.
const listLimit = 200

// app is the console's HTTP layer.
type app struct {
	cfg       config
	store     *store
	sessions  *sessions
	log       *slog.Logger
	templates map[string]*template.Template
	now       func() time.Time
}

func newApp(cfg config, st *store, log *slog.Logger) (*app, error) {
	templates, err := parseTemplates()
	if err != nil {
		return nil, err
	}

	return &app{
		cfg:       cfg,
		store:     st,
		sessions:  newSessions(),
		log:       log,
		templates: templates,
		now:       time.Now,
	}, nil
}

// parseTemplates pairs each page with the shared layout, so a page is rendered
// by executing its own set rather than one tree where the last "content"
// definition would win.
func parseTemplates() (map[string]*template.Template, error) {
	return parseTemplatesFrom(web.Console)
}

// parseTemplatesFrom reads the pages out of a filesystem. The console reads its
// own, embedded ones; a test hands it a filesystem missing a page, which is the
// only way the failure below can be seen — and it is the failure that decides
// whether a console starts at all.
func parseTemplatesFrom(files fs.FS) (map[string]*template.Template, error) {
	pages := []string{"login", "sandboxes", "traffic", "rules", "transactions", "sandbox",
		"transaction", "rule", "entry", "cards", "card", "receipts", "receipt",
		"payments", "dashboard"}
	out := make(map[string]*template.Template, len(pages))

	for _, page := range pages {
		t, err := template.New(page).Funcs(templateFuncs()).
			ParseFS(files, "console/layout.html", "console/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("parse %s template: %w", page, err)
		}
		out[page] = t
	}

	return out, nil
}

// view is what every template receives.
type view struct {
	Title        string
	Nav          string
	User         string
	Error        string
	Notice       string
	Sandboxes    []sandboxRow
	Profiles     []profileRow
	Entries      []trafficDetail
	Rules        []ruleRow
	Errors       []errorRow
	Transactions []transactionRow
	Receipts     []receiptRow
	Payments     []paymentRow
	// Dashboard is the front screen's figures.
	Dashboard      dashboard
	Accounts       []accountRow
	Orders         []orderRow
	Cards          []cardRow
	IPRules        []ipRuleRow
	Tab            string
	MockCount      int
	CashboxCount   int
	Outcomes       []behaviourOption
	FilterOutcomes []behaviourOption
	Statuses       []string
	States         []stateOption
	Filter         transactionFilter
	// Search is what the list's own search box holds, shown back so a screen
	// never claims to be the whole list when it is not.
	Search string
	// The payments screen's own filter, which spans both sides of a payment.
	PaymentFilter paymentFilter
	// The traffic screen's own filter, and what it may be narrowed to.
	TrafficFilter trafficFilter
	Results       []sortOption
	Services      []string
	// Page describes the pager under a list that can outgrow a screen.
	Page       pageView
	Sorts      []sortOption
	CardFilter cardFilter
	// The live screen: what moved inside the window, and how wide it is.
	LiveTransactions []liveTransactionRow
	LiveCards        []liveCardRow
	Window           int
	Windows          []int
	// LiveOn reports whether the live panel is drawn at all. A filtered screen
	// hides it rather than showing an empty one, which would read as "nothing
	// is happening" when the truth is "you asked about something else".
	LiveOn  bool
	Kinds   []kindOption
	Sandbox sandboxRow
	Order   orderRow
	Card    cardRow
	// Activity is what a card has been put through, shown on its own page, and
	// Cashboxes are the registers it may be charged through.
	Activity    cardActivity
	Cashboxes   []cardCashbox
	Transaction transactionRow
	// The receipts screen has a filter and a state list of its own: a receipt's
	// states are the Subscribe API's, which share no numbers with a merchant
	// transaction's, and one shared field would let a screen offer the wrong
	// ones.
	ReceiptFilter receiptFilter
	ReceiptStates []stateOption
	Receipt       receiptRow
	Rule          ruleRow
	Entry         trafficDetail
	// Curl is the entry rendered as the command that would make the call
	// again. It is built in the handler rather than the template because the
	// address it went to is not a field of the log.
	Curl    string
	History []balanceEvent
	Groups  []methodGroup
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /login", a.showLogin)
	mux.HandleFunc("POST /login", a.doLogin)
	mux.HandleFunc("POST /logout", a.doLogout)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.Handle("GET /{$}", a.authenticated(a.showDashboard))
	mux.Handle("GET /dashboard", a.authenticated(a.showDashboard))
	mux.Handle("GET /sandboxes", a.authenticated(a.showSandboxes))
	mux.Handle("POST /sandboxes", a.authenticated(a.createSandbox))
	mux.Handle("GET /sandboxes/{id}", a.authenticated(a.showSandbox))
	mux.Handle("POST /sandboxes/{id}", a.authenticated(a.editSandbox))
	mux.Handle("POST /sandboxes/{id}/reset", a.authenticated(a.resetSandbox))
	mux.Handle("POST /sandboxes/{id}/delete", a.authenticated(a.deleteSandbox))
	mux.Handle("GET /traffic", a.authenticated(a.showTraffic))
	mux.Handle("GET /traffic/{id}", a.authenticated(a.showTrafficEntry))
	// The command on its own, so a list can offer it per row without loading
	// every body and every header of every row to build it.
	mux.Handle("GET /traffic/{id}/curl", a.authenticated(a.showTrafficCurl))
	mux.Handle("POST /traffic/{id}/delete", a.authenticated(a.deleteTrafficEntry))
	mux.Handle("GET /rules", a.authenticated(a.showRules))
	mux.Handle("GET /rules/{id}", a.authenticated(a.showRule))
	mux.Handle("POST /rules", a.authenticated(a.createRule))
	mux.Handle("POST /rules/{id}", a.authenticated(a.editRule))
	mux.Handle("POST /rules/{id}/toggle", a.authenticated(a.toggleRule))
	mux.Handle("POST /rules/{id}/delete", a.authenticated(a.deleteRule))
	mux.Handle("GET /receipts/{id}", a.authenticated(a.showReceipt))
	// One screen shows both sides of a payment. The old two addresses still
	// work — they are in links and bookmarks — and lead to it.
	mux.Handle("GET /payments", a.authenticated(a.showPayments))
	mux.Handle("GET /transactions", a.authenticated(a.showPayments))
	mux.Handle("GET /receipts", a.authenticated(a.showPayments))
	mux.Handle("GET /transactions/{id}", a.authenticated(a.showTransaction))
	mux.Handle("POST /transactions/{id}", a.authenticated(a.editTransaction))
	mux.Handle("POST /transactions/{id}/delete", a.authenticated(a.deleteTransaction))
	// Orders have no screen of their own. They are not a thing an operator
	// manages; they are what a payment settles against, so they live on the
	// stand that holds them and are made and unmade from there.
	mux.Handle("POST /sandboxes/{id}/orders", a.authenticated(a.createOrder))
	mux.Handle("POST /orders/{id}/delete", a.authenticated(a.deleteOrder))
	mux.Handle("GET /cards", a.authenticated(a.showCards))
	mux.Handle("GET /cards/{id}", a.authenticated(a.showCard))
	mux.Handle("POST /cards", a.authenticated(a.createCard))
	mux.Handle("POST /cards/{id}", a.authenticated(a.editCard))
	mux.Handle("POST /cards/{id}/delete", a.authenticated(a.deleteCard))
	mux.Handle("POST /cards/{id}/block", a.authenticated(a.blockCard))
	mux.Handle("POST /cards/{id}/cashbox", a.authenticated(a.setCardCashbox))
	mux.Handle("POST /cards/seed", a.authenticated(a.seedPaymeCards))
	mux.Handle("POST /sandboxes/{id}/access", a.authenticated(a.createIPRule))
	mux.Handle("POST /access/{id}/delete", a.authenticated(a.deleteIPRule))
	mux.Handle("POST /accounts/{id}", a.authenticated(a.editAccount))
	mux.Handle("POST /accounts/{id}/delete", a.authenticated(a.deleteAccount))
	mux.Handle("POST /accounts/{id}/balance", a.authenticated(a.setBalance))
	mux.Handle("POST /accounts/{id}/block", a.authenticated(a.setBlocked))

	return mux
}

// local reports the operator at this machine's keyboard, who is not asked to
// sign in. Anything arriving from elsewhere still is.
func (a *app) local(r *http.Request) bool {
	return a.cfg.OpenOnLoopback && peerIsLocal(r.RemoteAddr, a.cfg.TrustPrivateNetwork)
}

// authenticated sends anyone without a live session to the login screen.
func (a *app) authenticated(next func(http.ResponseWriter, *http.Request, string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A request from this machine is the operator at the keyboard, so the
		// local stand does not ask them to sign in. Anything off-box still does.
		if a.local(r) {
			next(w, r, localUser)
			return
		}

		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		user, ok := a.sessions.lookup(cookie.Value, a.now())
		if !ok {
			// The cookie outlived its session, so clear it rather than leave
			// the browser sending a token that will never work again.
			clearSessionCookie(w)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		next(w, r, user)
	})
}

func (a *app) showLogin(w http.ResponseWriter, r *http.Request) {
	// Nobody local needs the form; they are already through.
	if a.local(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// An operator who is already signed in has no reason to see the form.
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if _, ok := a.sessions.lookup(cookie.Value, a.now()); ok {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}

	a.render(w, "login", view{Title: "Sign in"})
}

func (a *app) doLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.renderStatus(w, http.StatusBadRequest, "login", view{Title: "Sign in", Error: "Malformed form."})
		return
	}

	user := r.PostFormValue("username")

	if !a.cfg.checkCredentials(user, r.PostFormValue("password")) {
		// The message names neither field: telling an attacker which half was
		// right would hand them a working username for free.
		a.log.Warn("rejected login", "user", user, "remote", r.RemoteAddr)
		a.renderStatus(w, http.StatusUnauthorized, "login",
			view{Title: "Sign in", Error: "Wrong username or password."})
		return
	}

	setSessionCookie(w, a.sessions.issue(user, a.now()))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *app) doLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		a.sessions.revoke(cookie.Value)
	}

	clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *app) showSandboxes(w http.ResponseWriter, r *http.Request, user string) {
	a.renderSandboxes(w, r, user, "")
}

// renderSandboxes draws the list, optionally with the error a failed create or
// delete produced.
func (a *app) renderSandboxes(w http.ResponseWriter, r *http.Request, user, message string) {
	query := searchQuery(r)

	sandboxes, err := a.store.searchSandboxes(r.Context(), a.cfg.GatewayBaseURL, query)
	if err != nil {
		a.fail(w, "list sandboxes", err)
		return
	}

	profiles, err := a.store.Profiles(r.Context())
	if err != nil {
		a.fail(w, "list profiles", err)
		return
	}

	a.render(w, "sandboxes", view{
		Title: "Sandboxes", Nav: "sandboxes", User: user, Error: message, Notice: notice(r),
		Sandboxes: sandboxes, Profiles: profiles, Kinds: registerKinds(),
		Search: query,
	})
}

func (a *app) createSandbox(w http.ResponseWriter, r *http.Request, user string) {
	if err := r.ParseForm(); err != nil {
		a.renderSandboxes(w, r, user, "Malformed form.")
		return
	}

	sandbox, err := sandboxdomain.New(r.PostFormValue("slug"), r.PostFormValue("name"), credentials{})
	if err != nil {
		// A rejected slug is the operator's mistake, so the domain's own
		// wording is shown rather than a generic failure.
		a.renderSandboxes(w, r, user, err.Error())
		return
	}

	kind := r.PostFormValue("kind")
	if !billing.Kind(kind).Valid() {
		a.renderSandboxes(w, r, user, "Pick which way the register moves money.")
		return
	}
	sandbox.Kind = kind
	sandbox.MerchantGroup = strings.TrimSpace(r.PostFormValue("merchant_group"))
	sandbox.MerchantName = strings.TrimSpace(r.PostFormValue("merchant_name"))

	var configID *int64
	if raw := r.PostFormValue("config_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			a.renderSandboxes(w, r, user, "Unknown profile.")
			return
		}
		configID = &id
	}

	balance := int64(0)
	if raw := r.PostFormValue("balance"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			a.renderSandboxes(w, r, user, "Balance must be a whole number of tiyin, zero or more.")
			return
		}
		balance = parsed
	}

	if err := a.store.CreateSandbox(r.Context(), sandbox, configID, balance); err != nil {
		if errors.Is(err, errSlugTaken) {
			a.renderSandboxes(w, r, user, err.Error())
			return
		}
		a.fail(w, "create sandbox", err)
		return
	}

	a.log.Info("sandbox created", "slug", sandbox.Slug, "balance", balance, "by", user)

	// Redirecting after the write keeps a refresh from creating a second one.
	done(w, r, "/sandboxes", "Sandbox "+sandbox.Slug+" created.")
}

func (a *app) deleteSandbox(w http.ResponseWriter, r *http.Request, user string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.renderSandboxes(w, r, user, "Unknown sandbox.")
		return
	}

	if err := a.store.DeleteSandbox(r.Context(), id); err != nil {
		a.fail(w, "delete sandbox", err)
		return
	}

	a.log.Info("sandbox deleted", "id", id, "by", user)
	done(w, r, "/sandboxes", "Sandbox deleted.")
}

func (a *app) showTraffic(w http.ResponseWriter, r *http.Request, user string) {
	a.renderTraffic(w, r, user, "")
}

func (a *app) renderTraffic(w http.ResponseWriter, r *http.Request, user, message string) {
	filter := trafficFilter{
		Sandbox: r.URL.Query().Get("sandbox"),
		Service: r.URL.Query().Get("service"),
		Result:  r.URL.Query().Get("result"),
		Query:   searchQuery(r),
	}

	page := pageOf(r)

	entries, err := a.store.TrafficDetails(r.Context(), filter, page)
	if err != nil {
		a.fail(w, "list traffic", err)
		return
	}

	entries, pager := paginate(entries, page, r)

	sandboxes, err := a.store.Sandboxes(r.Context(), a.cfg.GatewayBaseURL)
	if err != nil {
		a.fail(w, "list sandboxes", err)
		return
	}

	a.render(w, "traffic", view{
		Title: "Traffic", Nav: "traffic", User: user, Error: message, Notice: notice(r),
		Entries: entries, Search: filter.Query, TrafficFilter: filter,
		Results: trafficResults(), Services: trafficServices(),
		Sandboxes: sandboxes, Page: pager,
	})
}

func (a *app) showRules(w http.ResponseWriter, r *http.Request, user string) {
	a.renderRules(w, r, user, "")
}

// renderRules draws the list together with the form that creates one, so
// breaking a method is a single step from the screen that shows what is broken.
func (a *app) renderRules(w http.ResponseWriter, r *http.Request, user, message string) {
	query := searchQuery(r)

	rules, err := a.store.Rules(r.Context(), query)
	if err != nil {
		a.fail(w, "list rules", err)
		return
	}

	sandboxes, err := a.store.Sandboxes(r.Context(), a.cfg.GatewayBaseURL)
	if err != nil {
		a.fail(w, "list sandboxes", err)
		return
	}

	catalog, err := a.store.Errors(r.Context())
	if err != nil {
		a.fail(w, "list errors", err)
		return
	}

	a.render(w, "rules", view{
		Title: "Rules", Nav: "rules", User: user, Error: message, Notice: notice(r),
		Rules: rules, Sandboxes: sandboxes, Errors: catalog,
		Groups: buildMethodGroups(catalog), Search: query,
	})
}

func (a *app) createRule(w http.ResponseWriter, r *http.Request, user string) {
	if err := r.ParseForm(); err != nil {
		a.renderRules(w, r, user, "Malformed form.")
		return
	}

	rule, err := parseRuleForm(r)
	if err != nil {
		a.renderRules(w, r, user, err.Error())
		return
	}

	id, err := a.store.CreateRule(r.Context(), rule)
	if err != nil {
		a.fail(w, "create rule", err)
		return
	}

	a.log.Info("fault rule created", "id", id, "action", rule.Action, "by", user)

	// Redirecting after the write keeps a refresh from creating a second one.
	done(w, r, "/rules", "Rule saved. The next call to that method takes it.")
}

func (a *app) toggleRule(w http.ResponseWriter, r *http.Request, user string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.renderRules(w, r, user, "Unknown rule.")
		return
	}

	if err := a.store.ToggleRule(r.Context(), id); err != nil {
		a.fail(w, "toggle rule", err)
		return
	}

	done(w, r, backTo(r, "/rules"), "Rule switched.")
}

func (a *app) deleteRule(w http.ResponseWriter, r *http.Request, user string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.renderRules(w, r, user, "Unknown rule.")
		return
	}

	if err := a.store.DeleteRule(r.Context(), id); err != nil {
		a.fail(w, "delete rule", err)
		return
	}

	a.log.Info("fault rule deleted", "id", id, "by", user)
	done(w, r, "/rules", "Rule deleted.")
}

func (a *app) renderTransactions(w http.ResponseWriter, r *http.Request, user, message string) {
	filter := transactionFilter{
		Sandbox: r.URL.Query().Get("sandbox"),
		State:   r.URL.Query().Get("state"),
		Query:   strings.TrimSpace(r.URL.Query().Get("q")),
		Sort:    sortOrder(r),
	}

	page := pageOf(r)

	transactions, err := a.store.Transactions(r.Context(), filter, page)
	if err != nil {
		a.fail(w, "list transactions", err)
		return
	}

	transactions, pager := paginate(transactions, page, r)

	// A payment in progress is what someone is watching for, so it is above
	// the list rather than found by filtering it.
	//
	// A filtered screen shows no live panel: filtering is asking a question
	// about what has already happened, and a panel that kept moving under a
	// narrowed list would answer a different one. The filter is also how an
	// operator gets the live rows on their own — state=live is that question
	// asked properly.
	minutes := liveWindow(r)

	var live []liveTransactionRow

	if filter.empty() {
		live, err = a.store.LiveTransactions(r.Context(), filter.Sandbox, minutes, listLimit)
		if err != nil {
			a.fail(w, "list live transactions", err)
			return
		}
	}

	sandboxes, err := a.store.Sandboxes(r.Context(), a.cfg.GatewayBaseURL)
	if err != nil {
		a.fail(w, "list sandboxes", err)
		return
	}

	a.render(w, "transactions", view{
		Title: "Transactions", Nav: "transactions", User: user, Error: message, Notice: notice(r),
		Transactions: transactions, Sandboxes: sandboxes,
		States: transactionStates(), Filter: filter, Sorts: listSorts(),
		LiveTransactions: live, Window: minutes, Windows: windowChoices(),
		LiveOn: filter.empty(), Page: pager,
	})
}

func (a *app) setBalance(w http.ResponseWriter, r *http.Request, user string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.renderSandboxes(w, r, user, "Unknown payer.")
		return
	}

	if err := r.ParseForm(); err != nil {
		a.renderSandboxes(w, r, user, "Malformed form.")
		return
	}

	amount, err := strconv.ParseInt(r.PostFormValue("amount"), 10, 64)
	if err != nil {
		a.renderSandboxes(w, r, user, "Amount must be a whole number of tiyin.")
		return
	}

	// The sign carries the direction: one field says both how much and which
	// way, so there is no second control to disagree with it.
	op := OpAdd
	if amount < 0 {
		op, amount = OpSubtract, -amount
	}

	balance, err := a.store.ChangeBalance(r.Context(), id, op, amount)
	if err != nil {
		switch {
		case errors.Is(err, errNoAccount):
			a.renderSandboxes(w, r, user, "That payer is gone.")
		case errors.Is(err, errNegativeBalance):
			a.renderSandboxes(w, r, user, "That would leave a negative balance.")
		default:
			a.fail(w, "change balance", err)
		}
		return
	}

	a.log.Info("balance changed", "account", id, "op", op, "amount", amount,
		"balance", balance, "by", user)
	done(w, r, backTo(r, "/sandboxes"), "Balance is now "+formatSum(balance)+".")
}

func (a *app) setBlocked(w http.ResponseWriter, r *http.Request, user string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.renderSandboxes(w, r, user, "Unknown payer.")
		return
	}

	if err := r.ParseForm(); err != nil {
		a.renderSandboxes(w, r, user, "Malformed form.")
		return
	}

	blocked := r.PostFormValue("blocked") == "true"

	if err := a.store.SetBlocked(r.Context(), id, blocked); err != nil {
		a.fail(w, "set blocked", err)
		return
	}

	a.log.Info("payer state changed", "account", id, "blocked", blocked, "by", user)
	done(w, r, backTo(r, "/sandboxes"), map[bool]string{true: "Payer blocked.", false: "Payer unblocked."}[blocked])
}

// done redirects after a write and carries one line saying what happened. A
// page that only reloads looks identical whether the write landed or not, so
// the message is what tells an operator to stop clicking.
func done(w http.ResponseWriter, r *http.Request, path, message string) {
	http.Redirect(w, r, path+"?ok="+url.QueryEscape(message), http.StatusSeeOther)
}

// backTo is where a write returns to. The same actions are offered on the list
// and on one stand's own page, and an operator who acted from the second should
// land back on it rather than on the list.
func backTo(r *http.Request, fallback string) string {
	// Only a path is accepted: a full URL from a form would let a crafted page
	// bounce an authenticated operator somewhere else.
	if back := r.PostFormValue("back"); strings.HasPrefix(back, "/") &&
		!strings.HasPrefix(back, "//") {
		return back
	}
	return fallback
}

// searchQuery is what the search box on a list submits. It is trimmed because
// a pasted identifier usually arrives with a space on one end, and a search
// that fails on that looks broken rather than empty.
func searchQuery(r *http.Request) string {
	return strings.TrimSpace(r.URL.Query().Get("q"))
}

// notice reads back what the last write said about itself.
func notice(r *http.Request) string {
	return r.URL.Query().Get("ok")
}

func (a *app) render(w http.ResponseWriter, page string, data view) {
	a.renderStatus(w, http.StatusOK, page, data)
}

func (a *app) renderStatus(w http.ResponseWriter, status int, page string, data view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	if err := a.templates[page].ExecuteTemplate(w, "layout", data); err != nil {
		// The status line is already sent, so the response cannot be turned
		// into an error; all that is left is to record it.
		a.log.Error("render failed", "page", page, "error", err)
	}
}

// fail logs the cause and shows the operator a plain message, since a database
// error is nothing they can act on.
func (a *app) fail(w http.ResponseWriter, what string, err error) {
	a.log.Error(what+" failed", "error", err)
	http.Error(w, "Something went wrong. Check the console logs.", http.StatusInternalServerError)
}
