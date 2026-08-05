package infrastructure_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/simulation/fault/domain"
	"github.com/bakhod1r/payme-mock/internal/context/simulation/fault/infrastructure"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres/testdb"
)

type stand struct {
	pool      *postgres.Pool
	store     *infrastructure.RuleStore
	ctx       context.Context
	sandboxID int64
}

func newStand(t *testing.T) *stand {
	t.Helper()

	pool := testdb.New(t)

	return &stand{
		pool:      pool,
		store:     infrastructure.NewRuleStore(pool),
		ctx:       context.Background(),
		sandboxID: testdb.Seed(t, pool, "qa"),
	}
}

// rule is what a test asks the stand to store.
type rule struct {
	sandboxID *int64
	configID  *int64
	method    string
	service   string
	action    string
	enabled   bool
	priority  int
	timesLeft *int
	paymeID   *string
	errorCode *int
}

func (s *stand) insert(t *testing.T, r rule) int64 {
	t.Helper()

	if r.service == "" {
		r.service = "merchant"
	}
	if r.method == "" {
		r.method = "*"
	}
	if r.action == "" {
		r.action = "rpc_error"
	}

	var id int64
	require.NoError(t, s.pool.QueryRow(s.ctx, `
		INSERT INTO control.fault_rules
			(sandbox_id, config_id, name, enabled, priority, service, method,
			 action, error_code, error_message, match_payme_id, times_left)
		VALUES ($1, $2, 'test rule', $3, $4, $5, $6, $7, $8, '{}'::jsonb, $9, $10)
		RETURNING id`,
		r.sandboxID, r.configID, r.enabled, r.priority, r.service, r.method,
		r.action, r.errorCode, r.paymeID, r.timesLeft).Scan(&id))

	return id
}

func TestE2EActiveReturnsTheStandsRules(t *testing.T) {
	s := newStand(t)
	id := s.insert(t, rule{sandboxID: &s.sandboxID, enabled: true, method: "CheckTransaction"})

	rules, err := s.store.Active(s.ctx, s.sandboxID)

	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, id, rules[0].ID)
	assert.Equal(t, "CheckTransaction", rules[0].Method)
	assert.Equal(t, domain.ServiceMerchant, rules[0].Service)
	assert.Equal(t, domain.ActionRPCError, rules[0].Action)
	assert.True(t, rules[0].Enabled)
	// The columns that may be NULL reach the domain as their zero values
	// rather than failing the scan.
	assert.Empty(t, rules[0].MatchPaymeID)
	assert.Zero(t, rules[0].ErrorCode)
	assert.Nil(t, rules[0].TimesLeft)
}

// A rule with no sandbox is how a global outage is simulated, so it applies
// to every stand.
func TestE2EActiveIncludesRulesWithoutASandbox(t *testing.T) {
	s := newStand(t)
	global := s.insert(t, rule{enabled: true})

	rules, err := s.store.Active(s.ctx, s.sandboxID)

	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, global, rules[0].ID)
}

// Another stand's rule must never fire here.
func TestE2EActiveSkipsAnotherStandsRules(t *testing.T) {
	s := newStand(t)
	other := testdb.Seed(t, s.pool, "other")
	s.insert(t, rule{sandboxID: &other, enabled: true})

	rules, err := s.store.Active(s.ctx, s.sandboxID)

	require.NoError(t, err)
	assert.Empty(t, rules)
}

func TestE2EActiveSkipsDisabledRules(t *testing.T) {
	s := newStand(t)
	s.insert(t, rule{sandboxID: &s.sandboxID, enabled: false})

	rules, err := s.store.Active(s.ctx, s.sandboxID)

	require.NoError(t, err)
	assert.Empty(t, rules)
}

// Profiles are switched as a set from the console; including their rules here
// would apply every seeded scenario at once.
func TestE2EActiveSkipsProfileRules(t *testing.T) {
	s := newStand(t)
	configID := s.configID(t)
	s.insert(t, rule{sandboxID: &s.sandboxID, configID: &configID, enabled: true})

	rules, err := s.store.Active(s.ctx, s.sandboxID)

	require.NoError(t, err)
	assert.Empty(t, rules)
}

func TestE2EActiveOrdersByPriority(t *testing.T) {
	s := newStand(t)
	second := s.insert(t, rule{sandboxID: &s.sandboxID, enabled: true, priority: 200})
	first := s.insert(t, rule{sandboxID: &s.sandboxID, enabled: true, priority: 10, method: "CreateTransaction"})

	rules, err := s.store.Active(s.ctx, s.sandboxID)

	require.NoError(t, err)
	require.Len(t, rules, 2)
	assert.Equal(t, first, rules[0].ID)
	assert.Equal(t, second, rules[1].ID)
}

// Every optional column round-trips, so a rule narrowed in the console behaves
// the same after a restart.
func TestE2EActiveReadsTheOptionalColumns(t *testing.T) {
	s := newStand(t)
	paymeID := "5305e3bab097f420a62ced0b"
	code := int(payerr.CodeCannotPerform)
	times := 3
	id := s.insert(t, rule{
		sandboxID: &s.sandboxID, enabled: true, paymeID: &paymeID,
		errorCode: &code, timesLeft: &times,
	})

	rules, err := s.store.Active(s.ctx, s.sandboxID)

	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, id, rules[0].ID)
	assert.Equal(t, paymeID, rules[0].MatchPaymeID)
	assert.Equal(t, payerr.CodeCannotPerform, rules[0].ErrorCode)
	require.NotNil(t, rules[0].TimesLeft)
	assert.Equal(t, times, *rules[0].TimesLeft)
}

// A rule that names the offending field and answers with a status round-trips
// too, so an HTTP-level failure survives a restart.
func TestE2EActiveReadsTheErrorDetailColumns(t *testing.T) {
	s := newStand(t)
	id := s.insert(t, rule{sandboxID: &s.sandboxID, enabled: true, action: "http_status"})

	_, err := s.pool.Exec(s.ctx,
		`UPDATE control.fault_rules SET error_data = 'order_id', http_status = 502 WHERE id = $1`, id)
	require.NoError(t, err)

	rules, err := s.store.Active(s.ctx, s.sandboxID)

	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "order_id", rules[0].ErrorData)
	assert.Equal(t, 502, rules[0].HTTPStatus)
}

func TestE2EActiveDecodesTheAccountMatchAndMessage(t *testing.T) {
	s := newStand(t)
	id := s.insert(t, rule{sandboxID: &s.sandboxID, enabled: true})

	_, err := s.pool.Exec(s.ctx, `
		UPDATE control.fault_rules
		SET match_account = '{"order_id":"42"}'::jsonb,
		    error_message = '{"ru":"р","uz":"u","en":"e"}'::jsonb
		WHERE id = $1`, id)
	require.NoError(t, err)

	rules, err := s.store.Active(s.ctx, s.sandboxID)

	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, map[string]string{"order_id": "42"}, rules[0].MatchAccount)
	assert.Equal(t, "e", rules[0].ErrorMessage.EN)
}

// A rule nobody can decode must be reported, not skipped: a stand that
// silently dropped it would answer normally and look correct.
func TestE2EActiveReportsAnUndecodableRule(t *testing.T) {
	s := newStand(t)
	id := s.insert(t, rule{sandboxID: &s.sandboxID, enabled: true})

	_, err := s.pool.Exec(s.ctx,
		`UPDATE control.fault_rules SET match_account = '"not an object"'::jsonb WHERE id = $1`, id)
	require.NoError(t, err)

	_, err = s.store.Active(s.ctx, s.sandboxID)

	assert.ErrorContains(t, err, "decode rule account match")
}

func TestE2EActiveReportsAnUndecodableMessage(t *testing.T) {
	s := newStand(t)
	id := s.insert(t, rule{sandboxID: &s.sandboxID, enabled: true})

	_, err := s.pool.Exec(s.ctx,
		`UPDATE control.fault_rules SET error_message = '"not an object"'::jsonb WHERE id = $1`, id)
	require.NoError(t, err)

	_, err = s.store.Active(s.ctx, s.sandboxID)

	assert.ErrorContains(t, err, "decode rule message")
}

// A row that no longer fits the domain is reported rather than skipped.
func TestE2EActiveReportsABadRow(t *testing.T) {
	s := newStand(t)
	s.insert(t, rule{sandboxID: &s.sandboxID, enabled: true})

	_, err := s.pool.Exec(s.ctx, `ALTER TABLE control.fault_rules ALTER COLUMN name DROP NOT NULL`)
	require.NoError(t, err)
	_, err = s.pool.Exec(s.ctx, `UPDATE control.fault_rules SET name = NULL`)
	require.NoError(t, err)

	_, err = s.store.Active(s.ctx, s.sandboxID)

	assert.ErrorContains(t, err, "scan fault rule")
}

func TestE2EActiveReportsALostDatabase(t *testing.T) {
	s := newStand(t)
	s.pool.Close()

	_, err := s.store.Active(s.ctx, s.sandboxID)

	assert.ErrorContains(t, err, "select fault rules")
}

// A limited rule is spent as it fires and switches itself off on the last use,
// so a "fail once" scenario cannot leak into the next test.
func TestE2EConsumeSpendsTheLastUse(t *testing.T) {
	s := newStand(t)
	times := 1
	id := s.insert(t, rule{sandboxID: &s.sandboxID, enabled: true, timesLeft: &times})

	require.NoError(t, s.store.Consume(s.ctx, id))

	var (
		left    *int
		enabled bool
		hits    int64
	)
	require.NoError(t, s.pool.QueryRow(s.ctx,
		`SELECT times_left, enabled, hit_count FROM control.fault_rules WHERE id = $1`, id).
		Scan(&left, &enabled, &hits))

	assert.Equal(t, 0, *left)
	assert.False(t, enabled)
	assert.Equal(t, int64(1), hits)
}

func TestE2EConsumeLeavesUsesOnAMultiUseRule(t *testing.T) {
	s := newStand(t)
	times := 3
	id := s.insert(t, rule{sandboxID: &s.sandboxID, enabled: true, timesLeft: &times})

	require.NoError(t, s.store.Consume(s.ctx, id))

	var (
		left    *int
		enabled bool
	)
	require.NoError(t, s.pool.QueryRow(s.ctx,
		`SELECT times_left, enabled FROM control.fault_rules WHERE id = $1`, id).Scan(&left, &enabled))

	assert.Equal(t, 2, *left)
	assert.True(t, enabled)
}

// An unlimited rule is counted but never spent.
func TestE2EConsumeLeavesAnUnlimitedRuleAlone(t *testing.T) {
	s := newStand(t)
	id := s.insert(t, rule{sandboxID: &s.sandboxID, enabled: true})

	require.NoError(t, s.store.Consume(s.ctx, id))

	var (
		left    *int
		enabled bool
		hits    int64
	)
	require.NoError(t, s.pool.QueryRow(s.ctx,
		`SELECT times_left, enabled, hit_count FROM control.fault_rules WHERE id = $1`, id).
		Scan(&left, &enabled, &hits))

	assert.Nil(t, left)
	assert.True(t, enabled)
	assert.Equal(t, int64(1), hits)
}

func TestE2EConsumeReportsAMissingRule(t *testing.T) {
	s := newStand(t)

	err := s.store.Consume(s.ctx, 999999)

	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestE2EConsumeReportsALostDatabase(t *testing.T) {
	s := newStand(t)
	id := s.insert(t, rule{sandboxID: &s.sandboxID, enabled: true})
	s.pool.Close()

	err := s.store.Consume(s.ctx, id)

	assert.ErrorContains(t, err, "consume fault rule")
}

// Without a hit count the console could not show that an unlimited rule is
// doing anything at all.
func TestE2EHitCounts(t *testing.T) {
	s := newStand(t)
	id := s.insert(t, rule{sandboxID: &s.sandboxID, enabled: true})

	require.NoError(t, s.store.Hit(s.ctx, id))
	require.NoError(t, s.store.Hit(s.ctx, id))

	var hits int64
	require.NoError(t, s.pool.QueryRow(s.ctx,
		`SELECT hit_count FROM control.fault_rules WHERE id = $1`, id).Scan(&hits))
	assert.Equal(t, int64(2), hits)
}

func TestE2EHitReportsALostDatabase(t *testing.T) {
	s := newStand(t)
	id := s.insert(t, rule{sandboxID: &s.sandboxID, enabled: true})
	s.pool.Close()

	err := s.store.Hit(s.ctx, id)

	assert.ErrorContains(t, err, "record fault rule hit")
}

func (s *stand) configID(t *testing.T) int64 {
	t.Helper()

	var id int64
	require.NoError(t, s.pool.QueryRow(s.ctx, `SELECT id FROM control.configs LIMIT 1`).Scan(&id))

	return id
}
