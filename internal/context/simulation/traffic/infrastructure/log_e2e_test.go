package infrastructure_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/simulation/traffic/domain"
	"github.com/bakhod1r/payme-mock/internal/context/simulation/traffic/infrastructure"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres/testdb"
)

type stand struct {
	pool      *postgres.Pool
	recorder  *infrastructure.Recorder
	ctx       context.Context
	sandboxID int64
}

func newStand(t *testing.T) *stand {
	t.Helper()

	pool := testdb.New(t)

	return &stand{
		pool:      pool,
		recorder:  infrastructure.NewRecorder(pool),
		ctx:       context.Background(),
		sandboxID: testdb.Seed(t, pool, "qa"),
	}
}

func TestE2ERecordStoresTheWholeCall(t *testing.T) {
	s := newStand(t)
	code := -31008

	err := s.recorder.Record(s.ctx, domain.Entry{
		SandboxID:    &s.sandboxID,
		Service:      "merchant",
		Direction:    domain.DirectionIn,
		Method:       "CheckTransaction",
		RPCID:        "42",
		HTTPStatus:   200,
		RequestBody:  []byte(`{"method":"CheckTransaction"}`),
		ResponseBody: []byte(`{"error":{"code":-31008}}`),
		DurationMS:   12,
		ErrorCode:    &code,
		RemoteAddr:   "127.0.0.1:54321",
	})

	require.NoError(t, err)

	var (
		sandboxID  *int64
		service    string
		direction  string
		method     *string
		rpcID      *string
		status     *int
		request    string
		response   string
		duration   int
		errorCode  *int
		remoteAddr *string
	)
	require.NoError(t, s.pool.QueryRow(s.ctx, `
		SELECT sandbox_id, service, direction, method, rpc_id, http_status,
		       request_body::text, response_body::text, duration_ms, error_code, remote_addr
		FROM control.request_log`).
		Scan(&sandboxID, &service, &direction, &method, &rpcID, &status,
			&request, &response, &duration, &errorCode, &remoteAddr))

	assert.Equal(t, s.sandboxID, *sandboxID)
	assert.Equal(t, "merchant", service)
	assert.Equal(t, "in", direction)
	assert.Equal(t, "CheckTransaction", *method)
	assert.Equal(t, "42", *rpcID)
	assert.Equal(t, 200, *status)
	assert.JSONEq(t, `{"method":"CheckTransaction"}`, request)
	assert.JSONEq(t, `{"error":{"code":-31008}}`, response)
	assert.Equal(t, 12, duration)
	assert.Equal(t, code, *errorCode)
	assert.Equal(t, "127.0.0.1:54321", *remoteAddr)
}

// A request that never resolved to a stand is still worth recording: it is the
// only evidence that someone called an address that does not exist.
func TestE2ERecordStoresACallWithoutASandbox(t *testing.T) {
	s := newStand(t)

	err := s.recorder.Record(s.ctx, domain.Entry{
		Service:    "merchant",
		Direction:  domain.DirectionIn,
		HTTPStatus: 404,
		DurationMS: 1,
	})

	require.NoError(t, err)

	var (
		sandboxID *int64
		method    *string
		request   *string
	)
	require.NoError(t, s.pool.QueryRow(s.ctx, `
		SELECT sandbox_id, method, request_body::text FROM control.request_log`).
		Scan(&sandboxID, &method, &request))

	assert.Nil(t, sandboxID)
	assert.Nil(t, method, "a body with no method leaves the column empty rather than blank")
	assert.Nil(t, request)
}

// Malformed output is what this stand produces on purpose, so a body that is
// not JSON must never cost the record that explains it.
func TestE2ERecordStoresABodyThatIsNotJSON(t *testing.T) {
	s := newStand(t)

	err := s.recorder.Record(s.ctx, domain.Entry{
		SandboxID:    &s.sandboxID,
		Service:      "merchant",
		Direction:    domain.DirectionOut,
		ResponseBody: []byte(`{"result":`),
		DurationMS:   3,
	})

	require.NoError(t, err)

	var response string
	require.NoError(t, s.pool.QueryRow(s.ctx,
		`SELECT response_body::text FROM control.request_log`).Scan(&response))

	// The raw text survives as a JSON string, which the console still shows.
	assert.JSONEq(t, `"{\"result\":"`, response)
}

// The rule that shaped a response is named on the record, which is what lets
// the traffic screen say why a call failed.
func TestE2ERecordNamesTheRuleThatFired(t *testing.T) {
	s := newStand(t)
	ruleID := s.seedRule(t)

	err := s.recorder.Record(s.ctx, domain.Entry{
		SandboxID:   &s.sandboxID,
		Service:     "merchant",
		Direction:   domain.DirectionIn,
		FaultRuleID: &ruleID,
		DurationMS:  2,
	})

	require.NoError(t, err)

	var stored *int64
	require.NoError(t, s.pool.QueryRow(s.ctx,
		`SELECT fault_rule_id FROM control.request_log`).Scan(&stored))
	assert.Equal(t, ruleID, *stored)
}

// The record is the only account of a request that failed, so it is written
// against the pool rather than any transaction that may be rolled back.
func TestE2ERecordSurvivesARolledBackTransaction(t *testing.T) {
	s := newStand(t)

	err := postgres.WithTx(s.ctx, s.pool, func(inner context.Context) error {
		require.NoError(t, s.recorder.Record(inner, domain.Entry{
			SandboxID:  &s.sandboxID,
			Service:    "merchant",
			Direction:  domain.DirectionIn,
			DurationMS: 1,
		}))

		return context.Canceled
	})

	require.Error(t, err)

	var count int
	require.NoError(t, s.pool.QueryRow(s.ctx,
		`SELECT count(*) FROM control.request_log`).Scan(&count))
	assert.Equal(t, 1, count, "the record outlives the transaction that failed")
}

func TestE2ERecordReportsALostDatabase(t *testing.T) {
	s := newStand(t)
	s.pool.Close()

	err := s.recorder.Record(s.ctx, domain.Entry{Service: "merchant", Direction: domain.DirectionIn})

	assert.ErrorContains(t, err, "insert traffic entry")
}

func (s *stand) seedRule(t *testing.T) int64 {
	t.Helper()

	var id int64
	require.NoError(t, s.pool.QueryRow(s.ctx, `
		INSERT INTO control.fault_rules (sandbox_id, name, action, error_message)
		VALUES ($1, 'check fails', 'rpc_error', '{}'::jsonb)
		RETURNING id`, s.sandboxID).Scan(&id))

	return id
}

// Headers are half of what goes wrong between two services, so a call that
// carried some keeps them — and one that carried none reads as unset rather
// than as an empty object, which would say "this call had no headers" about a
// record that simply never collected them.
func TestE2ERecordKeepsHeadersAndLeavesNoneUnset(t *testing.T) {
	s := newStand(t)

	require.NoError(t, s.recorder.Record(s.ctx, domain.Entry{
		SandboxID:  &s.sandboxID,
		Service:    "paymemock",
		Direction:  domain.DirectionIn,
		Method:     "cards.check",
		HTTPStatus: 200,
		RequestHeaders: map[string]string{
			"X-Auth": "merchant:key",
		},
		ResponseHeaders: map[string]string{
			"Content-Type": "text/json; charset=UTF-8",
		},
	}))

	var request, response *string
	require.NoError(t, s.pool.QueryRow(s.ctx, `
		SELECT request_headers::text, response_headers::text
		FROM control.request_log ORDER BY id DESC LIMIT 1`).Scan(&request, &response))

	require.NotNil(t, request)
	assert.JSONEq(t, `{"X-Auth":"merchant:key"}`, *request)
	require.NotNil(t, response)
	assert.JSONEq(t, `{"Content-Type":"text/json; charset=UTF-8"}`, *response)

	require.NoError(t, s.recorder.Record(s.ctx, domain.Entry{
		SandboxID:  &s.sandboxID,
		Service:    "paymemock",
		Direction:  domain.DirectionIn,
		Method:     "cards.check",
		HTTPStatus: 200,
	}))

	require.NoError(t, s.pool.QueryRow(s.ctx, `
		SELECT request_headers::text, response_headers::text
		FROM control.request_log ORDER BY id DESC LIMIT 1`).Scan(&request, &response))

	assert.Nil(t, request, "a record with nothing to say about headers says nothing")
	assert.Nil(t, response)
}
