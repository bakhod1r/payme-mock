package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	fault "github.com/bakhod1r/payme-mock/internal/context/simulation/fault/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/jsonrpc"
)

// Evaluator decides whether a request should be faulted.
type Evaluator interface {
	Evaluate(ctx context.Context, req fault.Request) (fault.Decision, error)
}

// RequestDescriber turns an HTTP request into the facts a rule matches on:
// which method was called, for which account, for how much. It is supplied per
// service because the merchant and Subscribe protocols carry them differently.
type RequestDescriber func(r *http.Request, body []byte) fault.Request

// Sleeper waits for a duration. Tests substitute one that records the wait
// instead of spending it.
type Sleeper interface {
	Sleep(ctx context.Context, d time.Duration)
}

// RealSleeper waits for real, aborting early if the client goes away.
type RealSleeper struct{}

// Sleep waits for d or until the context ends.
func (RealSleeper) Sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// FaultMiddleware applies fault rules before the real handler runs.
type FaultMiddleware struct {
	evaluator Evaluator
	describe  RequestDescriber
	sleeper   Sleeper
	// onDecision reports what happened so the traffic log can show which rule
	// fired. It is optional.
	onDecision func(ctx context.Context, d fault.Decision)
}

// NewFaultMiddleware builds the middleware.
func NewFaultMiddleware(e Evaluator, describe RequestDescriber, sleeper Sleeper) *FaultMiddleware {
	return &FaultMiddleware{evaluator: e, describe: describe, sleeper: sleeper}
}

// OnDecision registers a callback invoked with every decision, faulted or not.
func (m *FaultMiddleware) OnDecision(fn func(ctx context.Context, d fault.Decision)) {
	m.onDecision = fn
}

// Wrap returns next guarded by the fault layer.
func (m *FaultMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := readAndRestoreBody(r)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		decision, err := m.evaluator.Evaluate(r.Context(), m.describe(r, body))
		if err != nil {
			// A fault layer that cannot reach its rules must not take the
			// stand down; the request proceeds as if no rule matched.
			next.ServeHTTP(w, r)
			return
		}

		if m.onDecision != nil {
			m.onDecision(r.Context(), decision)
		}

		// The traffic log names the rule that shaped the response, so a
		// decision that fired is reported even before the response exists.
		if decision.Faulted() {
			ReportFiredRule(r.Context(), decision.Rule)
		}

		m.sleeper.Sleep(r.Context(), time.Duration(decision.DelayMillis)*time.Millisecond)

		if !decision.Faulted() {
			next.ServeHTTP(w, r)
			return
		}

		m.apply(w, r, next, decision)
	})
}

// apply carries out the decided action.
func (m *FaultMiddleware) apply(w http.ResponseWriter, r *http.Request, next http.Handler, d fault.Decision) {
	switch d.Rule.Action {
	case fault.ActionDelay:
		// The wait already happened; the request is otherwise untouched.
		next.ServeHTTP(w, r)

	case fault.ActionRPCError:
		writeJSON(w, http.StatusOK, jsonrpc.NewError(nil, d.Rule.ProtocolError()))

	case fault.ActionHTTPStatus:
		status := d.Rule.HTTPStatus
		if status == 0 {
			status = http.StatusInternalServerError
		}
		w.WriteHeader(status)

	case fault.ActionMalformed:
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":`))

	case fault.ActionDrop:
		dropConnection(w)

	case fault.ActionDuplicate:
		// The handler runs twice against a discarded first response, which is
		// how a repeated delivery reaches the idempotency logic.
		next.ServeHTTP(&discardWriter{header: make(http.Header)}, r)
		next.ServeHTTP(w, r)

	default:
		next.ServeHTTP(w, r)
	}
}

// dropConnection closes the connection without a response, which the caller
// sees as a timeout. When the connection cannot be hijacked — as under
// httptest's recorder — an empty body is the closest equivalent.
//
// The controller is used rather than a type assertion because this handler
// runs wrapped by the traffic log, and only the controller follows Unwrap down
// to the writer that actually owns the connection.
func dropConnection(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// discardWriter swallows a response. It backs the duplicate action, where the
// first delivery's reply is thrown away exactly as a lost network reply would be.
type discardWriter struct {
	header http.Header
	status int
}

func (d *discardWriter) Header() http.Header       { return d.header }
func (*discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardWriter) WriteHeader(status int)    { d.status = status }
