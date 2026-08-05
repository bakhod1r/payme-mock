// Package jsonrpc implements the JSON-RPC 2.0 wire format exactly as the Payme
// documentation specifies it: named parameters only, a `result` or `error`
// object, and a three-language message inside every error.
package jsonrpc

import (
	"encoding/json"

	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// Request is an incoming RPC call. The documentation shows `id` as an integer,
// but accepts it as an opaque value, so it is preserved verbatim and echoed back.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	ID     json.RawMessage `json:"id,omitempty"`
}

// Version is the protocol version the Subscribe API names in every response.
const Version = "2.0"

// Response is an outgoing RPC reply. Exactly one of Result and Error is set.
//
// The two protocols answer in different envelopes, and the mock copies each as
// documented rather than picking one: the Merchant API's documented replies
// carry only result-or-error and id, while every Subscribe API reply names the
// protocol version and echoes id even when the request carried none.
type Response struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *ErrorObject    `json:"error,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
}

// WithVersion returns the response in the Subscribe API's envelope: the
// version named, and id present as null rather than dropped when the caller
// sent none, which is what the documented examples show.
func (r Response) WithVersion() Response {
	r.JSONRPC = Version
	if len(r.ID) == 0 {
		r.ID = json.RawMessage("null")
	}
	return r
}

// ErrorObject is the `error` member of a response.
type ErrorObject struct {
	Code    payerr.Code    `json:"code"`
	Message payerr.Message `json:"message"`
	Data    string         `json:"data,omitempty"`
}

// IDOf reads the id a request carried, so an error raised before dispatch can
// still echo it. The documented error envelope always names the request it
// answers, and a caller matching responses by id would lose one that did not.
// An unreadable body yields no id, which is all a response can honestly say.
func IDOf(body []byte) json.RawMessage {
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil
	}
	return probe.ID
}

// NewResult builds a success response echoing the request id.
func NewResult(id json.RawMessage, result any) Response {
	return Response{Result: result, ID: id}
}

// NewError builds an error response echoing the request id.
func NewError(id json.RawMessage, err *payerr.ProtocolError) Response {
	return Response{
		Error: &ErrorObject{Code: err.Code, Message: err.Message, Data: err.Data},
		ID:    id,
	}
}

// ErrorFrom converts any error into a response. Protocol errors keep their
// code; anything else becomes a system error, since leaking internal failure
// text would diverge from what the real provider returns.
func ErrorFrom(id json.RawMessage, err error) Response {
	if pe, ok := payerr.As(err); ok {
		return NewError(id, pe)
	}
	return NewError(id, payerr.ErrTransport)
}
