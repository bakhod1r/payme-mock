package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	billinginfra "github.com/bakhod1r/payme-mock/internal/context/payment/billing/infrastructure"
	merchantinfra "github.com/bakhod1r/payme-mock/internal/context/payment/merchant/infrastructure"
	accessinfra "github.com/bakhod1r/payme-mock/internal/context/simulation/access/infrastructure"
	faultinfra "github.com/bakhod1r/payme-mock/internal/context/simulation/fault/infrastructure"
	sandboxdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/sandbox/domain"
	sandboxinfra "github.com/bakhod1r/payme-mock/internal/context/simulation/sandbox/infrastructure"
	trafficdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/traffic/domain"
	trafficinfra "github.com/bakhod1r/payme-mock/internal/context/simulation/traffic/infrastructure"
	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres/testdb"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// This service is the address a cash register is pointed at, and everything it
// does happens before the protocol: it finds the stand the path names, checks
// the address the call came from, builds the handler that stand's profile asks
// for, and writes down what happened. None of that is exercised by the
// application tests, which start from a handler that already exists.

// stand is the service assembled the way run assembles it, in front of a real
// database.
type stand struct {
	pool      *postgres.Pool
	server    *httptest.Server
	sandboxID int64
	orderID   int64
	ctx       context.Context
}

const (
	slug      = "qa"
	liveKey   = "live-key"
	payerName = "Test payer"
	amount    = int64(500000)
)

func newStand(t *testing.T) *stand {
	t.Helper()

	pool := testdb.New(t)
	sandboxID := testdb.Seed(t, pool, slug)
	_, orderID := testdb.SeedAccountAndOrder(t, pool, sandboxID, amount)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	clk := clock.New()

	handlers := newHandlerCache(pool, dependencies{
		transactions: merchantinfra.NewTransactionRepository(pool),
		events:       merchantinfra.NewEventRecorder(pool),
		accounts:     billinginfra.NewAccountRepository(pool),
		orders:       billinginfra.NewOrderRepository(pool),
		walkIns:      billinginfra.NewWalkInRepository(pool),
	}, clk)

	rules := faultinfra.NewRuleStore(pool)

	handler := withMiddleware(handlers, middlewareDeps{
		rules:   rules,
		hits:    rules,
		traffic: trafficinfra.NewRecorder(pool),
		clock:   clk,
	}, log)

	server := httptest.NewServer(routes(
		sandboxinfra.NewRepository(pool),
		httpx.Allowlist(accessinfra.NewRepository(pool), true, log),
		handler,
	))
	t.Cleanup(server.Close)

	return &stand{pool: pool, server: server, sandboxID: sandboxID, orderID: orderID,
		ctx: context.Background()}
}

// call sends one Merchant API request the way Payme sends it, and returns the
// decoded envelope.
func (s *stand) call(t *testing.T, path, key, body string) (int, map[string]any) {
	t.Helper()

	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.server.URL+path,
		bytes.NewReader([]byte(body)))
	require.NoError(t, err)

	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization",
			"Basic "+base64.StdEncoding.EncodeToString([]byte("Paycom:"+key)))
	}

	resp, err := s.server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var envelope map[string]any
	_ = json.Unmarshal(raw, &envelope)

	return resp.StatusCode, envelope
}

// checkPerform is the call every stand answers first. The account names the
// order being paid, which is what the documented profile identifies a payer by.
func (s *stand) checkPerform(amount int64) string {
	return fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"CheckPerformTransaction",`+
			`"params":{"amount":%d,"account":{"order_id":"%d"}}}`, amount, s.orderID)
}

// The whole path a real call takes: the stand is found by the slug, the key is
// checked against that stand's own, the profile in force decides the handler,
// and the call is written down as it was answered.
func TestE2EMerchantAnswersACallForTheStandItNames(t *testing.T) {
	s := newStand(t)

	status, envelope := s.call(t, "/s/"+slug+"/payme/merchant", liveKey, s.checkPerform(amount))

	require.Equal(t, http.StatusOK, status)
	result, ok := envelope["result"].(map[string]any)
	require.True(t, ok, "envelope: %v", envelope)
	assert.Equal(t, true, result["allow"])

	var logged struct {
		method  string
		service string
		status  int
	}
	require.NoError(t, s.pool.QueryRow(s.ctx, `
		SELECT coalesce(method, ''), service, coalesce(http_status, 0)
		FROM control.request_log
		WHERE sandbox_id = $1
		ORDER BY id DESC LIMIT 1`, s.sandboxID).
		Scan(&logged.method, &logged.service, &logged.status))

	assert.Equal(t, "CheckPerformTransaction", logged.method)
	assert.Equal(t, "merchant", logged.service)
	assert.Equal(t, http.StatusOK, logged.status)
}

// A second call to the same stand is answered by the handler the first one
// built. The profile is what a handler is built from, so rebuilding it per
// request would read the database on every call and lose nothing else.
func TestE2EMerchantReusesTheHandlerItBuilt(t *testing.T) {
	s := newStand(t)

	for range 2 {
		status, _ := s.call(t, "/s/"+slug+"/payme/merchant", liveKey, s.checkPerform(amount))
		require.Equal(t, http.StatusOK, status)
	}
}

// The key belongs to the stand, not to the service: a call carrying no
// credential, or another stand's, is refused with the code the protocol owes it
// rather than let through to a handler.
func TestE2EMerchantRefusesTheWrongKey(t *testing.T) {
	s := newStand(t)

	for _, key := range []string{"", "someone-elses-key"} {
		_, envelope := s.call(t, "/s/"+slug+"/payme/merchant", key, s.checkPerform(amount))

		failure, ok := envelope["error"].(map[string]any)
		require.True(t, ok, "envelope: %v", envelope)
		assert.Equal(t, float64(-32504), failure["code"])
	}
}

// An unknown slug is a wrong address rather than a protocol error: no JSON-RPC
// body could say which stand it came from, so it is answered at the HTTP level.
func TestE2EMerchantAnswersAnUnknownStandAsAWrongAddress(t *testing.T) {
	s := newStand(t)

	status, _ := s.call(t, "/s/nobody/payme/merchant", liveKey, s.checkPerform(amount))

	assert.Equal(t, http.StatusNotFound, status)
}

// A database that went away is the service's own failure and is answered as
// one, rather than as "no such stand" — which would send an integration
// looking at its own configuration.
func TestE2EMerchantReportsALostDatabaseAsItsOwnFailure(t *testing.T) {
	s := newStand(t)
	s.pool.Close()

	status, _ := s.call(t, "/s/"+slug+"/payme/merchant", liveKey, s.checkPerform(amount))

	assert.Equal(t, http.StatusInternalServerError, status)
}

// The stand answers only the addresses it was told to. The check runs after the
// stand is known — a rule belongs to a stand — and before the handler, which is
// where the real provider drops traffic from an address nobody registered.
func TestE2EMerchantDropsAnAddressTheStandNeverRegistered(t *testing.T) {
	s := newStand(t)

	_, err := s.pool.Exec(s.ctx, `
		INSERT INTO control.ip_rules (sandbox_id, cidr, note)
		VALUES ($1, '10.0.0.0/8'::cidr, 'somewhere else')`, s.sandboxID)
	require.NoError(t, err)

	status, _ := s.call(t, "/s/"+slug+"/payme/merchant", liveKey, s.checkPerform(amount))

	assert.Equal(t, http.StatusForbidden, status)
}

// A rule is what a stand is for: the call is answered with the failure an
// operator asked for, the rule's use is counted, and the log says which rule
// did it.
func TestE2EMerchantAppliesAFaultRule(t *testing.T) {
	s := newStand(t)

	var ruleID int64
	require.NoError(t, s.pool.QueryRow(s.ctx, `
		INSERT INTO control.fault_rules
			(sandbox_id, name, service, method, action, error_code, enabled)
		VALUES ($1, 'probe', 'merchant', 'CheckPerformTransaction', 'rpc_error', -31099, TRUE)
		RETURNING id`, s.sandboxID).Scan(&ruleID))

	_, envelope := s.call(t, "/s/"+slug+"/payme/merchant", liveKey, s.checkPerform(amount))

	failure, ok := envelope["error"].(map[string]any)
	require.True(t, ok, "envelope: %v", envelope)
	assert.Equal(t, float64(-31099), failure["code"])

	var hits int
	require.NoError(t, s.pool.QueryRow(s.ctx,
		`SELECT hit_count FROM control.fault_rules WHERE id = $1`, ruleID).Scan(&hits))
	assert.Equal(t, 1, hits, "a rule that fired is counted, or the console cannot show it working")
}

// A rule that narrows by amount reads the amount out of the params, which is
// the only reason the request has to be described before it is dispatched.
func TestE2EMerchantAppliesARuleNarrowedByAmount(t *testing.T) {
	s := newStand(t)

	_, err := s.pool.Exec(s.ctx, `
		INSERT INTO control.fault_rules
			(sandbox_id, name, service, method, action, error_code, enabled,
			 amount_min)
		VALUES ($1, 'big ones only', 'merchant', '*', 'rpc_error', -31099, TRUE, $2)`,
		s.sandboxID, amount)
	require.NoError(t, err)

	// The small call is refused for its own reason — it is not the amount the
	// order expects — and what matters here is that the refusal is not the
	// rule's: a rule with a floor must leave everything under it alone.
	_, small := s.call(t, "/s/"+slug+"/payme/merchant", liveKey, s.checkPerform(1))
	under, ok := small["error"].(map[string]any)
	require.True(t, ok, "envelope: %v", small)
	assert.NotEqual(t, float64(-31099), under["code"],
		"a call under the rule's floor is not the rule's business")

	_, big := s.call(t, "/s/"+slug+"/payme/merchant", liveKey, s.checkPerform(amount))
	failure, ok := big["error"].(map[string]any)
	require.True(t, ok, "envelope: %v", big)
	assert.Equal(t, float64(-31099), failure["code"])
}

// A body that is not JSON-RPC still describes a request; it simply has no
// method to narrow a rule by, so only service-wide rules can match it.
func TestE2EMerchantHandlesABodyThatIsNotJSONRPC(t *testing.T) {
	s := newStand(t)

	status, _ := s.call(t, "/s/"+slug+"/payme/merchant", liveKey, "not json at all")

	assert.Equal(t, http.StatusOK, status, "the protocol reports its errors inside a 200")
}

// The health check answers without a stand, because it is what a container
// runtime asks before any stand exists.
func TestE2EMerchantHealthz(t *testing.T) {
	s := newStand(t)

	resp, err := s.server.Client().Get(s.server.URL + "/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "ok", string(body))
}

// ---------- the parts, where the whole cannot reach them ----------

// Reaching the handler with no stand on the context is a wiring mistake rather
// than a caller's error, and it is answered as one.
func TestHandlerCacheRefusesAnUnscopedRequest(t *testing.T) {
	cache := newHandlerCache(nil, dependencies{}, clock.New())

	rec := httptest.NewRecorder()
	cache.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// A profile that cannot be read leaves the service with no settings to build a
// handler from. Falling back to the defaults would answer a call by rules
// nobody chose, so the request is refused instead.
func TestE2EHandlerCacheReportsAProfileItCannotRead(t *testing.T) {
	s := newStand(t)
	cache := newHandlerCache(s.pool, dependencies{}, clock.New())

	t.Run("a profile that is not there", func(t *testing.T) {
		_, err := cache.forRegister(s.ctx, 999999, "topup")

		assert.ErrorContains(t, err, "load profile")
	})

	t.Run("a profile that is not settings", func(t *testing.T) {
		var configID int64
		require.NoError(t, s.pool.QueryRow(s.ctx, `
			INSERT INTO control.configs (name, description, settings, builtin)
			VALUES ('broken', '', '"not an object"'::jsonb, false)
			RETURNING id`).Scan(&configID))

		_, err := cache.forRegister(s.ctx, configID, "topup")

		assert.ErrorContains(t, err, "decode profile")
	})
}

// A stand with no profile of its own runs on the documented defaults rather
// than refusing to serve, which is the state every stand is created in.
func TestE2EHandlerCacheFallsBackToTheDefaults(t *testing.T) {
	s := newStand(t)
	cache := newHandlerCache(s.pool, dependencies{}, clock.New())

	handler, err := cache.forRegister(s.ctx, 0, "topup")

	require.NoError(t, err)
	assert.NotNil(t, handler)
}

// The key the call is checked against is the stand's, so a context carrying no
// stand has no key to offer and says so.
func TestResolveKeyNeedsAStand(t *testing.T) {
	_, err := resolveKey(context.Background())
	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)

	key, err := resolveKey(sandboxctx.With(context.Background(), sandboxctx.Sandbox{Key: "k"}))
	require.NoError(t, err)
	assert.Equal(t, "k", key)
}

// A stand with no profile is zero downstream, which is what "run on the
// defaults" is spelled as.
func TestConfigIDFlattensTheOptionalProfile(t *testing.T) {
	assert.Zero(t, configID(&sandboxdomain.Sandbox{}))

	id := int64(7)
	assert.Equal(t, id, configID(&sandboxdomain.Sandbox{ConfigID: &id}))
}

// A stand whose profile cannot be read has no settings to build a handler
// from, and the call is refused rather than answered by rules nobody chose.
func TestE2EMerchantRefusesACallForAnUnreadableProfile(t *testing.T) {
	s := newStand(t)

	_, err := s.pool.Exec(s.ctx, `
		UPDATE control.configs SET settings = '"not an object"'::jsonb
		WHERE id = (SELECT active_config_id FROM control.sandboxes WHERE id = $1)`,
		s.sandboxID)
	require.NoError(t, err)

	status, _ := s.call(t, "/s/"+slug+"/payme/merchant", liveKey, s.checkPerform(amount))

	assert.Equal(t, http.StatusInternalServerError, status)
}

// A rule that fires only sometimes is decided on a random draw, which is the
// one thing in the stack that cannot be read off the request. A rule that never
// draws high enough leaves the call alone.
func TestE2EMerchantDrawsForAProbabilityRule(t *testing.T) {
	s := newStand(t)

	_, err := s.pool.Exec(s.ctx, `
		INSERT INTO control.fault_rules
			(sandbox_id, name, service, method, action, error_code, enabled, probability)
		VALUES ($1, 'never', 'merchant', '*', 'rpc_error', -31099, TRUE, 0)`, s.sandboxID)
	require.NoError(t, err)

	_, envelope := s.call(t, "/s/"+slug+"/payme/merchant", liveKey, s.checkPerform(amount))

	result, ok := envelope["result"].(map[string]any)
	require.True(t, ok, "envelope: %v", envelope)
	assert.Equal(t, true, result["allow"])
}

// Bookkeeping about a call must not change what the caller was answered. A rule
// whose use could not be counted, and a call whose record could not be written,
// are both told to the operator and to nobody else.
func TestE2EMerchantAnswersEvenWhenItsOwnBookkeepingFails(t *testing.T) {
	s := newStand(t)

	var ruleID int64
	require.NoError(t, s.pool.QueryRow(s.ctx, `
		INSERT INTO control.fault_rules
			(sandbox_id, name, service, method, action, error_code, enabled)
		VALUES ($1, 'probe', 'merchant', '*', 'rpc_error', -31099, TRUE)
		RETURNING id`, s.sandboxID).Scan(&ruleID))

	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))

	handler := withMiddleware(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"result":{}}`))
		}),
		middlewareDeps{
			rules:   faultinfra.NewRuleStore(s.pool),
			hits:    brokenHits{},
			traffic: brokenTraffic{},
			clock:   clock.New(),
		}, log)

	server := httptest.NewServer(routes(
		sandboxinfra.NewRepository(s.pool),
		func(next http.Handler) http.Handler { return next },
		handler,
	))
	defer server.Close()

	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost,
		server.URL+"/s/"+slug+"/payme/merchant", bytes.NewReader([]byte(s.checkPerform(amount))))
	require.NoError(t, err)

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Contains(t, string(body), "-31099", "the caller is answered as the rule says")

	// The record is written after the answer has gone out, which is the point:
	// the caller waits for nothing the log does.
	require.Eventually(t, func() bool {
		return strings.Contains(logged.String(), "fault rule hit not recorded") &&
			strings.Contains(logged.String(), "traffic record failed")
	}, 5*time.Second, 20*time.Millisecond, "the operator is told: %s", logged.String())
}

// brokenHits cannot count anything, which is what a database that went away
// looks like to the counter.
type brokenHits struct{}

func (brokenHits) Hit(context.Context, int64) error { return errBroken }

// brokenTraffic cannot write the log.
type brokenTraffic struct{}

func (brokenTraffic) Record(context.Context, trafficdomain.Entry) error { return errBroken }

var errBroken = errors.New("the database is gone")

// ---------- the service itself ----------

// The service boots the way its own main boots it, serves, and stops when it is
// told to. A shutdown that hangs or a boot that fails silently is the failure
// this covers, and neither can be seen from a handler test.
func TestE2EServiceStartsAndStops(t *testing.T) {
	_, url := testdb.NewWithURL(t)

	t.Setenv("DATABASE_URL", url)
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	// Migrating on start is what the service does in its own compose file, and
	// running it against an already-migrated database is the ordinary case.
	t.Setenv("MIGRATE_ON_START", "true")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	// The listener is up before the shutdown is asked for, so what is being
	// tested is a running service stopping rather than a race with its start.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the service did not stop when it was told to")
	}
}

// Migrations that will not apply stop the service. Serving on a schema the
// code does not match would answer every call with a failure about a column,
// which is a worse way to find out than not starting.
func TestE2EServiceReportsMigrationsItCannotApply(t *testing.T) {
	pool, url := testdb.NewWithURL(t)

	// The record of what has already run is dropped while the tables it
	// describes stay, so the next run tries to create what is there.
	_, err := pool.Exec(context.Background(), `DROP TABLE goose_db_version`)
	require.NoError(t, err)

	t.Setenv("DATABASE_URL", url)
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("MIGRATE_ON_START", "true")

	err = run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	assert.Error(t, err)
}

// A database it cannot reach is reported rather than served around: a service
// that came up without one would answer every call with a failure nobody could
// explain.
func TestServiceReportsADatabaseItCannotReach(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://payme:payme@127.0.0.1:1/paymemock?sslmode=disable&connect_timeout=1")

	err := run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	assert.Error(t, err)
}

// A configuration that cannot be read stops the service before it opens
// anything.
func TestServiceReportsAConfigurationItCannotRead(t *testing.T) {
	t.Setenv("SHUTDOWN_TIMEOUT", "not a duration")

	err := run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	assert.Error(t, err)
}

// An address already taken is the ordinary way a service fails to start, and it
// has to be reported rather than waited on.
func TestE2EServiceReportsAnAddressItCannotListenOn(t *testing.T) {
	_, url := testdb.NewWithURL(t)

	taken := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer taken.Close()

	t.Setenv("DATABASE_URL", url)
	t.Setenv("HTTP_ADDR", taken.Listener.Addr().String())
	t.Setenv("MIGRATE_ON_START", "false")

	err := run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	assert.Error(t, err)
}

// main is the entry point, and what it does with a failure — say so and leave
// with a status a container runtime can read — is the one decision it makes.
func TestMainReportsAFailedStart(t *testing.T) {
	t.Setenv("SHUTDOWN_TIMEOUT", "not a duration")

	var status int
	exit = func(code int) { status = code }
	defer func() { exit = osExit }()

	main()

	assert.Equal(t, 1, status)
}
