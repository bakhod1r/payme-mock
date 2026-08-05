package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// errNoRow reports a screen asked for a row that is not there, which is what a
// stale link or a browser back button produces after a delete.
var errNoRow = errors.New("no such row")

// A list says which rows exist; a row's own page is where it is read and acted
// on. These fetches back those pages, so a table never has to carry an edit
// form, a delete button and a details dialog for every line it draws.

// OrderByID returns one order.
func (s *store) OrderByID(ctx context.Context, id int64) (orderRow, error) {
	var out orderRow

	err := s.pool.QueryRow(ctx, `
		SELECT o.id, s.slug, o.account_id, a.name, o.amount, o.status,
		       o.description,
		       EXISTS (SELECT 1 FROM merchant.transactions t
		               WHERE t.order_id = o.id AND t.state IN (1, 2))
		FROM merchant.orders o
		JOIN control.sandboxes s ON s.id = o.sandbox_id
		JOIN merchant.accounts a ON a.id = o.account_id
		WHERE o.id = $1`, id).
		Scan(&out.ID, &out.Sandbox, &out.AccountID, &out.Payer, &out.Amount,
			&out.Status, &out.Description, &out.Locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return orderRow{}, errNoRow
	}
	if err != nil {
		return orderRow{}, fmt.Errorf("select order: %w", err)
	}

	return out, nil
}

// TransactionByID returns one payment with everything the protocol carried.
func (s *store) TransactionByID(ctx context.Context, id int64) (transactionRow, error) {
	out, err := scanTransaction(s.pool.QueryRow(ctx,
		`SELECT `+transactionColumns+transactionSource+` WHERE t.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return transactionRow{}, errNoRow
	}
	if err != nil {
		return transactionRow{}, fmt.Errorf("select transaction: %w", err)
	}

	return out, nil
}

// RuleByID returns one fault rule.
func (s *store) RuleByID(ctx context.Context, id int64) (ruleRow, error) {
	var (
		out         ruleRow
		delayMS     int
		errorCode   *int
		httpStatus  *int
		probability float64
		timesLeft   *int
	)

	err := s.pool.QueryRow(ctx, `
		SELECT r.id, r.name, coalesce(s.slug, ''), r.service, r.method, r.action,
		       r.enabled, r.priority, r.delay_ms, r.error_code, r.http_status,
		       r.probability, r.times_left, r.hit_count
		FROM control.fault_rules r
		LEFT JOIN control.sandboxes s ON s.id = r.sandbox_id
		WHERE r.id = $1`, id).
		Scan(&out.ID, &out.Name, &out.Sandbox, &out.Service, &out.Method,
			&out.Action, &out.Enabled, &out.Priority, &delayMS, &errorCode,
			&httpStatus, &probability, &timesLeft, &out.HitCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return ruleRow{}, errNoRow
	}
	if err != nil {
		return ruleRow{}, fmt.Errorf("select fault rule: %w", err)
	}

	if out.Sandbox == "" {
		out.Sandbox = "all"
	}
	out.Detail = describeAction(out.Action, delayMS, errorCode, httpStatus, probability, timesLeft)
	out.Outcome = outcomeOf(out.Action, errorCode)
	out.DelaySeconds = trimZero(float64(delayMS) / 1000)
	out.Percent = trimZero(probability * 100)
	if timesLeft != nil {
		out.TimesLeft = fmt.Sprint(*timesLeft)
	}

	return out, nil
}

// TrafficByID returns one logged request with its bodies.
func (s *store) TrafficByID(ctx context.Context, id int64) (trafficDetail, error) {
	var (
		out             trafficDetail
		status          *int
		errorCode       *int
		requestHeaders  string
		responseHeaders string
	)

	err := s.pool.QueryRow(ctx, `
		SELECT l.id, `+stamp("l.at")+`, coalesce(s.slug, '—'),
		       l.service, l.direction, coalesce(l.method, '—'), l.http_status,
		       l.duration_ms, l.error_code, coalesce(l.request_body::text, ''),
		       coalesce(l.response_body::text, ''), coalesce(l.remote_addr, ''),
		       coalesce(r.name, ''), coalesce(l.request_headers::text, ''),
		       coalesce(l.response_headers::text, '')
		FROM control.request_log l
		LEFT JOIN control.sandboxes s ON s.id = l.sandbox_id
		LEFT JOIN control.fault_rules r ON r.id = l.fault_rule_id
		WHERE l.id = $1`, id).
		Scan(&out.ID, &out.At, &out.Sandbox, &out.Service, &out.Direction,
			&out.Method, &status, &out.DurationMS, &errorCode, &out.RequestBody,
			&out.ResponseBody, &out.RemoteAddr, &out.Rule,
			&requestHeaders, &responseHeaders)
	if errors.Is(err, pgx.ErrNoRows) {
		return trafficDetail{}, errNoRow
	}
	if err != nil {
		return trafficDetail{}, fmt.Errorf("select traffic entry: %w", err)
	}

	// The headers read the same way an account does: names against values.
	out.RequestHeaders = readAccount(requestHeaders)
	out.ResponseHeaders = readAccount(responseHeaders)

	out.Status = "—"
	if status != nil {
		out.Status = fmt.Sprint(*status)
		out.Failed = *status >= 400
	}
	if errorCode != nil {
		out.ErrorCode = fmt.Sprint(*errorCode)
		out.Failed = true
	}

	out.RequestBody = prettyJSON(out.RequestBody)
	out.ResponseBody = prettyJSON(out.ResponseBody)

	return out, nil
}

// AccountByID returns one payer, which is what the balance form on a stand's
// page needs when the list is not on screen.
func (s *store) AccountByID(ctx context.Context, id int64) (accountRow, error) {
	var out accountRow

	err := s.pool.QueryRow(ctx, `
		SELECT a.id, s.slug, a.name, coalesce(a.phone, ''), coalesce(a.login, ''),
		       a.balance, a.blocked,
		       (SELECT count(*) FROM merchant.orders o WHERE o.account_id = a.id)
		FROM merchant.accounts a
		JOIN control.sandboxes s ON s.id = a.sandbox_id
		WHERE a.id = $1`, id).
		Scan(&out.ID, &out.Sandbox, &out.Name, &out.Phone, &out.Login,
			&out.Balance, &out.Blocked, &out.Orders)
	if errors.Is(err, pgx.ErrNoRows) {
		return accountRow{}, errNoRow
	}
	if err != nil {
		return accountRow{}, fmt.Errorf("select payer: %w", err)
	}

	return out, nil
}
