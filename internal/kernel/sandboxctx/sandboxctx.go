// Package sandboxctx carries the active sandbox through a request. Repositories
// read it and add a sandbox_id condition to every query, so isolation cannot be
// forgotten in a bounded context.
package sandboxctx

import (
	"context"
	"errors"
)

// ErrNoSandbox reports a request that reached a repository without a sandbox.
// It is a wiring mistake rather than a user error: no query may run unscoped.
var ErrNoSandbox = errors.New("no sandbox in context")

type contextKey struct{}

// Sandbox is what a request carries about the stand it belongs to.
type Sandbox struct {
	ID   int64
	Slug string
	// MerchantID is the cash register identifier callers authenticate with.
	MerchantID string
	// MerchantName is the organization the payer sees on a receipt.
	MerchantName string
	Key          string
	TestKey      string
	// ConfigID names the configuration profile in force.
	ConfigID int64
	// Kind is the direction the register moves money in: money coming in on a
	// topup register, money paid out on a dividend or deposit one.
	Kind string
}

// With returns a context carrying the sandbox.
func With(ctx context.Context, s Sandbox) context.Context {
	return context.WithValue(ctx, contextKey{}, s)
}

// Get returns the sandbox in the context, reporting whether one is present.
func Get(ctx context.Context) (Sandbox, bool) {
	s, ok := ctx.Value(contextKey{}).(Sandbox)
	return s, ok
}

// From returns the sandbox identifier, or ErrNoSandbox when the request was
// never scoped. Repositories call this so an unscoped query fails loudly
// instead of quietly reading every stand's data.
func From(ctx context.Context) (int64, error) {
	s, ok := Get(ctx)
	if !ok {
		return 0, ErrNoSandbox
	}
	return s.ID, nil
}
