package jsonrpc

import (
	"context"
	"encoding/json"

	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// Handler executes one RPC method. Params arrive as raw JSON so each handler
// decodes into its own parameter struct.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Router dispatches RPC calls to registered handlers.
type Router struct {
	handlers map[string]Handler
}

// NewRouter returns an empty router.
func NewRouter() *Router {
	return &Router{handlers: make(map[string]Handler)}
}

// Register binds a handler to a method name, replacing any previous binding.
func (r *Router) Register(method string, h Handler) {
	r.handlers[method] = h
}

// Methods returns the registered method names, unordered.
func (r *Router) Methods() []string {
	names := make([]string, 0, len(r.handlers))
	for name := range r.handlers {
		names = append(names, name)
	}
	return names
}

// Dispatch parses a raw request body and executes the matching handler. It
// never returns an error: every failure becomes a Response carrying a protocol
// error, because the merchant server must always answer HTTP 200 with a
// well-formed JSON-RPC body.
func (r *Router) Dispatch(ctx context.Context, body []byte) Response {
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return NewError(nil, payerr.ErrParse)
	}

	if req.Method == "" {
		return NewError(req.ID, payerr.ErrInvalidRequest)
	}

	handler, ok := r.handlers[req.Method]
	if !ok {
		return NewError(req.ID, payerr.ErrMethodNotFound)
	}

	result, err := handler(ctx, req.Params)
	if err != nil {
		return ErrorFrom(req.ID, err)
	}

	return NewResult(req.ID, result)
}

// DecodeParams unmarshals named parameters, translating a malformed payload
// into the documented "required field missing" error rather than a raw JSON
// error, which the protocol has no way to express.
func DecodeParams(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return payerr.ErrInvalidRequest
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return payerr.ErrInvalidRequest
	}
	return nil
}
