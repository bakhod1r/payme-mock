package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	fault "github.com/bakhod1r/payme-mock/internal/context/simulation/fault/domain"
	traffic "github.com/bakhod1r/payme-mock/internal/context/simulation/traffic/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// maxRecordedBody caps what the traffic log keeps of a body. Payme's own
// messages are small; a larger one is truncated rather than filling the log.
const maxRecordedBody = 64 << 10 // 64 KiB

// TrafficMiddleware records every request and response the service handled.
//
// It wraps the fault layer rather than sitting inside it, so a faulted
// response is logged as what the caller actually received.
type TrafficMiddleware struct {
	recorder traffic.Recorder
	service  string
	clock    clock.Clock
	// onError reports a failed write. Recording must never fail a request that
	// otherwise succeeded, so the error is surfaced here and nowhere else.
	onError func(error)
}

// NewTrafficMiddleware builds the middleware for one service.
func NewTrafficMiddleware(r traffic.Recorder, service string, clk clock.Clock, onError func(error)) *TrafficMiddleware {
	return &TrafficMiddleware{recorder: r, service: service, clock: clk, onError: onError}
}

// Wrap returns next with its traffic recorded.
func (m *TrafficMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := readAndRestoreBody(r)
		if err != nil {
			// A body that cannot be read cannot be handled either, so the
			// request is passed on and left to fail where it would anyway.
			next.ServeHTTP(w, r)
			return
		}

		started := m.clock.Now()
		capture := &captureWriter{ResponseWriter: w, status: http.StatusOK}

		// The fault layer runs inside this one and fills the slot in with the
		// rule it applied, which is how the record can name it.
		slot := &firedRule{}
		r = r.WithContext(withFiredRule(r.Context(), slot))

		next.ServeHTTP(capture, r)

		m.record(r, body, capture, m.clock.Now().Sub(started))
	})
}

func (m *TrafficMiddleware) record(r *http.Request, body []byte, capture *captureWriter, took time.Duration) {
	entry := traffic.Entry{
		Service:         m.service,
		Direction:       traffic.DirectionIn,
		HTTPStatus:      capture.status,
		RequestHeaders:  flattenHeader(r.Header),
		ResponseHeaders: flattenHeader(capture.Header()),
		RequestBody:     truncate(body),
		ResponseBody:    truncate(capture.body.Bytes()),
		DurationMS:      int(took.Milliseconds()),
		RemoteAddr:      r.RemoteAddr,
	}

	if sandbox, ok := sandboxctx.Get(r.Context()); ok {
		id := sandbox.ID
		entry.SandboxID = &id
	}

	entry.Method, entry.RPCID = describeRPC(body)
	entry.ErrorCode = errorCodeOf(capture.body.Bytes())

	if rule, ok := FiredRule(r.Context()); ok {
		id := rule.ID
		entry.FaultRuleID = &id
	}

	// The request is already answered, so its context may be cancelled; the
	// record is written against a fresh one rather than being lost.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()

	if err := m.recorder.Record(ctx, entry); err != nil && m.onError != nil {
		m.onError(err)
	}
}

// captureWriter records the status and body while passing them through.
type captureWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	// wroteHeader guards against a handler that writes the header twice, which
	// would otherwise record the second status the client never saw.
	wroteHeader bool
}

func (c *captureWriter) WriteHeader(status int) {
	if c.wroteHeader {
		return
	}
	c.wroteHeader = true
	c.status = status
	c.ResponseWriter.WriteHeader(status)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if !c.wroteHeader {
		c.wroteHeader = true
	}
	// A body larger than the cap is still delivered in full; only what is kept
	// for the log is limited.
	if c.body.Len() < maxRecordedBody {
		c.body.Write(b[:min(len(b), maxRecordedBody-c.body.Len())])
	}
	return c.ResponseWriter.Write(b)
}

// Unwrap lets the fault layer reach the real writer to hijack the connection.
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// firedRuleKey carries the slot the fault layer reports into, so the traffic
// log can name the rule without the fault layer writing the record itself.
type firedRuleKey struct{}

// firedRule is the slot. It is filled in by the fault layer while the request
// is still being handled and read once the response is complete.
type firedRule struct {
	rule *fault.Rule
}

// withFiredRule returns a context carrying the slot.
func withFiredRule(ctx context.Context, slot *firedRule) context.Context {
	return context.WithValue(ctx, firedRuleKey{}, slot)
}

// ReportFiredRule records the rule that shaped the response. It does nothing
// when nothing is listening, so the fault layer works unwrapped too.
func ReportFiredRule(ctx context.Context, rule *fault.Rule) {
	if slot, ok := ctx.Value(firedRuleKey{}).(*firedRule); ok {
		slot.rule = rule
	}
}

// FiredRule returns the rule that shaped the response, if one did.
func FiredRule(ctx context.Context) (*fault.Rule, bool) {
	slot, ok := ctx.Value(firedRuleKey{}).(*firedRule)
	if !ok || slot.rule == nil {
		return nil, false
	}
	return slot.rule, true
}

// rpcEnvelope is the part of a JSON-RPC message the log records.
type rpcEnvelope struct {
	Method string          `json:"method"`
	ID     json.RawMessage `json:"id"`
	Error  *struct {
		Code int `json:"code"`
	} `json:"error"`
}

// describeRPC pulls the method and id out of a request body. A body that is
// not JSON-RPC leaves both empty rather than failing the record.
func describeRPC(body []byte) (method, id string) {
	var envelope rpcEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", ""
	}

	return envelope.Method, string(bytes.Trim(envelope.ID, `"`))
}

// errorCodeOf reports the protocol error a response carried. The protocol
// reports errors inside a 200, so the status alone cannot say whether a call
// succeeded.
func errorCodeOf(body []byte) *int {
	var envelope rpcEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Error == nil {
		return nil
	}

	code := envelope.Error.Code

	return &code
}

func truncate(body []byte) []byte {
	if len(body) > maxRecordedBody {
		return body[:maxRecordedBody]
	}
	return body
}

// flattenHeader turns the header map into one the log can store.
//
// A header sent twice is joined with a comma, which is how the HTTP spec says
// repeated values may be written and how they read back.
func flattenHeader(header http.Header) map[string]string {
	if len(header) == 0 {
		return nil
	}

	out := make(map[string]string, len(header))
	for name, values := range header {
		out[name] = strings.Join(values, ", ")
	}

	return out
}
