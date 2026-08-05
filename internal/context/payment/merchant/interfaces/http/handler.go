// Package http exposes the Merchant API over JSON-RPC. It is a thin layer: it
// authenticates, decodes, delegates to the application service and encodes.
package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/bakhod1r/payme-mock/internal/context/payment/merchant/application"
	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
	"github.com/bakhod1r/payme-mock/internal/kernel/jsonrpc"

	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// maxBodyBytes caps a request body. Payme's own requests are small; anything
// larger is a client defect or an attack, not traffic worth buffering.
const maxBodyBytes = 1 << 20 // 1 MiB

// KeyResolver returns the key the incoming request must authenticate against.
// It is a function so each sandbox can carry its own key.
type KeyResolver func(ctx context.Context) (string, error)

// Handler serves POST /payme/merchant.
type Handler struct {
	router      *jsonrpc.Router
	resolveKey  KeyResolver
	maxBodySize int64
}

// NewHandler registers the six protocol methods against the service.
func NewHandler(svc *application.Service, resolveKey KeyResolver) *Handler {
	r := jsonrpc.NewRouter()

	register(r, "CheckPerformTransaction", svc.CheckPerformTransaction)
	register(r, "CreateTransaction", svc.CreateTransaction)
	register(r, "PerformTransaction", svc.PerformTransaction)
	register(r, "CancelTransaction", svc.CancelTransaction)
	register(r, "CheckTransaction", svc.CheckTransaction)
	register(r, "GetStatement", svc.GetStatement)

	return &Handler{router: r, resolveKey: resolveKey, maxBodySize: maxBodyBytes}
}

// register adapts a typed use case into an RPC handler, decoding its params.
func register[P, R any](r *jsonrpc.Router, method string, fn func(context.Context, P) (R, error)) {
	r.Register(method, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var params P
		if err := jsonrpc.DecodeParams(raw, &params); err != nil {
			return nil, err
		}
		return fn(ctx, params)
	})
}

// ServeHTTP answers every request with HTTP 200 and a JSON-RPC body. The
// protocol treats any other status as a transport error, so failures are
// reported inside the body instead.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeResponse(w, jsonrpc.NewError(nil, payerr.ErrInvalidHTTPMethod))
		return
	}

	key, err := h.resolveKey(r.Context())
	if err != nil {
		writeResponse(w, jsonrpc.NewError(nil, payerr.ErrUnauthorized))
		return
	}

	if !httpx.CheckMerchantAuth(r.Header.Get("Authorization"), key) {
		writeResponse(w, jsonrpc.NewError(nil, payerr.ErrUnauthorized))
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, h.maxBodySize))
	if err != nil {
		writeResponse(w, jsonrpc.NewError(nil, payerr.ErrTransport))
		return
	}

	writeResponse(w, h.router.Dispatch(r.Context(), body))
}

func writeResponse(w http.ResponseWriter, resp jsonrpc.Response) {
	// The provider answers with text/json, not application/json.
	w.Header().Set("Content-Type", "text/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	// The body is built from types that always marshal, and the status is
	// already sent, so a write failure has nowhere left to be reported.
	_ = json.NewEncoder(w).Encode(resp)
}
