// Package domain models the traffic log: one record per request the stand
// handled, which is what the console's traffic screen reads back.
package domain

import "context"

// Direction says which way a call went.
type Direction string

// The two directions. Incoming is a request the stand answered; outgoing is
// one it made, such as a webhook to the merchant.
const (
	DirectionIn  Direction = "in"
	DirectionOut Direction = "out"
)

// Entry is one recorded call.
type Entry struct {
	ID int64
	// SandboxID is nil for a request that never resolved to a stand, such as
	// one addressed to a slug that does not exist.
	SandboxID *int64
	Service   string
	Direction Direction
	// Method is the JSON-RPC method, empty when the body carried none.
	Method string
	// RPCID is the request's JSON-RPC id, kept as text because the protocol
	// allows a number or a string.
	RPCID      string
	HTTPStatus int
	// RequestHeaders and ResponseHeaders are the headers as they crossed the
	// wire. Half of what goes wrong between two services is in them — a wrong
	// key, a missing content type, a proxy that rewrote the address — and a
	// log that kept only the bodies could not show it.
	//
	// A credential is recorded as it arrived: this is a stand, the keys are
	// its own, and a header logged with its value blanked out cannot answer
	// the question it was kept for.
	RequestHeaders  map[string]string
	ResponseHeaders map[string]string
	RequestBody     []byte
	ResponseBody    []byte
	DurationMS      int
	// FaultRuleID names the rule that shaped the response, if one fired.
	FaultRuleID *int64
	// ErrorCode is the protocol error the response carried, if any. It is
	// recorded separately because the protocol reports errors inside a 200.
	ErrorCode  *int
	RemoteAddr string
}

// Recorder stores traffic entries.
//
// Recording must never fail a request that otherwise succeeded, so callers
// log the error and carry on rather than propagating it.
type Recorder interface {
	Record(ctx context.Context, e Entry) error
}
