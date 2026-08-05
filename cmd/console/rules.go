package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	faultdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/fault/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
)

// ruleRow is a fault rule as the rules screen shows it.
type ruleRow struct {
	ID       int64
	Name     string
	Sandbox  string
	Service  string
	Method   string
	Action   string
	Enabled  bool
	Priority int
	// Detail spells out what the action does, so the list reads without
	// opening anything: the error returned, the delay held, the status sent.
	Detail   string
	HitCount int64
	// The fields the edit dialog opens on: what the rule returns now, and the
	// numbers behind it in the units the form asks for.
	Outcome      string
	DelaySeconds string
	Percent      string
	TimesLeft    string
}

// errorRow is one entry of the error catalog.
type errorRow struct {
	Code    int
	Slug    string
	Scope   string
	Message string
	Builtin bool
}

// newRule is what the create form submits.
type newRule struct {
	SandboxID *int64
	// Service is the half of the protocol the method belongs to, which is what
	// keeps a Subscribe rule from firing on a Merchant call of the same name.
	Service string
	Method  string
	Action  faultdomain.Action
	// DelaySeconds is what the operator types for a delay or a timeout; the
	// form asks in seconds because that is how a stalled provider is described.
	DelaySeconds float64
	ErrorCode    payerr.Code
	HTTPStatus   int
	Probability  float64
	TimesLeft    *int
}

// SeedErrorCatalog inserts every documented error, skipping those already
// there, and reports how many it created.
//
// The catalog lives in Go because the protocol's codes and their three
// translations are part of the implementation; the table exists so the console
// can show them and an operator can add their own.
func (s *store) SeedErrorCatalog(ctx context.Context) (int, error) {
	var created int

	for _, entry := range payerr.Catalog {
		// Messages are plain structs, so encoding cannot fail.
		message, _ := json.Marshal(entry.Message)

		var code int
		err := s.pool.QueryRow(ctx, `
			INSERT INTO control.error_catalog (code, slug, scope, message, data_field, builtin)
			VALUES ($1, $2, $3, $4, $5, TRUE)
			ON CONFLICT (code) DO NOTHING
			RETURNING code`,
			int(entry.Code), slugFor(entry.Code), scopeFor(entry.Code), message,
			nullableText(entry.Data)).Scan(&code)

		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return created, fmt.Errorf("seed error %d: %w", entry.Code, err)
		}
		created++
	}

	return created, nil
}

// Errors lists the catalog, lowest code first.
func (s *store) Errors(ctx context.Context) ([]errorRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT code, slug, scope, message, builtin
		FROM control.error_catalog
		ORDER BY code DESC`)
	if err != nil {
		return nil, fmt.Errorf("select error catalog: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (errorRow, error) {
		var (
			out errorRow
			raw []byte
		)
		if err := row.Scan(&out.Code, &out.Slug, &out.Scope, &raw, &out.Builtin); err != nil {
			return errorRow{}, err
		}

		var message payerr.Message
		if err := json.Unmarshal(raw, &message); err != nil {
			return errorRow{}, fmt.Errorf("decode error %d: %w", out.Code, err)
		}
		out.Message = message.EN

		return out, nil
	})
}

// Rules lists the fault rules, in the order the engine evaluates them.
func (s *store) Rules(ctx context.Context, query string) ([]ruleRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.name, coalesce(s.slug, ''), r.service, r.method, r.action,
		       r.enabled, r.priority, r.delay_ms, r.error_code, r.http_status,
		       r.probability, r.times_left, r.hit_count
		FROM control.fault_rules r
		LEFT JOIN control.sandboxes s ON s.id = r.sandbox_id
		WHERE $1 = '' OR r.name ILIKE '%' || $1 || '%'
		   OR r.method ILIKE '%' || $1 || '%' OR s.slug ILIKE '%' || $1 || '%'
		   OR r.action ILIKE '%' || $1 || '%'
		ORDER BY r.priority, r.id`, query)
	if err != nil {
		return nil, fmt.Errorf("select fault rules: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ruleRow, error) {
		var (
			out         ruleRow
			delayMS     int
			errorCode   *int
			httpStatus  *int
			probability float64
			timesLeft   *int
		)

		if err := row.Scan(&out.ID, &out.Name, &out.Sandbox, &out.Service, &out.Method,
			&out.Action, &out.Enabled, &out.Priority, &delayMS, &errorCode,
			&httpStatus, &probability, &timesLeft, &out.HitCount); err != nil {
			return ruleRow{}, err
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
	})
}

// CreateRule stores a rule and returns its identifier.
//
// A method carries one rule at a time: the rule that replaces it says what the
// method returns now, and stacking a second one would leave the answer decided
// by evaluation order rather than by what was asked for last.
func (s *store) CreateRule(ctx context.Context, r newRule) (int64, error) {
	delayMS := int(r.DelaySeconds * 1000)

	var id int64

	err := postgres.WithTx(ctx, s.pool, func(inner context.Context) error {
		if _, err := postgres.From(inner, s.pool).Exec(inner, `
			DELETE FROM control.fault_rules
			WHERE config_id IS NULL AND service = $1 AND method = $2
			  AND sandbox_id IS NOT DISTINCT FROM $3`,
			r.Service, r.Method, r.SandboxID); err != nil {
			return fmt.Errorf("replace fault rule: %w", err)
		}

		return s.insertRule(inner, r, delayMS, &id)
	})
	if err != nil {
		return 0, err
	}

	return id, nil
}

// insertRule writes the row itself.
func (s *store) insertRule(ctx context.Context, r newRule, delayMS int, id *int64) error {
	err := postgres.From(ctx, s.pool).QueryRow(ctx, `
		INSERT INTO control.fault_rules
			(sandbox_id, name, service, method, action, delay_ms, error_code,
			 error_message, http_status, probability, times_left, priority)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 100)
		RETURNING id`,
		r.SandboxID, ruleName(r), r.Service, r.Method, string(r.Action), delayMS,
		nullableCode(r.ErrorCode), catalogMessage(r.ErrorCode),
		nullableInt(r.HTTPStatus), r.Probability, r.TimesLeft).Scan(id)
	if err != nil {
		return fmt.Errorf("insert fault rule: %w", err)
	}

	return nil
}

// ToggleRule enables or disables a rule, which is how a scenario is switched
// off without losing how it was set up.
func (s *store) ToggleRule(ctx context.Context, id int64) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE control.fault_rules SET enabled = NOT enabled, updated_at = now() WHERE id = $1`,
		id); err != nil {
		return fmt.Errorf("toggle fault rule: %w", err)
	}

	return nil
}

// DeleteRule removes a rule.
func (s *store) DeleteRule(ctx context.Context, id int64) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM control.fault_rules WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete fault rule: %w", err)
	}

	return nil
}

// catalogMessage is the three-language text a rule answers with. The rule
// carries its own copy so editing the catalog later cannot silently change
// what a running scenario returns.
func catalogMessage(code payerr.Code) []byte {
	entry, ok := payerr.ByCode(code)
	if !ok {
		return []byte(`{}`)
	}

	raw, _ := json.Marshal(entry.Message)

	return raw
}

// ruleName is what the rules list calls a rule when the operator did not name
// it, which the console's one-step form never asks for.
func ruleName(r newRule) string {
	method := r.Method
	if method == "*" {
		method = "every method"
	}

	switch r.Action {
	case faultdomain.ActionRPCError:
		return fmt.Sprintf("%s returns %d", method, r.ErrorCode)
	case faultdomain.ActionDelay:
		return fmt.Sprintf("%s waits %gs", method, r.DelaySeconds)
	case faultdomain.ActionHTTPStatus:
		return fmt.Sprintf("%s answers HTTP %d", method, r.HTTPStatus)
	case faultdomain.ActionDrop:
		return fmt.Sprintf("%s times out", method)
	case faultdomain.ActionMalformed:
		return fmt.Sprintf("%s returns broken JSON", method)
	case faultdomain.ActionDuplicate:
		return fmt.Sprintf("%s is delivered twice", method)
	default:
		return method
	}
}

// describeAction spells out a rule for the list.
func describeAction(action string, delayMS int, errorCode, httpStatus *int, probability float64, timesLeft *int) string {
	var detail string

	switch faultdomain.Action(action) {
	case faultdomain.ActionRPCError:
		detail = fmt.Sprintf("returns error %d", derefInt(errorCode))
	case faultdomain.ActionDelay:
		detail = fmt.Sprintf("waits %.3gs", float64(delayMS)/1000)
	case faultdomain.ActionHTTPStatus:
		detail = fmt.Sprintf("answers HTTP %d", derefInt(httpStatus))
	case faultdomain.ActionDrop:
		detail = "closes the connection"
	case faultdomain.ActionMalformed:
		detail = "returns broken JSON"
	case faultdomain.ActionDuplicate:
		detail = "delivers the request twice"
	default:
		detail = "lets the request through"
	}

	if delayMS > 0 && faultdomain.Action(action) != faultdomain.ActionDelay {
		detail += fmt.Sprintf(", after %.3gs", float64(delayMS)/1000)
	}
	if probability < 1 {
		detail += fmt.Sprintf(", %.0f%% of the time", probability*100)
	}
	if timesLeft != nil {
		detail += fmt.Sprintf(", %d use(s) left", *timesLeft)
	}

	return detail
}

// outcomeOf names what a stored rule returns, in the words the form offers.
func outcomeOf(action string, errorCode *int) string {
	switch faultdomain.Action(action) {
	case faultdomain.ActionDrop:
		return outcomeTimeout
	case faultdomain.ActionRPCError:
		if errorCode != nil {
			return fmt.Sprint(*errorCode)
		}
		return outcomeSuccess
	default:
		return outcomeSuccess
	}
}

// trimZero formats a number without a trailing ".0", so a form shows "2"
// rather than "2.000000".
func trimZero(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// slugFor names an error for the catalog. The slug is what an operator types
// and a script matches on, so it is derived from the code rather than the
// Russian text.
func slugFor(code payerr.Code) string {
	if known, ok := errorSlugs[code]; ok {
		return known
	}
	return fmt.Sprintf("error_%d", -int(code))
}

// errorSlugs names the documented errors.
var errorSlugs = map[payerr.Code]string{
	payerr.CodeParse:             "parse_error",
	payerr.CodeInvalidRequest:    "invalid_request",
	payerr.CodeMethodNotFound:    "method_not_found",
	payerr.CodeUnauthorized:      "unauthorized",
	payerr.CodeTransport:         "transport_error",
	payerr.CodeInvalidHTTP:       "invalid_http_method",
	payerr.CodeInvalidAmount:     "invalid_amount",
	payerr.CodeTransactionNotFnd: "transaction_not_found",
	payerr.CodeOrderCompleted:    "order_completed",
	payerr.CodeCannotPerform:     "cannot_perform",
	payerr.CodeAccountMax:        "account_not_found",
}

// scopeFor says which half of the protocol an error belongs to, which is what
// the catalog's scope column records.
func scopeFor(code payerr.Code) string {
	switch {
	case code.IsAccountCode():
		return "merchant"
	case code <= payerr.CodeInvalidAmount && code >= payerr.CodeCannotPerform:
		return "merchant"
	default:
		return "general"
	}
}
