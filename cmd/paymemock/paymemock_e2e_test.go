package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	billinginfra "github.com/bakhod1r/payme-mock/internal/context/payment/billing/infrastructure"
	subscribeinfra "github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/infrastructure"
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

// This service is the provider's own address: it finds the stand a call is for
// — by the path or by the credential — checks where the call came from, builds
// the handler that stand's profile asks for, replays a repeat rather than doing
// it twice, and writes down what happened. All of it sits outside the
// application, which starts from a handler that already exists.

const (
	standSlug  = "qa"
	merchantID = "merchant-qa"
	testKey    = "test-key"
	cardNumber = "8600069195406311"
)

type stand struct {
	pool      *postgres.Pool
	server    *httptest.Server
	sandboxID int64
	ctx       context.Context
}

func newStand(t *testing.T) *stand {
	t.Helper()

	pool := testdb.New(t)
	sandboxID := testdb.Seed(t, pool, standSlug)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	clk := clock.New()
	scheduler := subscribeinfra.NewScheduler(log)

	handlers := newHandlerCache(pool, dependencies{
		cards:     subscribeinfra.NewCardRepository(pool),
		receipts:  subscribeinfra.NewReceiptRepository(pool),
		scheduler: scheduler,
		tokens:    subscribeinfra.NewTokens(),
		sms:       subscribeinfra.NewSMSLog(log),
		newClient: merchantClients(),
		ledger:    billinginfra.NewCashboxLedger(pool),
	}, clk, "http://merchant.invalid")

	rules := faultinfra.NewRuleStore(pool)

	handler := withMiddleware(handlers, middlewareDeps{
		rules:   rules,
		hits:    rules,
		traffic: trafficinfra.NewRecorder(pool),
		clock:   clk,
		repeats: httpx.NewIdempotencyMiddleware(
			postgres.NewCallStore(pool), idempotentMethods, time.Hour,
			func(ctx context.Context) (int64, bool) {
				sandbox, ok := sandboxctx.Get(ctx)
				return sandbox.ID, ok
			},
		),
	}, log)

	server := httptest.NewServer(routes(
		sandboxinfra.NewRepository(pool),
		httpx.Allowlist(accessinfra.NewRepository(pool), true, log),
		handler,
	))
	t.Cleanup(func() {
		server.Close()
		scheduler.Wait()
	})

	return &stand{pool: pool, server: server, sandboxID: sandboxID, ctx: context.Background()}
}

// call sends one Subscribe API request and returns the decoded envelope.
func (s *stand) call(t *testing.T, path, auth, body string) (int, map[string]any) {
	t.Helper()

	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.server.URL+path,
		bytes.NewReader([]byte(body)))
	require.NoError(t, err)

	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("X-Auth", auth)
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

const createCard = `{"jsonrpc":"2.0","id":1,"method":"cards.create",` +
	`"params":{"card":{"number":"` + cardNumber + `","expire":"0399"},"save":true}}`

// cardToken reads the token out of a cards.create answer, which is what every
// later call sends.
func cardToken(t *testing.T, envelope map[string]any) string {
	t.Helper()

	result, ok := envelope["result"].(map[string]any)
	require.True(t, ok, "envelope: %v", envelope)

	card, ok := result["card"].(map[string]any)
	require.True(t, ok, "envelope: %v", envelope)

	token, ok := card["token"].(string)
	require.True(t, ok, "envelope: %v", envelope)

	return token
}

// The path names the stand, which is the address the console shows and the
// gateway forwards to.
func TestE2ESubscribeAnswersOnTheStandsOwnPath(t *testing.T) {
	s := newStand(t)

	status, envelope := s.call(t, "/s/"+standSlug+"/api", merchantID, createCard)

	require.Equal(t, http.StatusOK, status)
	assert.NotEmpty(t, cardToken(t, envelope))

	var method string
	require.NoError(t, s.pool.QueryRow(s.ctx, `
		SELECT coalesce(method, '') FROM control.request_log
		WHERE sandbox_id = $1 ORDER BY id DESC LIMIT 1`, s.sandboxID).Scan(&method))
	assert.Equal(t, "cards.create", method)
}

// A client holding several cash registers but one API URL reaches its stands by
// the merchant id in the credential, which is the shape of the real provider's
// configuration.
func TestE2ESubscribeAnswersOnTheSharedAddress(t *testing.T) {
	s := newStand(t)

	status, envelope := s.call(t, "/api", merchantID, createCard)

	require.Equal(t, http.StatusOK, status)
	assert.NotEmpty(t, cardToken(t, envelope))
}

// The shared address serves every register and tells them apart by the
// credential, so a credential it cannot serve is refused in the protocol — the
// same envelope every other refusal arrives in.
func TestE2ESubscribeRefusesACredentialTheSharedAddressCannotServe(t *testing.T) {
	s := newStand(t)

	for _, auth := range []string{"", "nobody"} {
		status, envelope := s.call(t, "/api", auth, createCard)

		require.Equal(t, http.StatusOK, status, "a protocol refusal is not an HTTP failure")
		failure, ok := envelope["error"].(map[string]any)
		require.True(t, ok, "envelope: %v", envelope)
		assert.Equal(t, float64(-32504), failure["code"])
	}
}

// A slug nobody holds is a wrong address, which is answered as one: the path is
// the address here, not a credential.
func TestE2ESubscribeAnswersAnUnknownSlugAsAWrongAddress(t *testing.T) {
	s := newStand(t)

	status, _ := s.call(t, "/s/nobody/api", merchantID, createCard)

	assert.Equal(t, http.StatusNotFound, status)
}

// A database that went away is the service's own failure on both addresses, and
// is answered as one rather than as a credential or an address the caller got
// wrong.
func TestE2ESubscribeReportsALostDatabase(t *testing.T) {
	s := newStand(t)
	s.pool.Close()

	status, _ := s.call(t, "/s/"+standSlug+"/api", merchantID, createCard)
	assert.Equal(t, http.StatusInternalServerError, status)

	status, _ = s.call(t, "/api", merchantID, createCard)
	assert.Equal(t, http.StatusInternalServerError, status)
}

// A payout asked for twice is two payouts and the caller cannot tell, so the
// repeat is answered with the first answer — through the whole stack, where the
// replay has to sit inside the traffic log and inside the rules.
func TestE2ESubscribeReplaysARepeatedCall(t *testing.T) {
	s := newStand(t)

	_, first := s.call(t, "/api", merchantID, createCard)
	_, second := s.call(t, "/api", merchantID, createCard)

	assert.Equal(t, cardToken(t, first), cardToken(t, second))

	var calls int
	require.NoError(t, s.pool.QueryRow(s.ctx,
		`SELECT count(*) FROM control.request_log WHERE method = 'cards.create'`).Scan(&calls))
	assert.Equal(t, 2, calls, "a replay is still traffic an operator has to see")
}

// The stand answers only the addresses it was told to.
func TestE2ESubscribeDropsAnAddressTheStandNeverRegistered(t *testing.T) {
	s := newStand(t)

	_, err := s.pool.Exec(s.ctx, `
		INSERT INTO control.ip_rules (sandbox_id, cidr, note)
		VALUES ($1, '10.0.0.0/8'::cidr, 'somewhere else')`, s.sandboxID)
	require.NoError(t, err)

	status, _ := s.call(t, "/s/"+standSlug+"/api", merchantID, createCard)

	assert.Equal(t, http.StatusForbidden, status)
}

// A rule is what a stand is for: the call is answered with the failure an
// operator asked for, and the rule's use is counted.
func TestE2ESubscribeAppliesAFaultRule(t *testing.T) {
	s := newStand(t)

	var ruleID int64
	require.NoError(t, s.pool.QueryRow(s.ctx, `
		INSERT INTO control.fault_rules
			(sandbox_id, name, service, method, action, error_code, enabled)
		VALUES ($1, 'probe', 'paymemock', 'cards.create', 'rpc_error', -31099, TRUE)
		RETURNING id`, s.sandboxID).Scan(&ruleID))

	_, envelope := s.call(t, "/api", merchantID, createCard)

	failure, ok := envelope["error"].(map[string]any)
	require.True(t, ok, "envelope: %v", envelope)
	assert.Equal(t, float64(-31099), failure["code"])

	var hits int
	require.NoError(t, s.pool.QueryRow(s.ctx,
		`SELECT hit_count FROM control.fault_rules WHERE id = $1`, ruleID).Scan(&hits))
	assert.Equal(t, 1, hits)
}

// A rule that fires only sometimes is decided on a draw, which is the one thing
// in the stack that cannot be read off the request.
func TestE2ESubscribeDrawsForAProbabilityRule(t *testing.T) {
	s := newStand(t)

	_, err := s.pool.Exec(s.ctx, `
		INSERT INTO control.fault_rules
			(sandbox_id, name, service, method, action, error_code, enabled, probability)
		VALUES ($1, 'never', 'paymemock', '*', 'rpc_error', -31099, TRUE, 0)`, s.sandboxID)
	require.NoError(t, err)

	_, envelope := s.call(t, "/api", merchantID, createCard)

	assert.NotEmpty(t, cardToken(t, envelope))
}

// A body that is not JSON-RPC still describes a request; it simply has no
// method to narrow a rule by.
func TestE2ESubscribeHandlesABodyThatIsNotJSONRPC(t *testing.T) {
	s := newStand(t)

	status, _ := s.call(t, "/api", merchantID, "not json at all")

	assert.Equal(t, http.StatusOK, status)
}

// Bookkeeping about a call must not change what the caller was answered.
func TestE2ESubscribeAnswersEvenWhenItsOwnBookkeepingFails(t *testing.T) {
	s := newStand(t)

	_, err := s.pool.Exec(s.ctx, `
		INSERT INTO control.fault_rules
			(sandbox_id, name, service, method, action, error_code, enabled)
		VALUES ($1, 'probe', 'paymemock', '*', 'rpc_error', -31099, TRUE)`, s.sandboxID)
	require.NoError(t, err)

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
			repeats: httpx.NewIdempotencyMiddleware(
				postgres.NewCallStore(s.pool), idempotentMethods, time.Hour,
				func(ctx context.Context) (int64, bool) {
					sandbox, ok := sandboxctx.Get(ctx)
					return sandbox.ID, ok
				},
			),
		}, log)

	server := httptest.NewServer(routes(
		sandboxinfra.NewRepository(s.pool),
		func(next http.Handler) http.Handler { return next },
		handler,
	))
	defer server.Close()

	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost,
		server.URL+"/api", bytes.NewReader([]byte(createCard)))
	require.NoError(t, err)
	req.Header.Set("X-Auth", merchantID)

	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "-31099")

	require.Eventually(t, func() bool {
		return strings.Contains(logged.String(), "fault rule hit not recorded") &&
			strings.Contains(logged.String(), "traffic record failed")
	}, 5*time.Second, 20*time.Millisecond, "the operator is told: %s", logged.String())
}

type brokenHits struct{}

func (brokenHits) Hit(context.Context, int64) error { return errBroken }

type brokenTraffic struct{}

func (brokenTraffic) Record(context.Context, trafficdomain.Entry) error { return errBroken }

var errBroken = errors.New("the database is gone")

// A second call for the same stand is answered by the handler the first one
// built. The two calls carry different ids so both reach the handler: a repeat
// would be answered out of the replay store and never build anything.
func TestE2ESubscribeReusesTheHandlerItBuilt(t *testing.T) {
	s := newStand(t)

	for _, id := range []string{"1", "2"} {
		status, envelope := s.call(t, "/api", merchantID,
			`{"jsonrpc":"2.0","id":`+id+`,"method":"cards.create",`+
				`"params":{"card":{"number":"`+cardNumber+`","expire":"0399"},"save":true}}`)

		require.Equal(t, http.StatusOK, status)
		assert.NotEmpty(t, cardToken(t, envelope))
	}
}

// A stand with no profile of its own runs on the documented defaults rather
// than refusing to serve, which is the state a stand made by hand is in.
func TestE2ESubscribeServesAStandWithNoProfile(t *testing.T) {
	s := newStand(t)

	_, err := s.pool.Exec(s.ctx,
		`UPDATE control.sandboxes SET active_config_id = NULL WHERE id = $1`, s.sandboxID)
	require.NoError(t, err)

	status, envelope := s.call(t, "/api", merchantID, createCard)

	require.Equal(t, http.StatusOK, status)
	assert.NotEmpty(t, cardToken(t, envelope))
}

// The client the Payme side calls a merchant with is built per stand, and every
// one of them shares a connection pool and one sequence of call identifiers.
//
// The identifiers are what a merchant matches a retry by, so two clients handing
// out the same one would make two different calls look like one.
func TestMerchantClientsBuildsAClientPerStandOnOneSequence(t *testing.T) {
	var ids []any

	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&envelope))
		ids = append(ids, envelope["id"])

		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"allow":true}}`))
	}))
	defer merchant.Close()

	build := merchantClients()

	first := build(merchant.URL, "key")
	second := build(merchant.URL, "other-key")

	require.NoError(t, first.CheckPerformTransaction(context.Background(), 1000, nil))
	require.NoError(t, second.CheckPerformTransaction(context.Background(), 1000, nil))

	require.Len(t, ids, 2)
	assert.NotEqual(t, ids[0], ids[1], "two calls are two identifiers, whichever stand made them")
}

// ---------- the parts, where the whole cannot reach them ----------

// Reaching the handler with no stand on the context is a wiring mistake rather
// than a caller's error.
func TestHandlerCacheRefusesAnUnscopedRequest(t *testing.T) {
	cache := newHandlerCache(nil, dependencies{}, clock.New(), "")

	rec := httptest.NewRecorder()
	cache.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api", nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// A stand whose profile cannot be read has no settings to build a handler from,
// and the call is refused rather than answered by rules nobody chose.
func TestE2ESubscribeRefusesACallForAnUnreadableProfile(t *testing.T) {
	s := newStand(t)

	_, err := s.pool.Exec(s.ctx, `
		UPDATE control.configs SET settings = '"not an object"'::jsonb
		WHERE id = (SELECT active_config_id FROM control.sandboxes WHERE id = $1)`,
		s.sandboxID)
	require.NoError(t, err)

	status, _ := s.call(t, "/api", merchantID, createCard)

	assert.Equal(t, http.StatusInternalServerError, status)
}

func TestE2EHandlerCacheReportsAProfileThatIsNotThere(t *testing.T) {
	s := newStand(t)
	cache := newHandlerCache(s.pool, dependencies{}, clock.New(), "")

	_, err := cache.loadSettings(s.ctx, 999999)

	assert.ErrorContains(t, err, "load profile")
}

// The endpoint a stand's calls go out to is the same URL the console shows and
// a cash register would be configured with, whether or not the base carries a
// trailing slash.
func TestMerchantEndpointNamesTheStand(t *testing.T) {
	for _, base := range []string{"http://merchant:8081", "http://merchant:8081/"} {
		cache := newHandlerCache(nil, dependencies{}, clock.New(), base)

		assert.Equal(t, "http://merchant:8081/s/qa/payme/merchant", cache.merchantEndpoint("qa"))
	}
}

// The credential a call is checked against is the stand's, so a context
// carrying no stand has none to offer and says so.
func TestResolveCredentialsNeedAStand(t *testing.T) {
	_, err := resolveCredentials(context.Background())
	assert.ErrorIs(t, err, sandboxctx.ErrNoSandbox)

	creds, err := resolveCredentials(sandboxctx.With(context.Background(),
		sandboxctx.Sandbox{MerchantID: merchantID, TestKey: testKey}))
	require.NoError(t, err)
	assert.Equal(t, merchantID, creds.MerchantID)
	assert.Equal(t, testKey, creds.Key, "the Subscribe API is answered with the test key")
}

// A stand with no profile is zero downstream, which is what "run on the
// defaults" is spelled as.
func TestConfigIDFlattensTheOptionalProfile(t *testing.T) {
	assert.Zero(t, configID(&sandboxdomain.Sandbox{}))

	id := int64(7)
	assert.Equal(t, id, configID(&sandboxdomain.Sandbox{ConfigID: &id}))
}

// ---------- the service itself ----------

// The service boots the way its own main boots it, answers a real call on the
// address it was given, and stops when it is told to.
func TestE2EServiceStartsServesAndStops(t *testing.T) {
	pool, url := testdb.NewWithURL(t)
	testdb.Seed(t, pool, standSlug)

	addr := freeAddress(t)

	t.Setenv("DATABASE_URL", url)
	t.Setenv("HTTP_ADDR", addr)
	t.Setenv("MIGRATE_ON_START", "true")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode == http.StatusOK
	}, 30*time.Second, 100*time.Millisecond, "the service never came up")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+addr+"/api", bytes.NewReader([]byte(createCard)))
	require.NoError(t, err)
	req.Header.Set("X-Auth", merchantID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	assert.Contains(t, string(body), "token", "the running service answers: %s", string(body))

	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the service did not stop when it was told to")
	}
}

// A shutdown that runs out of time is reported rather than swallowed: a process
// that leaves saying nothing looks the same as one that stopped cleanly, and
// the difference is a request that was cut off.
func TestE2EServiceReportsAShutdownThatRanOutOfTime(t *testing.T) {
	pool, url := testdb.NewWithURL(t)
	testdb.Seed(t, pool, standSlug)

	addr := freeAddress(t)

	t.Setenv("DATABASE_URL", url)
	t.Setenv("HTTP_ADDR", addr)
	t.Setenv("MIGRATE_ON_START", "false")
	t.Setenv("SHUTDOWN_TIMEOUT", "1ns")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	// A rule that stalls for a minute holds a connection open, so the shutdown
	// has something it cannot finish inside its window.
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode == http.StatusOK
	}, 30*time.Second, 100*time.Millisecond)

	_, err := pool.Exec(context.Background(), `
		INSERT INTO control.fault_rules
			(sandbox_id, name, service, method, action, delay_ms, enabled)
		SELECT id, 'a long stall', 'paymemock', '*', 'delay', 60000, TRUE
		FROM control.sandboxes WHERE slug = $1`, standSlug)
	require.NoError(t, err)

	stalling := make(chan struct{})
	go func() {
		defer close(stalling)

		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api",
			bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":9,"method":"cards.check","params":{"token":"nope"}}`)))
		if err != nil {
			return
		}
		req.Header.Set("X-Auth", merchantID)

		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.Error(t, err, "a shutdown that could not finish says so")
	case <-time.After(90 * time.Second):
		t.Fatal("the service never stopped")
	}

	<-stalling
}

// freeAddress is an address nothing is listening on, taken and released so the
// service under test can bind it.
func freeAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	return addr
}

// Migrations that will not apply stop the service: serving on a schema the code
// does not match would answer every call with a failure about a column.
func TestE2EServiceReportsMigrationsItCannotApply(t *testing.T) {
	pool, url := testdb.NewWithURL(t)

	_, err := pool.Exec(context.Background(), `DROP TABLE goose_db_version`)
	require.NoError(t, err)

	t.Setenv("DATABASE_URL", url)
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("MIGRATE_ON_START", "true")

	err = run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	assert.Error(t, err)
}

func TestServiceReportsADatabaseItCannotReach(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://payme:payme@127.0.0.1:1/paymemock?sslmode=disable&connect_timeout=1")

	err := run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	assert.Error(t, err)
}

func TestServiceReportsAConfigurationItCannotRead(t *testing.T) {
	t.Setenv("SHUTDOWN_TIMEOUT", "not a duration")

	err := run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	assert.Error(t, err)
}

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

// The health check answers without a stand, because it is what a container
// runtime asks before any stand exists.
func TestE2ESubscribeHealthz(t *testing.T) {
	s := newStand(t)

	resp, err := s.server.Client().Get(s.server.URL + "/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "ok", string(body))
}
