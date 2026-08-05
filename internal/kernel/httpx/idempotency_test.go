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

	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
)

// memCalls is the store the middleware needs, kept in memory.
type memCalls struct {
	rows map[httpx.CallKey][]byte
}

func newMemCalls() *memCalls { return &memCalls{rows: map[httpx.CallKey][]byte{}} }

func (m *memCalls) Recall(_ context.Context, key httpx.CallKey, _ time.Duration) ([]byte, bool, error) {
	body, ok := m.rows[key]
	return body, ok, nil
}

func (m *memCalls) Remember(_ context.Context, key httpx.CallKey, response []byte) error {
	m.rows[key] = response
	return nil
}

const oneSandbox = 7

func standing(_ context.Context) (int64, bool) { return oneSandbox, true }

// counting answers a fresh receipt id per call, so a replayed response can be
// told from a second one done for real.
func counting(hits *int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*hits++
		_, _ = w.Write([]byte(body))
	})
}

func post(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/api", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return rec
}

// A payout asked for twice is two payouts, and the caller cannot tell a lost
// response from a lost request. The repeat is answered with the first answer.
func TestIdempotencyReplaysARepeat(t *testing.T) {
	store := newMemCalls()
	hits := 0

	middleware := httpx.NewIdempotencyMiddleware(store,
		[]string{"transactions.create"}, time.Hour, standing)
	handler := middleware.Wrap(counting(&hits, `{"result":{"receipt":{"_id":"one"}}}`))

	call := `{"id":42,"method":"transactions.create","params":{"amount":100}}`

	first := post(t, handler, call)
	second := post(t, handler, call)

	require.Equal(t, 1, hits, "the work is done once")
	assert.Equal(t, first.Body.String(), second.Body.String(),
		"the repeat is answered with the first answer, byte for byte")
}

// An id reused for different parameters is a different intention, and answering
// it with the earlier response would hide the mistake rather than the retry.
func TestIdempotencyDoesNotReplayADifferentCall(t *testing.T) {
	store := newMemCalls()
	hits := 0

	middleware := httpx.NewIdempotencyMiddleware(store,
		[]string{"transactions.create"}, time.Hour, standing)
	handler := middleware.Wrap(counting(&hits, `{"result":{"receipt":{"_id":"one"}}}`))

	post(t, handler, `{"id":42,"method":"transactions.create","params":{"amount":100}}`)
	post(t, handler, `{"id":42,"method":"transactions.create","params":{"amount":900}}`)

	assert.Equal(t, 2, hits)
}

// A refusal is the stand's answer to the state it was in. An operator who fixes
// that state — unblocks the register, tops it up — must be able to retry.
func TestIdempotencyDoesNotStoreARefusal(t *testing.T) {
	store := newMemCalls()
	hits := 0

	middleware := httpx.NewIdempotencyMiddleware(store,
		[]string{"transactions.create"}, time.Hour, standing)
	handler := middleware.Wrap(counting(&hits, `{"error":{"code":-31008}}`))

	call := `{"id":42,"method":"transactions.create","params":{"amount":100}}`
	post(t, handler, call)
	post(t, handler, call)

	assert.Equal(t, 2, hits, "a refusal is not an answer worth replaying")
	assert.Empty(t, store.rows)
}

// Reads are left alone: answering cards.check out of a store would hide the
// very state an operator changed between two calls.
func TestIdempotencyLeavesUnlistedMethodsAlone(t *testing.T) {
	store := newMemCalls()
	hits := 0

	middleware := httpx.NewIdempotencyMiddleware(store,
		[]string{"transactions.create"}, time.Hour, standing)
	handler := middleware.Wrap(counting(&hits, `{"result":{"card":{}}}`))

	call := `{"id":42,"method":"cards.check","params":{"token":"x"}}`
	post(t, handler, call)
	post(t, handler, call)

	assert.Equal(t, 2, hits)
}

// Off is a setting, and the provider's own behaviour: a rehearsal of what
// duplicate payouts do to a merchant needs the duplicates.
func TestIdempotencyIsOffWithoutAWindow(t *testing.T) {
	store := newMemCalls()
	hits := 0

	middleware := httpx.NewIdempotencyMiddleware(store,
		[]string{"transactions.create"}, 0, standing)
	handler := middleware.Wrap(counting(&hits, `{"result":{"receipt":{"_id":"one"}}}`))

	call := `{"id":42,"method":"transactions.create","params":{"amount":100}}`
	post(t, handler, call)
	post(t, handler, call)

	assert.Equal(t, 2, hits)
}

// A call with no id names no intention, so there is nothing to recognise a
// repeat of it by.
func TestIdempotencyNeedsAnID(t *testing.T) {
	store := newMemCalls()
	hits := 0

	middleware := httpx.NewIdempotencyMiddleware(store,
		[]string{"transactions.create"}, time.Hour, standing)
	handler := middleware.Wrap(counting(&hits, `{"result":{"receipt":{"_id":"one"}}}`))

	call := `{"method":"transactions.create","params":{"amount":100}}`
	post(t, handler, call)
	post(t, handler, call)

	assert.Equal(t, 2, hits)
}

// Everything the middleware declines to recognise, and the one failure it has
// to survive: a body that is not JSON at all, a request outside any stand, an
// empty body, and a store that cannot be read. None of them may swallow the
// call — a repeat that cannot be recognised is simply done again, which is what
// the API did before any of this existed.
func TestIdempotencyLetsThroughWhatItCannotRecognise(t *testing.T) {
	nowhere := func(_ context.Context) (int64, bool) { return 0, false }

	tests := []struct {
		name    string
		body    string
		sandbox func(context.Context) (int64, bool)
		store   httpx.CallStore
	}{
		{name: "not JSON", body: "{{{", sandbox: standing, store: newMemCalls()},
		{name: "no body", body: "", sandbox: standing, store: newMemCalls()},
		{
			name:    "no stand to answer as",
			body:    `{"id":1,"method":"transactions.create"}`,
			sandbox: nowhere,
			store:   newMemCalls(),
		},
		{
			name:    "a store that cannot be read",
			body:    `{"id":1,"method":"transactions.create"}`,
			sandbox: standing,
			store:   brokenCalls{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits := 0
			handler := httpx.NewIdempotencyMiddleware(tt.store,
				[]string{"transactions.create"}, time.Hour, tt.sandbox).
				Wrap(counting(&hits, `{"result":{"receipt":{"_id":"one"}}}`))

			post(t, handler, tt.body)
			post(t, handler, tt.body)

			assert.Equal(t, 2, hits, "the call is done rather than lost")
		})
	}
}

// A response that is not JSON is not one this can replay, and storing it would
// hand the next caller something no client could read.
func TestIdempotencyDoesNotStoreAnUnreadableResponse(t *testing.T) {
	store := newMemCalls()
	hits := 0

	handler := httpx.NewIdempotencyMiddleware(store,
		[]string{"transactions.create"}, time.Hour, standing).
		Wrap(counting(&hits, "not json"))

	call := `{"id":42,"method":"transactions.create"}`
	post(t, handler, call)
	post(t, handler, call)

	assert.Equal(t, 2, hits)
	assert.Empty(t, store.rows)
}

// Listing no methods is the same as switching it off, which is what a service
// wired without the feature passes.
func TestIdempotencyIsOffWithoutMethods(t *testing.T) {
	hits := 0
	handler := httpx.NewIdempotencyMiddleware(newMemCalls(), nil, time.Hour, standing).
		Wrap(counting(&hits, `{"result":{}}`))

	call := `{"id":42,"method":"transactions.create"}`
	post(t, handler, call)
	post(t, handler, call)

	assert.Equal(t, 2, hits)
}

// brokenCalls is a store that answers nothing but failure, which is what a
// database that has gone away looks like from here.
type brokenCalls struct{}

func (brokenCalls) Recall(context.Context, httpx.CallKey, time.Duration) ([]byte, bool, error) {
	return nil, false, errBroken
}

func (brokenCalls) Remember(context.Context, httpx.CallKey, []byte) error { return errBroken }

var errBroken = errors.New("the store is gone")
