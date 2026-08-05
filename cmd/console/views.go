package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/jackc/pgx/v5"

	billing "github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
)

// errNoAccount reports an edit aimed at a payer that is not there.
var errNoAccount = errors.New("no such payer")

// accountRow is a payer with the balance the cash register holds for them.
type accountRow struct {
	ID      int64
	Sandbox string
	Name    string
	Phone   string
	Login   string
	Balance int64
	Blocked bool
	Orders  int
}

// transactionRow is one payment with everything the protocol carried, so the
// details view needs no second query.
type transactionRow struct {
	ID        int64
	Sandbox   string
	PaymeID   string
	OrderID   string
	AccountID int64
	// Account is the `account` object as it arrived, which is what a support
	// question is usually about.
	Account string
	// AccountFields is that object read out field by field. A payment is
	// identified by what the account carried — a login, an order, a person's
	// name — and a screen that prints one line of JSON makes the reader do the
	// parsing themselves.
	AccountFields []accountField
	Amount        int64
	State         int
	// StateLabel is what the state means, since the protocol's numbers say
	// nothing on their own.
	StateLabel  string
	Reason      string
	PaymeTime   int64
	CreateTime  int64
	PerformTime int64
	CancelTime  int64
	Receivers   string
	CreatedAt   string
	UpdatedAt   string
	Failed      bool
	// Kind is which way this register moves money, and KindMeaning says it in
	// words. A payment reads differently on a top-up register than on a
	// dividend one, so the screen shows which it was.
	Kind        string
	KindMeaning string
	// MerchantName is the business behind this register, and From/To are where
	// the money went in the order it travelled.
	MerchantName string
	From         string
	To           string
	// The card behind the payment, and the receipt that links the two.
	//
	// A merchant transaction carries no card: the provider does not tell a
	// merchant which card paid, and the stand keeps that faithful. The link is
	// the receipt the Payme side drove this transaction from, which is why both
	// are shown here and either can be missing — a payment made straight
	// against the Merchant API never had a receipt at all.
	Card       string
	CardID     int64
	ReceiptID  string
	ReceiptRow int64
}

// The columns every transaction read selects, written once so the list and one
// row's own page cannot drift into showing different records.
var transactionColumns = `
	t.id, s.slug, s.kind, coalesce(s.merchant_name, ''), t.payme_id, t.order_id,
	t.account_id, t.account::text,
	t.amount, t.state, t.reason, t.payme_time, t.create_time,
	t.perform_time, t.cancel_time, coalesce(t.receivers::text, ''),
	` + stamp("t.created_at") + `, ` + stamp("t.updated_at") + `,
	coalesce(c.number_full, ''), coalesce(c.id, 0),
	coalesce(r.receipt_id, ''), coalesce(r.id, 0)`

// The joins a transaction read needs: its stand, the receipt that drove it if
// the payment came through the Subscribe API, and that receipt's card if the
// card still exists.
const transactionSource = `
	FROM merchant.transactions t
	JOIN control.sandboxes s ON s.id = t.sandbox_id
	LEFT JOIN mock.receipts r
	       ON r.sandbox_id = t.sandbox_id AND r.merchant_txn = t.payme_id
	LEFT JOIN mock.cards c ON c.id = r.card_id`

// scanTransaction reads one row of transactionColumns.
func scanTransaction(row pgx.Row) (transactionRow, error) {
	var (
		out     transactionRow
		orderID *int64
		reason  *int16
		card    string
	)

	if err := row.Scan(&out.ID, &out.Sandbox, &out.Kind, &out.MerchantName,
		&out.PaymeID, &orderID,
		&out.AccountID, &out.Account, &out.Amount, &out.State, &reason,
		&out.PaymeTime, &out.CreateTime, &out.PerformTime, &out.CancelTime,
		&out.Receivers, &out.CreatedAt, &out.UpdatedAt, &card, &out.CardID,
		&out.ReceiptID, &out.ReceiptRow); err != nil {
		return transactionRow{}, err
	}

	out.OrderID = "—"
	if orderID != nil {
		out.OrderID = fmt.Sprint(*orderID)
	}

	out.Reason = "—"
	if reason != nil {
		out.Reason = fmt.Sprint(*reason)
	}

	out.StateLabel = stateLabel(out.State)
	out.KindMeaning = billing.Kind(out.Kind).Describe()
	out.AccountFields = readAccount(out.Account)
	out.Failed = out.State < 0

	// A payment outlives the card that made it, so the number is only shown
	// while the card is there; masking a number that is gone would invent one.
	out.Card = "—"
	if card != "" {
		out.Card = maskNumber(card)
	}

	// Where the money went. A payment with no card behind it came from the
	// payer's own account on the merchant's books, which is what is named
	// instead — inventing a card would be worse than saying so.
	register := out.Sandbox
	if out.MerchantName != "" {
		register = out.MerchantName + " · " + out.Sandbox
	}

	payer := out.Card
	if card == "" {
		payer = fmt.Sprintf("payer #%d", out.AccountID)
	}

	out.From, out.To = payer, register
	if !billing.Kind(out.Kind).Inbound() {
		out.From, out.To = register, payer
	}

	return out, nil
}

// accountField is one entry of an `account` object, ready to be read.
type accountField struct {
	Name  string
	Value string
}

// readAccount turns the stored account object into fields, in a fixed order so
// two identical payments never read differently.
//
// A value that is not a string is printed as it arrived rather than dropped:
// the account is the caller's own data and the screen's job is to show it, not
// to judge it.
func readAccount(raw string) []accountField {
	var object map[string]any
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		return nil
	}

	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]accountField, 0, len(names))
	for _, name := range names {
		out = append(out, accountField{Name: name, Value: printValue(object[name])})
	}

	return out
}

// printValue renders one account value as text.
func printValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case nil:
		return "—"
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(encoded)
	}
}

// transactionFilter is what the filter form submits.
type transactionFilter struct {
	Sandbox string
	State   string
	Query   string
	// Sort is which end of the list is worth reading first. Newest is the
	// default because the screen is a tail of what just happened.
	Sort string
}

// empty reports a filter that narrows nothing, which is the only state the
// live panel is shown in.
func (f transactionFilter) empty() bool {
	return f.Sandbox == "" && f.State == "" && f.Query == ""
}

// The state filter offers two groupings that are not states: the payments that
// never finished, and the ones that came back an error. Those are the two
// questions anyone actually opens this screen with, and asking them by state
// number means knowing that -1 and -2 are both failures and that 1 is a
// payment still holding an order.
const (
	filterUnfinished = "live"
	filterFailed     = "failed"
)

// The orders the list may be read in.
const (
	sortNewest  = "newest"
	sortOldest  = "oldest"
	sortLargest = "largest"
)

// sortOption is one entry of the order dropdown.
type sortOption struct {
	Value string
	Label string
}

// listSorts are the orders a list may be read in.
func listSorts() []sortOption {
	return []sortOption{
		{Value: sortNewest, Label: "newest first"},
		{Value: sortOldest, Label: "oldest first"},
		{Value: sortLargest, Label: "largest first"},
	}
}

// stateOption is one entry of the state dropdowns.
type stateOption struct {
	Value  string
	Number int
	Label  string
	// Settable reports whether an operator may force a payment into it. Every
	// documented state can be forced; the list exists so the filter can also
	// offer groupings that are not states at all.
	Settable bool
}

// transactionStates are the states a payment may be in or be forced into,
// together with the two groupings the filter offers on top of them.
func transactionStates() []stateOption {
	return []stateOption{
		{Value: filterUnfinished, Label: "unfinished · created, never performed"},
		{Value: filterFailed, Label: "failed · came back an error"},
		{Value: "1", Number: 1, Label: "1 · created", Settable: true},
		{Value: "2", Number: 2, Label: "2 · performed", Settable: true},
		{Value: "-1", Number: -1, Label: "-1 · cancelled", Settable: true},
		{Value: "-2", Number: -2, Label: "-2 · cancelled after perform", Settable: true},
	}
}

// trafficDetail is one request with its headers and bodies, which is what
// makes the log worth reading: the call and the answer, side by side.
type trafficDetail struct {
	trafficRow
	ID           int64
	RequestBody  string
	ResponseBody string
	RemoteAddr   string
	Rule         string
	// RequestHeaders and ResponseHeaders are read out field by field, because
	// half of what goes wrong between two services is in them and one line of
	// JSON makes the reader do the parsing.
	RequestHeaders  []accountField
	ResponseHeaders []accountField
}

// trafficFilter is what the traffic screen's filter form submits.
type trafficFilter struct {
	Sandbox string
	Service string
	// Result narrows to the calls worth looking at: the ones that failed at
	// the transport, and the ones that answered a protocol error inside a 200.
	Result string
	Query  string
}

// Narrowed reports a filter that leaves something out. It is exported because
// the template asks it: a template can only call an exported method, and a
// screen has to say when it is showing less than everything.
func (f trafficFilter) Narrowed() bool {
	return f.Sandbox != "" || f.Service != "" || f.Result != "" || f.Query != ""
}

// The results the traffic filter offers. They are questions rather than
// columns: "what broke" is not one field of a request log.
const (
	trafficFailed = "failed"
	trafficErrors = "errors"
	trafficOK     = "ok"
)

// trafficResults are the results a call may be narrowed to.
func trafficResults() []sortOption {
	return []sortOption{
		{Value: trafficFailed, Label: "failed · no 200 came back"},
		{Value: trafficErrors, Label: "protocol error · a 200 carrying an error"},
		{Value: trafficOK, Label: "clean · answered without an error"},
	}
}

// trafficServices are the two sides of the stand a call can belong to.
func trafficServices() []string { return []string{"paymemock", "merchant"} }

// Accounts lists the payers of every stand with their balances.
func (s *store) Accounts(ctx context.Context) ([]accountRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, s.slug, a.name, coalesce(a.phone, ''), coalesce(a.login, ''),
		       a.balance, a.blocked,
		       (SELECT count(*) FROM merchant.orders o WHERE o.account_id = a.id)
		FROM merchant.accounts a
		JOIN control.sandboxes s ON s.id = a.sandbox_id
		ORDER BY s.slug, a.id`)
	if err != nil {
		return nil, fmt.Errorf("select accounts: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (accountRow, error) {
		var out accountRow
		err := row.Scan(&out.ID, &out.Sandbox, &out.Name, &out.Phone, &out.Login,
			&out.Balance, &out.Blocked, &out.Orders)
		return out, err
	})
}

// The ways the console changes a balance. Both are relative: an operator
// setting up a scenario says how much to move, and the starting figure is
// chosen once when the sandbox is created.
const (
	// OpAdd tops the register up.
	OpAdd = "add"
	// OpSubtract takes funds away, which is how a payout that should fail for
	// want of money is arranged.
	OpSubtract = "subtract"
)

// errNegativeBalance reports a subtraction larger than the balance. A cash
// register cannot hold less than nothing, so the edit is refused rather than
// silently clamped to zero.
var errNegativeBalance = errors.New("that would leave a negative balance")

// ChangeBalance applies one balance edit and returns the new figure.
//
// The arithmetic runs inside the statement so two operators topping the same
// payer up at once cannot read the same figure and overwrite each other.
func (s *store) ChangeBalance(ctx context.Context, accountID int64, op string, amount int64) (int64, error) {
	var expression string

	switch op {
	case OpAdd:
		expression = "balance + $1"
	case OpSubtract:
		expression = "balance - $1"
	default:
		return 0, fmt.Errorf("unknown balance operation %q", op)
	}

	var balance int64
	err := s.pool.QueryRow(ctx, `
		WITH before AS (
			SELECT id, sandbox_id, balance FROM merchant.accounts WHERE id = $2
		), moved AS (
			UPDATE merchant.accounts SET balance = `+expression+`
			WHERE id = $2
			RETURNING id, sandbox_id, balance
		), recorded AS (
			INSERT INTO merchant.balance_events
				(sandbox_id, account_id, source, delta, balance_before, balance_after, note)
			SELECT moved.sandbox_id, moved.id, 'console', moved.balance - before.balance,
			       before.balance, moved.balance, $3
			FROM moved JOIN before ON before.id = moved.id
			RETURNING balance_after
		)
		SELECT balance_after FROM recorded`, amount, accountID, "changed in the console").Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errNoAccount
	}
	if err != nil {
		return 0, fmt.Errorf("update balance: %w", err)
	}

	if balance < 0 {
		// The row is already written, so the refusal has to undo it rather
		// than simply reporting; the caller runs this inside no transaction.
		if _, undo := s.pool.Exec(ctx, `
			WITH restored AS (
				UPDATE merchant.accounts SET balance = balance + $1 WHERE id = $2
				RETURNING id
			)
			DELETE FROM merchant.balance_events
			WHERE id = (SELECT max(id) FROM merchant.balance_events
			            WHERE account_id = (SELECT id FROM restored))`,
			amount, accountID); undo != nil {
			return 0, fmt.Errorf("restore balance: %w", undo)
		}
		return 0, errNegativeBalance
	}

	return balance, nil
}

// SetBlocked blocks or unblocks a payer, which is the other half of the
// account state a merchant reports on.
func (s *store) SetBlocked(ctx context.Context, accountID int64, blocked bool) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE merchant.accounts SET blocked = $1 WHERE id = $2`, blocked, accountID); err != nil {
		return fmt.Errorf("update blocked: %w", err)
	}

	return nil
}

// Transactions lists payments, newest first, narrowed by the filter.
//
// Every condition is written so an empty filter field matches everything,
// which keeps one query serving both the full list and any narrowing of it.
func (s *store) Transactions(ctx context.Context, f transactionFilter, page pageRequest) ([]transactionRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+transactionColumns+transactionSource+`
		WHERE ($1 = '' OR s.slug = $1)
		  AND CASE
		        WHEN $2 = '' THEN TRUE
		        -- Created and never performed: the payment that is still
		        -- holding an order, which is what an operator is hunting.
		        WHEN $2 = '`+filterUnfinished+`' THEN t.state = 1
		        WHEN $2 = '`+filterFailed+`' THEN t.state < 0
		        ELSE t.state = nullif($2, '')::smallint
		      END
		  AND ($3 = '' OR t.payme_id ILIKE '%' || $3 || '%'
		       OR t.order_id::text = $3
		       -- The card and the receipt are searched too: someone chasing a
		       -- payment has the number or the receipt id in front of them.
		       OR c.number_full ILIKE '%' || $3 || '%'
		       OR r.receipt_id ILIKE '%' || $3 || '%')
		ORDER BY
		  CASE WHEN $5 = '`+sortLargest+`' THEN t.amount END DESC,
		  CASE WHEN $5 = '`+sortOldest+`' THEN t.id END ASC,
		  t.id DESC
		LIMIT $4 OFFSET $6`, f.Sandbox, f.State, f.Query, page.Limit, f.Sort, page.Offset)
	if err != nil {
		return nil, fmt.Errorf("select transactions: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (transactionRow, error) {
		return scanTransaction(row)
	})
}

// TrafficDetails returns the recent log with bodies attached.
func (s *store) TrafficDetails(ctx context.Context, f trafficFilter, page pageRequest) ([]trafficDetail, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT l.id, `+stamp("l.at")+`, coalesce(s.slug, '—'),
		       l.service, l.direction, coalesce(l.method, '—'), l.http_status,
		       l.duration_ms, l.error_code, coalesce(l.request_body::text, ''),
		       coalesce(l.response_body::text, ''), coalesce(l.remote_addr, ''),
		       coalesce(r.name, ''), coalesce(l.request_headers::text, ''),
		       coalesce(l.response_headers::text, '')
		FROM control.request_log l
		LEFT JOIN control.sandboxes s ON s.id = l.sandbox_id
		LEFT JOIN control.fault_rules r ON r.id = l.fault_rule_id
		WHERE ($1 = '' OR l.method ILIKE '%' || $1 || '%'
		   OR s.slug ILIKE '%' || $1 || '%' OR l.remote_addr ILIKE '%' || $1 || '%'
		   -- The bodies are searched too: an identifier someone is chasing is
		   -- in the payload, not in the columns around it.
		   OR l.request_body::text ILIKE '%' || $1 || '%'
		   OR l.response_body::text ILIKE '%' || $1 || '%')
		  AND ($3 = '' OR s.slug = $3)
		  AND ($4 = '' OR l.service = $4)
		  AND CASE
		        WHEN $5 = '' THEN TRUE
		        WHEN $5 = '`+trafficFailed+`' THEN l.http_status IS DISTINCT FROM 200
		        WHEN $5 = '`+trafficErrors+`' THEN l.error_code IS NOT NULL
		        WHEN $5 = '`+trafficOK+`' THEN l.http_status = 200 AND l.error_code IS NULL
		        ELSE TRUE
		      END
		ORDER BY l.at DESC, l.id DESC
		LIMIT $2 OFFSET $6`, f.Query, page.Limit, f.Sandbox, f.Service, f.Result, page.Offset)
	if err != nil {
		return nil, fmt.Errorf("select traffic: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (trafficDetail, error) {
		var (
			out             trafficDetail
			status          *int
			errorCode       *int
			requestHeaders  string
			responseHeaders string
		)

		if err := row.Scan(&out.ID, &out.At, &out.Sandbox, &out.Service, &out.Direction,
			&out.Method, &status, &out.DurationMS, &errorCode, &out.RequestBody,
			&out.ResponseBody, &out.RemoteAddr, &out.Rule,
			&requestHeaders, &responseHeaders); err != nil {
			return trafficDetail{}, err
		}

		// The headers are an object like an account, and they read the same
		// way: a column of names against a column of values.
		out.RequestHeaders = readAccount(requestHeaders)
		out.ResponseHeaders = readAccount(responseHeaders)

		out.Status = "—"
		if status != nil {
			out.Status = fmt.Sprint(*status)
			out.Failed = *status >= 400
		}
		if errorCode != nil {
			out.ErrorCode = fmt.Sprint(*errorCode)
			// A protocol error is reported inside a 200 response, so the row is
			// still a failure even when the status says otherwise.
			out.Failed = true
		}

		out.RequestBody = prettyJSON(out.RequestBody)
		out.ResponseBody = prettyJSON(out.ResponseBody)

		return out, nil
	})
}

// prettyJSON indents a recorded body for reading.
//
// A body that is not JSON is returned untouched: broken output is exactly what
// this stand produces on purpose, and reformatting would hide what was sent.
func prettyJSON(body string) string {
	if body == "" {
		return ""
	}

	var out bytes.Buffer
	if err := json.Indent(&out, []byte(body), "", "  "); err != nil {
		return body
	}

	// A body stored as a JSON string is a raw payload the column could not
	// hold; it reads better unquoted than as one escaped line.
	var unquoted string
	if err := json.Unmarshal(out.Bytes(), &unquoted); err == nil {
		return unquoted
	}

	return out.String()
}

// stateLabel names a transaction state. The protocol's numbers are what the
// wire carries; the words are what an operator reads.
func stateLabel(state int) string {
	switch state {
	case 1:
		return "created"
	case 2:
		return "performed"
	case -1:
		return "cancelled"
	case -2:
		return "cancelled after perform"
	default:
		return "unknown"
	}
}

// sortOrder reads which way a list should be read. Anything unrecognized is
// the default rather than a failure: a hand-edited URL is not worth an error
// screen, and the wrong order is visible at a glance.
func sortOrder(r *http.Request) string {
	switch value := r.URL.Query().Get("sort"); value {
	case sortOldest, sortLargest:
		return value
	default:
		return sortNewest
	}
}
