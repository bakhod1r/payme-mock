package httpx_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	fault "github.com/bakhod1r/payme-mock/internal/context/simulation/fault/domain"
	traffic "github.com/bakhod1r/payme-mock/internal/context/simulation/traffic/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// recorder collects what the middleware wrote instead of storing it.
type recorder struct {
	entries []traffic.Entry
	err     error
}

func (r *recorder) Record(_ context.Context, e traffic.Entry) error {
	r.entries = append(r.entries, e)
	return r.err
}

// stepClock advances by a fixed amount on every reading, so a recorded
// duration is exact rather than however long the test happened to take.
type stepClock struct {
	now  time.Time
	step time.Duration
}

func (c *stepClock) Now() time.Time {
	c.now = c.now.Add(c.step)
	return c.now
}

func (c *stepClock) NowMillis() int64 { return clock.ToMillis(c.now) }

// Sleep advances the clock instead of blocking; nothing in these tests waits.
func (c *stepClock) Sleep(_ context.Context, d time.Duration) { c.now = c.now.Add(d) }

func newClock(step time.Duration) *stepClock {
	return &stepClock{now: time.Unix(0, 0), step: step}
}

// serve runs one request through the middleware and returns what was recorded.
func serve(t *testing.T, rec *recorder, clk clock.Clock, body string, next http.Handler, decorate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()

	var failed []error
	middleware := httpx.NewTrafficMiddleware(rec, "merchant", clk, func(err error) {
		failed = append(failed, err)
	})

	r := httptest.NewRequest(http.MethodPost, "/payme/merchant", strings.NewReader(body))
	r.RemoteAddr = "10.0.0.7:5000"
	if decorate != nil {
		decorate(r)
	}

	w := httptest.NewRecorder()
	middleware.Wrap(next).ServeHTTP(w, r)

	if rec.err != nil {
		require.Len(t, failed, 1, "a failed write is reported to the operator")
		assert.ErrorIs(t, failed[0], rec.err)
	} else {
		assert.Empty(t, failed)
	}

	return w
}

func okHandler(status int, response string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	})
}

func TestTrafficMiddlewareRecordsTheCall(t *testing.T) {
	rec := &recorder{}

	w := serve(t, rec, newClock(5*time.Millisecond),
		`{"id":42,"method":"CheckTransaction"}`,
		okHandler(http.StatusOK, `{"result":{"state":2}}`), nil)

	require.Len(t, rec.entries, 1)
	entry := rec.entries[0]

	assert.Equal(t, "merchant", entry.Service)
	assert.Equal(t, traffic.DirectionIn, entry.Direction)
	assert.Equal(t, "CheckTransaction", entry.Method)
	assert.Equal(t, "42", entry.RPCID)
	assert.Equal(t, http.StatusOK, entry.HTTPStatus)
	assert.JSONEq(t, `{"id":42,"method":"CheckTransaction"}`, string(entry.RequestBody))
	assert.JSONEq(t, `{"result":{"state":2}}`, string(entry.ResponseBody))
	assert.Equal(t, 5, entry.DurationMS)
	assert.Equal(t, "10.0.0.7:5000", entry.RemoteAddr)
	assert.Nil(t, entry.SandboxID)
	assert.Nil(t, entry.ErrorCode)
	assert.Nil(t, entry.FaultRuleID)

	// The response reaches the client unchanged.
	assert.JSONEq(t, `{"result":{"state":2}}`, w.Body.String())
}

// The handler reads the body too, so the middleware must put it back.
func TestTrafficMiddlewareLeavesTheBodyReadable(t *testing.T) {
	rec := &recorder{}
	var seen string

	serve(t, rec, newClock(time.Millisecond), `{"method":"CheckTransaction"}`,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := make([]byte, 64)
			n, _ := r.Body.Read(raw)
			seen = string(raw[:n])
			w.WriteHeader(http.StatusOK)
		}), nil)

	assert.JSONEq(t, `{"method":"CheckTransaction"}`, seen)
}

// The protocol reports errors inside a 200, so the status alone cannot say
// whether a call succeeded.
func TestTrafficMiddlewareRecordsAProtocolErrorInsideA200(t *testing.T) {
	rec := &recorder{}

	serve(t, rec, newClock(time.Millisecond), `{"id":"a1","method":"PerformTransaction"}`,
		okHandler(http.StatusOK, `{"error":{"code":-31008},"id":"a1"}`), nil)

	require.Len(t, rec.entries, 1)
	require.NotNil(t, rec.entries[0].ErrorCode)
	assert.Equal(t, -31008, *rec.entries[0].ErrorCode)
	assert.Equal(t, "a1", rec.entries[0].RPCID, "a string id is recorded unquoted")
}

func TestTrafficMiddlewareRecordsTheSandbox(t *testing.T) {
	rec := &recorder{}

	serve(t, rec, newClock(time.Millisecond), `{"method":"CheckTransaction"}`,
		okHandler(http.StatusOK, `{}`), func(r *http.Request) {
			ctx := sandboxctx.With(r.Context(), sandboxctx.Sandbox{ID: 7, Slug: "qa"})
			*r = *r.WithContext(ctx)
		})

	require.Len(t, rec.entries, 1)
	require.NotNil(t, rec.entries[0].SandboxID)
	assert.Equal(t, int64(7), *rec.entries[0].SandboxID)
}

// A body that is not JSON-RPC still describes a request; it simply has no
// method to record.
func TestTrafficMiddlewareRecordsABodyThatIsNotJSON(t *testing.T) {
	rec := &recorder{}

	serve(t, rec, newClock(time.Millisecond), `not json at all`,
		okHandler(http.StatusBadRequest, `also not json`), nil)

	require.Len(t, rec.entries, 1)
	assert.Empty(t, rec.entries[0].Method)
	assert.Empty(t, rec.entries[0].RPCID)
	assert.Nil(t, rec.entries[0].ErrorCode)
	assert.Equal(t, http.StatusBadRequest, rec.entries[0].HTTPStatus)
}

// A handler that never sets a status has answered 200, which is what the
// client saw and therefore what the log must say.
func TestTrafficMiddlewareDefaultsToOK(t *testing.T) {
	rec := &recorder{}

	serve(t, rec, newClock(time.Millisecond), `{}`,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"result":1}`))
		}), nil)

	require.Len(t, rec.entries, 1)
	assert.Equal(t, http.StatusOK, rec.entries[0].HTTPStatus)
}

// Only the first status reaches the client, so a second one must not be the
// one recorded.
func TestTrafficMiddlewareRecordsTheFirstStatusOnly(t *testing.T) {
	rec := &recorder{}

	serve(t, rec, newClock(time.Millisecond), `{}`,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
			w.WriteHeader(http.StatusInternalServerError)
		}), nil)

	require.Len(t, rec.entries, 1)
	assert.Equal(t, http.StatusTeapot, rec.entries[0].HTTPStatus)
}

// A body larger than the cap is delivered in full; only what is kept for the
// log is limited.
func TestTrafficMiddlewareTruncatesALargeBody(t *testing.T) {
	rec := &recorder{}
	const cap = 64 << 10
	large := strings.Repeat("x", cap+500)

	w := serve(t, rec, newClock(time.Millisecond), large, okHandler(http.StatusOK, large), nil)

	require.Len(t, rec.entries, 1)
	assert.Len(t, rec.entries[0].RequestBody, cap)
	assert.Len(t, rec.entries[0].ResponseBody, cap)
	assert.Len(t, w.Body.String(), cap+500, "the client still receives everything")
}

// The rule that shaped a response is named on the record, which is what lets
// the traffic screen say why a call failed.
func TestTrafficMiddlewareNamesTheRuleThatFired(t *testing.T) {
	rec := &recorder{}

	serve(t, rec, newClock(time.Millisecond), `{"method":"CheckTransaction"}`,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpx.ReportFiredRule(r.Context(), &fault.Rule{ID: 99})
			w.WriteHeader(http.StatusOK)
		}), nil)

	require.Len(t, rec.entries, 1)
	require.NotNil(t, rec.entries[0].FaultRuleID)
	assert.Equal(t, int64(99), *rec.entries[0].FaultRuleID)
}

// Reporting a rule outside the middleware does nothing, so the fault layer
// works unwrapped too.
func TestReportFiredRuleWithoutTheMiddleware(t *testing.T) {
	ctx := context.Background()

	assert.NotPanics(t, func() { httpx.ReportFiredRule(ctx, &fault.Rule{ID: 1}) })

	_, ok := httpx.FiredRule(ctx)
	assert.False(t, ok)
}

// A request that was answered must not be failed because its record could not
// be written.
func TestTrafficMiddlewareSurvivesAFailedWrite(t *testing.T) {
	rec := &recorder{err: errors.New("database gone")}

	w := serve(t, rec, newClock(time.Millisecond), `{}`,
		okHandler(http.StatusOK, `{"result":1}`), nil)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"result":1}`, w.Body.String())
}

// A body that cannot be read cannot be handled either, so the request is
// passed on and left to fail where it would anyway.
func TestTrafficMiddlewarePassesOnAnUnreadableBody(t *testing.T) {
	rec := &recorder{}
	var reached bool

	middleware := httpx.NewTrafficMiddleware(rec, "merchant", newClock(time.Millisecond), nil)

	r := httptest.NewRequest(http.MethodPost, "/payme/merchant", badBody{})
	w := httptest.NewRecorder()

	middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(w, r)

	assert.True(t, reached)
	assert.Empty(t, rec.entries, "there is nothing to record about a request nobody could read")
}

// badBody fails on the first read, standing in for a connection that dropped
// mid-request.
type badBody struct{}

func (badBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (badBody) Close() error             { return nil }

// The drop action closes the connection, and the traffic log sits between the
// fault layer and the writer that owns it. Only a real server can prove the
// hijack still reaches through.
func TestTrafficMiddlewareLetsTheFaultLayerDropTheConnection(t *testing.T) {
	rec := &recorder{}

	middleware := httpx.NewTrafficMiddleware(rec, "merchant", newClock(time.Millisecond), nil)

	server := httptest.NewServer(middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		require.NoError(t, err, "the log must not hide the connection from the fault layer")
		require.NoError(t, conn.Close())
	})))
	defer server.Close()

	_, err := server.Client().Post(server.URL, "application/json", strings.NewReader(`{}`))

	// A closed connection with no response is what the caller sees as a timeout.
	assert.Error(t, err)
}
