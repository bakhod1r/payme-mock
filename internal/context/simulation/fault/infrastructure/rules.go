// Package infrastructure implements the fault rule store against PostgreSQL.
package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/bakhod1r/payme-mock/internal/context/simulation/fault/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
)

// RuleStore provides the rules in force for a request and records their use.
type RuleStore struct {
	pool *postgres.Pool
}

// NewRuleStore wires the store to a pool.
func NewRuleStore(pool *postgres.Pool) *RuleStore {
	return &RuleStore{pool: pool}
}

// config_id is not selected: the store never serves a profile's rules, so a
// rule it returns always belongs to a sandbox or to every one of them.
const ruleColumns = `
	id, sandbox_id, name, enabled, priority, service, method,
	match_account, match_payme_id, amount_min, amount_max, action, delay_ms,
	error_code, error_message, error_data, http_status, probability, times_left,
	hit_count, note`

// Active returns the enabled rules that apply to a sandbox.
//
// A rule with no sandbox applies to every stand, which is how a global outage
// is simulated. A rule that belongs to a profile is left out: profiles are
// switched as a set from the console and are not what this store serves, so
// including them would apply every seeded scenario at once.
func (s *RuleStore) Active(ctx context.Context, sandboxID int64) ([]*domain.Rule, error) {
	rows, err := postgres.From(ctx, s.pool).Query(ctx, `
		SELECT `+ruleColumns+`
		FROM control.fault_rules
		WHERE enabled AND config_id IS NULL AND (sandbox_id = $1 OR sandbox_id IS NULL)
		ORDER BY priority, id`, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("select fault rules: %w", err)
	}

	// CollectRows drains, closes and reports a stream that failed partway, so
	// a connection lost mid-result cannot be mistaken for "nothing matched".
	out, err := pgx.CollectRows(rows, scanRule)
	if err != nil {
		return nil, fmt.Errorf("read fault rules: %w", err)
	}

	return out, nil
}

// Consume records one application of a rule, decrementing its remaining uses
// and disabling it once spent.
//
// The hit count is bumped in the same statement so a rule that fired is never
// counted without also being spent.
func (s *RuleStore) Consume(ctx context.Context, ruleID int64) error {
	tag, err := postgres.From(ctx, s.pool).Exec(ctx, `
		UPDATE control.fault_rules
		SET hit_count  = hit_count + 1,
		    times_left = CASE WHEN times_left IS NULL THEN NULL
		                      ELSE greatest(times_left - 1, 0) END,
		    enabled    = CASE WHEN times_left IS NOT NULL AND times_left <= 1
		                      THEN FALSE ELSE enabled END,
		    updated_at = now()
		WHERE id = $1`, ruleID)
	if err != nil {
		return fmt.Errorf("consume fault rule: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}

	return nil
}

// Hit records that a rule fired without spending one of its uses, which is
// what an unlimited rule needs so the console can still show its traffic.
func (s *RuleStore) Hit(ctx context.Context, ruleID int64) error {
	if _, err := postgres.From(ctx, s.pool).Exec(ctx, `
		UPDATE control.fault_rules SET hit_count = hit_count + 1 WHERE id = $1`,
		ruleID); err != nil {
		return fmt.Errorf("record fault rule hit: %w", err)
	}

	return nil
}

func scanRule(row pgx.CollectableRow) (*domain.Rule, error) {
	var (
		rule      domain.Rule
		service   string
		action    string
		account   []byte
		message   []byte
		paymeID   *string
		errorCode *int
		errorData *string
		httpCode  *int
	)

	err := row.Scan(&rule.ID, &rule.SandboxID, &rule.Name, &rule.Enabled,
		&rule.Priority, &service, &rule.Method, &account, &paymeID,
		&rule.AmountMin, &rule.AmountMax, &action, &rule.DelayMillis,
		&errorCode, &message, &errorData, &httpCode, &rule.Probability,
		&rule.TimesLeft, &rule.HitCount, &rule.Note)
	if err != nil {
		return nil, fmt.Errorf("scan fault rule: %w", err)
	}

	rule.Service = domain.Service(service)
	rule.Action = domain.Action(action)

	if paymeID != nil {
		rule.MatchPaymeID = *paymeID
	}
	if errorCode != nil {
		rule.ErrorCode = payerr.Code(*errorCode)
	}
	if errorData != nil {
		rule.ErrorData = *errorData
	}
	if httpCode != nil {
		rule.HTTPStatus = *httpCode
	}
	if len(account) > 0 {
		if err := json.Unmarshal(account, &rule.MatchAccount); err != nil {
			return nil, fmt.Errorf("decode rule account match: %w", err)
		}
	}
	if len(message) > 0 {
		if err := json.Unmarshal(message, &rule.ErrorMessage); err != nil {
			return nil, fmt.Errorf("decode rule message: %w", err)
		}
	}

	return &rule, nil
}
