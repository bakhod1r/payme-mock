package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	billing "github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
	configdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/config/domain"
	faultdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/fault/domain"
	sandboxdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/sandbox/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
)

// uniqueViolation is the SQLSTATE PostgreSQL raises when a unique index
// rejects a row; for the console it means the slug is taken.
const uniqueViolation = "23505"

// errSlugTaken reports a sandbox slug or cash register id already in use.
var errSlugTaken = errors.New("that slug is already taken")

// store is the console's read and write access to the control schema.
//
// The console is the only writer of these tables, so it queries them directly
// rather than going through a bounded context: there is no domain rule here
// beyond what the schema already enforces.
type store struct {
	pool *postgres.Pool
}

// sandboxRow is a sandbox as the list screen shows it, joined with the name of
// the profile it runs.
type sandboxRow struct {
	ID         int64
	Slug       string
	Name       string
	MerchantID string
	Key        string
	TestKey    string
	ConfigName string
	Endpoint   string
	// SubscribeURL is the other half of the address pair a merchant is given:
	// the endpoint their own backend calls.
	SubscribeURL string
	Kind         string
	KindMeaning  string
	// MerchantName is the organization a payer sees on the receipt, which is
	// what the provider reports in a receipt's merchant object.
	MerchantName string
	// MerchantGroup names the merchant this register belongs to. Registers
	// sharing a name share their cards, the way one merchant's cash registers
	// share the cards their customers saved.
	MerchantGroup string
	PayerName     string
	PayerPhone    string
	Transactions  int
	// AccountID and Balance are the stand's payer: one is created with every
	// sandbox, so the balance belongs on this screen rather than its own.
	AccountID int64
	Balance   int64
	Blocked   bool
	Orders    int
}

// profileRow is a profile with the counts the profiles screen shows.
type profileRow struct {
	ID          int64
	Name        string
	Description string
	Builtin     bool
	Rules       int
	Sandboxes   int
}

// trafficRow is one line of the request log.
type trafficRow struct {
	At         string
	Sandbox    string
	Service    string
	Direction  string
	Method     string
	Status     string
	Failed     bool
	DurationMS int
	ErrorCode  string
}

// SeedProfiles inserts the built-in profiles and their rules, skipping any
// that already exist, and reports how many profiles it created.
func (s *store) SeedProfiles(ctx context.Context) (int, error) {
	var created int

	for _, seed := range configdomain.SeedProfiles() {
		// Settings are plain structs, so encoding cannot fail.
		settings, _ := json.Marshal(seed.Profile.Settings)

		var id int64
		err := s.pool.QueryRow(ctx, `
			INSERT INTO control.configs (name, description, settings, builtin)
			VALUES ($1, $2, $3, TRUE)
			ON CONFLICT (name) DO NOTHING
			RETURNING id`,
			seed.Profile.Name, seed.Profile.Description, settings).Scan(&id)

		// No rows means the profile was already there, so its rules are too.
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return created, fmt.Errorf("seed profile %s: %w", seed.Profile.Name, err)
		}
		created++

		if err := s.seedRules(ctx, id, seed.Rules); err != nil {
			return created, err
		}
	}

	return created, nil
}

func (s *store) seedRules(ctx context.Context, configID int64, rules []*faultdomain.Rule) error {
	for _, rule := range rules {
		message, err := json.Marshal(rule.ErrorMessage)
		if err != nil {
			return fmt.Errorf("encode rule message: %w", err)
		}

		var account []byte
		if len(rule.MatchAccount) > 0 {
			account, _ = json.Marshal(rule.MatchAccount)
		}

		_, err = s.pool.Exec(ctx, `
			INSERT INTO control.fault_rules
				(config_id, name, enabled, priority, service, method, match_account,
				 match_payme_id, amount_min, amount_max, action, delay_ms,
				 error_code, error_message, error_data, http_status, probability,
				 times_left, note)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
			        $15, $16, $17, $18, $19)`,
			configID, rule.Name, rule.Enabled, rule.Priority, string(rule.Service),
			rule.Method, account, rule.MatchPaymeID, rule.AmountMin, rule.AmountMax,
			string(rule.Action), rule.DelayMillis, nullableCode(rule.ErrorCode),
			message, nullableText(rule.ErrorData), nullableInt(rule.HTTPStatus),
			rule.Probability, rule.TimesLeft, rule.Note,
		)
		if err != nil {
			return fmt.Errorf("seed rule %s: %w", rule.Name, err)
		}
	}

	return nil
}

// Sandboxes lists the live sandboxes, newest first.
func (s *store) Sandboxes(ctx context.Context, gatewayBase string) ([]sandboxRow, error) {
	return s.searchSandboxes(ctx, gatewayBase, "")
}

// searchSandboxes narrows the list to what a search names. The unfiltered form
// is what every dropdown on every other screen needs, so it stays the one the
// rest of the console calls.
func (s *store) searchSandboxes(ctx context.Context, gatewayBase, query string) ([]sandboxRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.id, s.slug, s.name, s.merchant_id, s.key, s.test_key, s.kind,
		       coalesce(s.merchant_group, ''), coalesce(s.merchant_name, ''),
		       coalesce(c.name, ''), coalesce(a.id, 0), coalesce(a.balance, 0),
		       coalesce(a.blocked, FALSE), coalesce(a.name, ''), coalesce(a.phone, ''),
		       (SELECT count(*) FROM merchant.orders o WHERE o.sandbox_id = s.id),
		       (SELECT count(*) FROM merchant.transactions t WHERE t.sandbox_id = s.id)
		FROM control.sandboxes s
		LEFT JOIN control.configs c ON c.id = s.active_config_id
		-- A stand has one payer; if an operator added more, the first is the
		-- one the balance column speaks for.
		LEFT JOIN LATERAL (
			SELECT id, balance, blocked, name, phone FROM merchant.accounts
			WHERE sandbox_id = s.id ORDER BY id LIMIT 1
		) a ON TRUE
		WHERE NOT s.archived
		  AND ($1 = '' OR s.slug ILIKE '%' || $1 || '%'
		       OR s.name ILIKE '%' || $1 || '%' OR s.merchant_id ILIKE '%' || $1 || '%'
		       OR s.merchant_group ILIKE '%' || $1 || '%'
		       OR s.merchant_name ILIKE '%' || $1 || '%')
		ORDER BY s.id DESC`, query)
	if err != nil {
		return nil, fmt.Errorf("select sandboxes: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (sandboxRow, error) {
		var out sandboxRow
		if err := row.Scan(&out.ID, &out.Slug, &out.Name, &out.MerchantID,
			&out.Key, &out.TestKey, &out.Kind, &out.MerchantGroup,
			&out.MerchantName, &out.ConfigName, &out.AccountID,
			&out.Balance, &out.Blocked, &out.PayerName, &out.PayerPhone,
			&out.Orders, &out.Transactions); err != nil {
			return sandboxRow{}, err
		}

		// EndpointURL is what the merchant pastes into their cash register, so
		// it is built by the domain rather than formatted here.
		out.KindMeaning = billing.Kind(out.Kind).Describe()

		sandbox := &sandboxdomain.Sandbox{Slug: out.Slug}
		out.Endpoint = sandbox.EndpointURL(gatewayBase)
		out.SubscribeURL = sandbox.SubscribeURL(gatewayBase)

		return out, nil
	})
}

// CreateSandbox stores a new sandbox together with the cash register balance
// it starts from.
//
// A stand with no payer cannot answer a single call, so the two are created in
// one transaction: a sandbox that exists without its payer would look ready and
// fail on the first request.
func (s *store) CreateSandbox(ctx context.Context, sb *sandboxdomain.Sandbox, configID *int64, balance int64) error {
	return postgres.WithTx(ctx, s.pool, func(inner context.Context) error {
		var sandboxID int64

		err := postgres.From(inner, s.pool).QueryRow(inner, `
			INSERT INTO control.sandboxes (slug, name, merchant_id, key, test_key,
			                               active_config_id, kind, merchant_group,
			                               merchant_name)
			VALUES ($1, $2, $3, $4, $5, $6, $7, nullif($8, ''), nullif($9, ''))
			RETURNING id`,
			sb.Slug, sb.Name, sb.MerchantID, sb.Key, sb.TestKey, configID, sb.Kind,
			sb.MerchantGroup, sb.MerchantName).Scan(&sandboxID)

		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return errSlugTaken
		}
		if err != nil {
			return fmt.Errorf("insert sandbox: %w", err)
		}

		sb.ID = sandboxID

		if _, err := postgres.From(inner, s.pool).Exec(inner, `
			INSERT INTO merchant.accounts (sandbox_id, phone, name, balance)
			VALUES ($1, $2, $3, $4)`,
			sandboxID, defaultPhone, sb.Name+" payer", balance); err != nil {
			return fmt.Errorf("insert payer: %w", err)
		}

		return nil
	})
}

// defaultPhone is the number a new stand's payer is reachable on. It is fixed
// so an integration can be pointed at a fresh sandbox without looking it up.
const defaultPhone = "901234567"

// DeleteSandbox removes a sandbox and everything that references it.
func (s *store) DeleteSandbox(ctx context.Context, id int64) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM control.sandboxes WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete sandbox: %w", err)
	}
	return nil
}

// Profiles lists the profiles with their rule and sandbox counts.
func (s *store) Profiles(ctx context.Context) ([]profileRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.name, c.description, c.builtin,
		       (SELECT count(*) FROM control.fault_rules r WHERE r.config_id = c.id),
		       (SELECT count(*) FROM control.sandboxes s WHERE s.active_config_id = c.id)
		FROM control.configs c
		ORDER BY c.builtin DESC, c.id`)
	if err != nil {
		return nil, fmt.Errorf("select profiles: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (profileRow, error) {
		var out profileRow
		err := row.Scan(&out.ID, &out.Name, &out.Description, &out.Builtin,
			&out.Rules, &out.Sandboxes)
		return out, err
	})
}

// Traffic returns the most recent request log entries.
func (s *store) Traffic(ctx context.Context, limit int) ([]trafficRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+stamp("l.at")+`, coalesce(s.slug, '—'),
		       l.service, l.direction, coalesce(l.method, '—'),
		       l.http_status, l.duration_ms, l.error_code
		FROM control.request_log l
		LEFT JOIN control.sandboxes s ON s.id = l.sandbox_id
		ORDER BY l.at DESC, l.id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("select traffic: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (trafficRow, error) {
		var (
			out       trafficRow
			status    *int
			errorCode *int
		)

		if err := row.Scan(&out.At, &out.Sandbox, &out.Service, &out.Direction,
			&out.Method, &status, &out.DurationMS, &errorCode); err != nil {
			return trafficRow{}, err
		}

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

		return out, nil
	})
}

func nullableCode(code payerr.Code) *int {
	if code == 0 {
		return nil
	}
	v := int(code)
	return &v
}

func nullableInt(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}

func nullableText(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
