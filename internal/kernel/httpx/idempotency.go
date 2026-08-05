package httpx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

// CallStore remembers what a call was answered, so a repeat can be answered the
// same way instead of doing the work twice.
type CallStore interface {
	// Recall returns the stored response for a call, reporting whether one was
	// found. A stored call whose body differs from this one is not a repeat and
	// must not be returned.
	Recall(ctx context.Context, key CallKey, window time.Duration) ([]byte, bool, error)
	// Remember stores what a call was answered.
	Remember(ctx context.Context, key CallKey, response []byte) error
}

// CallKey identifies one intention: which stand, which method, and the id the
// caller put on it.
type CallKey struct {
	SandboxID int64
	Method    string
	RequestID string
	BodyHash  string
}

// IdempotencyMiddleware answers a repeated call with the answer the first one
// got, for the methods that would otherwise do the work twice.
//
// The Merchant API has this in the protocol: CreateTransaction carries the
// payment's id and a repeat replays. The Subscribe API has no such field, so a
// payout asked for twice is two payouts — and an integration that lost the
// response to the first has no way to find out which. The JSON-RPC id is what
// the caller already sends once per intention, so that is what a repeat is
// recognised by here.
//
// It is off unless a window is set, because replaying is a decision about the
// stand: a rehearsal of a client's retry logic wants the replay, and a rehearsal
// of what the real provider does may not.
type IdempotencyMiddleware struct {
	store   CallStore
	methods map[string]bool
	window  time.Duration
	// sandbox reports which stand the request is for. It is supplied rather
	// than read here because the two services put it on the context by
	// different routes.
	sandbox func(ctx context.Context) (int64, bool)
}

// NewIdempotencyMiddleware wires the middleware to its store.
func NewIdempotencyMiddleware(
	store CallStore,
	methods []string,
	window time.Duration,
	sandbox func(ctx context.Context) (int64, bool),
) *IdempotencyMiddleware {
	set := make(map[string]bool, len(methods))
	for _, method := range methods {
		set[method] = true
	}

	return &IdempotencyMiddleware{store: store, methods: set, window: window, sandbox: sandbox}
}

// Wrap returns a handler that replays a repeat and records a first attempt.
func (m *IdempotencyMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := m.keyOf(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		if stored, found, err := m.store.Recall(r.Context(), key, m.window); err == nil && found {
			w.Header().Set("Content-Type", "text/json; charset=UTF-8")
			// A replay is the earlier answer, byte for byte. A caller matching
			// on anything in it — the receipt id above all — has to see what it
			// would have seen had the first response arrived.
			_, _ = w.Write(stored)
			return
		}

		capture := &captureWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(capture, r)

		// Only a call that got somewhere is worth replaying. A refusal is the
		// stand's answer to the state it was in at the time, and a retry after
		// the operator has fixed that state must be allowed to succeed.
		if capture.status == http.StatusOK && hasResult(capture.body.Bytes()) {
			_ = m.store.Remember(r.Context(), key, capture.body.Bytes())
		}
	})
}

// keyOf reads the call's identity, reporting false for anything this middleware
// leaves alone: a method not listed, a body with no id, or a request outside any
// stand.
func (m *IdempotencyMiddleware) keyOf(r *http.Request) (CallKey, bool) {
	if m.window <= 0 || len(m.methods) == 0 {
		return CallKey{}, false
	}

	body, err := readAndRestoreBody(r)
	if err != nil || len(body) == 0 {
		return CallKey{}, false
	}

	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return CallKey{}, false
	}

	if !m.methods[envelope.Method] || len(envelope.ID) == 0 {
		return CallKey{}, false
	}

	sandboxID, ok := m.sandbox(r.Context())
	if !ok {
		return CallKey{}, false
	}

	sum := sha256.Sum256(body)

	return CallKey{
		SandboxID: sandboxID,
		Method:    envelope.Method,
		// The id is kept as it arrived. Payme's own examples send it as a
		// number and clients send strings, and "1" from one is the same
		// intention as 1 from the other only if the caller says so.
		RequestID: string(envelope.ID),
		BodyHash:  hex.EncodeToString(sum[:]),
	}, true
}

// hasResult reports a JSON-RPC response that carries a result rather than an
// error.
func hasResult(body []byte) bool {
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return false
	}

	return len(envelope.Result) > 0
}
