package httpx_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	fault "github.com/bakhod1r/payme-mock/internal/context/simulation/fault/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
)

// recordingSleeper notes how long the middleware asked to wait without
// actually waiting, so a two-minute delay costs a test nothing.
type recordingSleeper struct{ waited time.Duration }

func (s *recordingSleeper) Sleep(_ context.Context, d time.Duration) { s.waited += d }

type stubEvaluator struct {
	decision fault.Decision
	err      error
	seen     fault.Request
}

func (s *stubEvaluator) Evaluate(_ context.Context, req fault.Request) (fault.Decision, error) {
	s.seen = req
	return s.decision, s.err
}

// countingHandler is the protected handler; it reports how often it ran.
type countingHandler struct{ calls atomic.Int32 }

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.calls.Add(1)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"result":{"allow":true},"id":1}`))
}

func describeAll(*http.Request, []byte) fault.Request {
	return fault.Request{Service: fault.ServiceMerchant, Method: "PerformTransaction"}
}

type stand struct {
	handler http.Handler
	inner   *countingHandler
	sleeper *recordingSleeper
	eval    *stubEvaluator
}

func newStand(decision fault.Decision, err error) *stand {
	inner := &countingHandler{}
	sleeper := &recordingSleeper{}
	eval := &stubEvaluator{decision: decision, err: err}
	mw := httpx.NewFaultMiddleware(eval, describeAll, sleeper)

	return &stand{handler: mw.Wrap(inner), inner: inner, sleeper: sleeper, eval: eval}
}

func (s *stand) call(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/payme/merchant",
		strings.NewReader(`{"method":"PerformTransaction","params":{"id":"x"},"id":1}`))
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

func ruleWith(action fault.Action) *fault.Rule {
	return &fault.Rule{ID: 1, Enabled: true, Action: action, Probability: 1}
}

func TestNoRuleLetsTheRequestThrough(t *testing.T) {
	s := newStand(fault.Decision{}, nil)

	rec := s.call(t)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int32(1), s.inner.calls.Load())
	assert.JSONEq(t, `{"result":{"allow":true},"id":1}`, rec.Body.String())
}

// The console's headline case: "make this API answer in two minutes".
func TestDelayActionWaitsThenAnswersNormally(t *testing.T) {
	rule := ruleWith(fault.ActionDelay)
	rule.DelayMillis = 120000
	s := newStand(fault.Decision{Rule: rule, DelayMillis: 120000}, nil)

	rec := s.call(t)

	assert.Equal(t, 2*time.Minute, s.sleeper.waited)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int32(1), s.inner.calls.Load())
	assert.JSONEq(t, `{"result":{"allow":true},"id":1}`, rec.Body.String())
}

// A delay applies to faulted responses too, so an error can be made slow.
func TestDelayAlsoAppliesToAnErrorAction(t *testing.T) {
	rule := ruleWith(fault.ActionRPCError)
	rule.ErrorCode = payerr.CodeCannotPerform
	s := newStand(fault.Decision{Rule: rule, DelayMillis: 5000}, nil)

	rec := s.call(t)

	assert.Equal(t, 5*time.Second, s.sleeper.waited)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int32(0), s.inner.calls.Load())
}

func TestRPCErrorActionAnswersWithTheChosenCode(t *testing.T) {
	rule := ruleWith(fault.ActionRPCError)
	rule.ErrorCode = payerr.CodeCannotPerform
	s := newStand(fault.Decision{Rule: rule}, nil)

	rec := s.call(t)

	require.Equal(t, http.StatusOK, rec.Code, "a protocol error still travels as HTTP 200")
	assert.Equal(t, int32(0), s.inner.calls.Load(), "the real handler must not run")

	var body struct {
		Error struct {
			Code    payerr.Code    `json:"code"`
			Message payerr.Message `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, payerr.CodeCannotPerform, body.Error.Code)
	assert.True(t, body.Error.Message.Complete(), "an injected error still ships all three languages")
}

func TestHTTPStatusAction(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		want       int
	}{
		{"explicit status", http.StatusInternalServerError, http.StatusInternalServerError},
		{"unauthorized", http.StatusUnauthorized, http.StatusUnauthorized},
		{"unset falls back to 500", 0, http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := ruleWith(fault.ActionHTTPStatus)
			rule.HTTPStatus = tt.configured
			s := newStand(fault.Decision{Rule: rule}, nil)

			rec := s.call(t)

			assert.Equal(t, tt.want, rec.Code)
			assert.Equal(t, int32(0), s.inner.calls.Load())
		})
	}
}

func TestMalformedActionAnswersUnparseableJSON(t *testing.T) {
	s := newStand(fault.Decision{Rule: ruleWith(fault.ActionMalformed)}, nil)

	rec := s.call(t)

	assert.Equal(t, http.StatusOK, rec.Code)
	var any map[string]any
	assert.Error(t, json.Unmarshal(rec.Body.Bytes(), &any), "the body must not parse")
}

func TestDropActionAnswersNothingWhenHijackIsUnavailable(t *testing.T) {
	s := newStand(fault.Decision{Rule: ruleWith(fault.ActionDrop)}, nil)

	rec := s.call(t)

	assert.Empty(t, rec.Body.String())
	assert.Equal(t, int32(0), s.inner.calls.Load())
}

// A connection that claims to be hijackable but refuses is left alone rather
// than answered, so the caller still observes a dead request.
func TestDropActionSurvivesAFailingHijack(t *testing.T) {
	s := newStand(fault.Decision{Rule: ruleWith(fault.ActionDrop)}, nil)

	w := &refusingHijacker{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodPost, "/payme/merchant", strings.NewReader(`{}`))

	s.handler.ServeHTTP(w, req)

	assert.True(t, w.attempted, "the middleware must try to hijack")
	assert.Empty(t, w.Body.String())
	assert.Equal(t, int32(0), s.inner.calls.Load())
}

// Over a real connection the drop action closes the socket, so the caller sees
// a broken connection rather than any HTTP response — a genuine timeout.
func TestDropActionClosesARealConnection(t *testing.T) {
	inner := &countingHandler{}
	mw := httpx.NewFaultMiddleware(
		&stubEvaluator{decision: fault.Decision{Rule: ruleWith(fault.ActionDrop)}},
		describeAll, &recordingSleeper{},
	)
	srv := httptest.NewServer(mw.Wrap(inner))
	defer srv.Close()

	resp, err := srv.Client().Post(srv.URL, "application/json", strings.NewReader(`{}`)) //nolint:noctx // short-lived test call
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
	}

	require.Error(t, err, "the connection must be closed without a response")
	assert.Equal(t, int32(0), inner.calls.Load())
}

// The duplicate action delivers the same request twice, which is exactly what
// the idempotency requirement has to survive.
func TestDuplicateActionDeliversTheRequestTwice(t *testing.T) {
	s := newStand(fault.Decision{Rule: ruleWith(fault.ActionDuplicate)}, nil)

	rec := s.call(t)

	assert.Equal(t, int32(2), s.inner.calls.Load())
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"result":{"allow":true},"id":1}`, rec.Body.String(),
		"only the second delivery's response reaches the caller")
}

func TestPassthroughRuleDoesNotAlterTheResponse(t *testing.T) {
	s := newStand(fault.Decision{Rule: ruleWith(fault.ActionPassthrough)}, nil)

	rec := s.call(t)

	assert.Equal(t, int32(1), s.inner.calls.Load())
	assert.JSONEq(t, `{"result":{"allow":true},"id":1}`, rec.Body.String())
}

func TestUnknownActionFallsThroughToTheHandler(t *testing.T) {
	s := newStand(fault.Decision{Rule: ruleWith(fault.Action("teleport"))}, nil)

	rec := s.call(t)

	assert.Equal(t, int32(1), s.inner.calls.Load())
	assert.Equal(t, http.StatusOK, rec.Code)
}

// A fault layer that cannot read its rules must not take the stand down.
func TestEvaluatorFailureLetsTheRequestThrough(t *testing.T) {
	s := newStand(fault.Decision{}, errors.New("rule store unavailable"))

	rec := s.call(t)

	assert.Equal(t, int32(1), s.inner.calls.Load())
	assert.Equal(t, http.StatusOK, rec.Code)
}

// The handler downstream must still see a complete body after the middleware
// has read it for rule matching.
func TestBodyIsRestoredForTheHandler(t *testing.T) {
	const payload = `{"method":"PerformTransaction","params":{"id":"5305e3bab097f420a62ced0b"},"id":1}`
	var seen string

	mw := httpx.NewFaultMiddleware(&stubEvaluator{}, describeAll, &recordingSleeper{})
	handler := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		buf := make([]byte, len(payload))
		n, _ := r.Body.Read(buf)
		seen = string(buf[:n])
	}))

	req := httptest.NewRequest(http.MethodPost, "/payme/merchant", strings.NewReader(payload))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, payload, seen)
}

func TestBodylessRequestIsHandled(t *testing.T) {
	s := newStand(fault.Decision{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/payme/merchant", nil)
	req.Body = nil
	rec := httptest.NewRecorder()

	s.handler.ServeHTTP(rec, req)

	assert.Equal(t, int32(1), s.inner.calls.Load())
}

func TestUnreadableBodyLetsTheRequestThrough(t *testing.T) {
	s := newStand(fault.Decision{Rule: ruleWith(fault.ActionDrop)}, nil)

	req := httptest.NewRequest(http.MethodPost, "/payme/merchant", failingBody{})
	rec := httptest.NewRecorder()

	s.handler.ServeHTTP(rec, req)

	assert.Equal(t, int32(1), s.inner.calls.Load(), "an unreadable body bypasses the fault layer")
}

func TestOnDecisionReportsEveryOutcome(t *testing.T) {
	var got []fault.Action
	rule := ruleWith(fault.ActionRPCError)
	rule.ErrorCode = payerr.CodeCannotPerform

	inner := &countingHandler{}
	mw := httpx.NewFaultMiddleware(
		&stubEvaluator{decision: fault.Decision{Rule: rule}},
		describeAll, &recordingSleeper{},
	)
	mw.OnDecision(func(_ context.Context, d fault.Decision) { got = append(got, d.Action()) })
	handler := mw.Wrap(inner)

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, []fault.Action{fault.ActionRPCError}, got)
}

func TestRealSleeper(t *testing.T) {
	s := httpx.RealSleeper{}

	t.Run("a zero delay returns at once", func(t *testing.T) {
		start := time.Now()

		s.Sleep(context.Background(), 0)

		assert.Less(t, time.Since(start), 50*time.Millisecond)
	})

	t.Run("waits for the duration", func(t *testing.T) {
		start := time.Now()

		s.Sleep(context.Background(), 20*time.Millisecond)

		assert.GreaterOrEqual(t, time.Since(start), 20*time.Millisecond)
	})

	t.Run("a client that goes away cuts the wait short", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()

		s.Sleep(ctx, time.Hour)

		assert.Less(t, time.Since(start), time.Second)
	})
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (failingBody) Close() error             { return nil }

// refusingHijacker advertises hijack support and then refuses, which is what a
// server does when the connection has already been taken over.
type refusingHijacker struct {
	*httptest.ResponseRecorder
	attempted bool
}

func (h *refusingHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.attempted = true
	return nil, nil, errors.New("connection already hijacked")
}
