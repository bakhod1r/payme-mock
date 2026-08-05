// Package http exposes the Subscribe API over JSON-RPC at POST /api.
package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/application"
	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
	"github.com/bakhod1r/payme-mock/internal/kernel/jsonrpc"

	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// maxBodyBytes caps a request body, as on the merchant side.
const maxBodyBytes = 1 << 20 // 1 MiB

// Credentials are the merchant id and key an incoming call must match.
type Credentials struct {
	MerchantID string
	Key        string
}

// CredentialResolver returns the credentials in force for a request, so each
// sandbox can carry its own.
type CredentialResolver func(ctx context.Context) (Credentials, error)

// browserMethods are the methods a browser may call with only the merchant id.
// Everything else requires the key, because it moves money or reads history.
//
// The list follows the documented examples: every card-entry method shows
// "X-Auth: {id}", including cards.check, which only reports what the browser
// already holds. cards.remove is the exception — it shows "X-Auth: {id}:{key}"
// and destroys a token, so it is server-side like the receipt methods.
var browserMethods = map[string]bool{
	"cards.create":          true,
	"cards.get_verify_code": true,
	"cards.verify":          true,
	"cards.check":           true,
}

// Handler serves the Subscribe API.
type Handler struct {
	router  *jsonrpc.Router
	resolve CredentialResolver
}

// NewHandler registers every method against the service.
func NewHandler(svc *application.Service, resolve CredentialResolver) *Handler {
	r := jsonrpc.NewRouter()

	register(r, "cards.create", svc.CardsCreate)
	register(r, "cards.get_verify_code", svc.CardsGetVerifyCode)
	register(r, "cards.verify", svc.CardsVerify)
	register(r, "cards.check", svc.CardsCheck)
	register(r, "cards.remove", svc.CardsRemove)

	register(r, "receipts.create", svc.ReceiptsCreate)
	register(r, "receipts.pay", svc.ReceiptsPay)
	register(r, "receipts.send", svc.ReceiptsSend)
	register(r, "receipts.cancel", svc.ReceiptsCancel)
	register(r, "receipts.check", svc.ReceiptsCheck)
	register(r, "receipts.get", svc.ReceiptsGet)
	register(r, "receipts.get_all", svc.ReceiptsGetAll)
	register(r, "receipts.confirm_hold", svc.ReceiptsConfirmHold)

	// Payouts move money out of the register, so they are server-side only:
	// browserMethods deliberately does not list them.
	// transactions.* are not part of the published Subscribe API. They are the
	// payout half of the stand — money handed to a saved card rather than
	// taken from one — and they are shaped like the documented methods so an
	// integration written against a payout contract can be rehearsed here.
	// Nothing in the provider's documentation describes them; anyone comparing
	// the two will not find these, and that is expected.
	register(r, "transactions.create", svc.TransactionsCreate)
	register(r, "transactions.complete", svc.TransactionsComplete)

	// accounts.getBalance is undocumented in the same way and for the same
	// reason: a register's own money is not something the published API reports,
	// and a payout integration that watches for an empty register calls this.
	// It reads the register's money, so it is server-side.
	register(r, "accounts.getBalance", svc.AccountsGetBalance)

	return &Handler{router: r, resolve: resolve}
}

// register adapts a typed use case into an RPC handler.
func register[P, R any](r *jsonrpc.Router, method string, fn func(context.Context, P) (R, error)) {
	r.Register(method, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var params P
		if err := jsonrpc.DecodeParams(raw, &params); err != nil {
			return nil, err
		}
		return fn(ctx, params)
	})
}

// ServeHTTP authenticates the X-Auth header and dispatches the call.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeResponse(w, jsonrpc.NewError(nil, payerr.ErrInvalidHTTPMethod).WithVersion())
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeResponse(w, jsonrpc.NewError(nil, payerr.ErrTransport).WithVersion())
		return
	}

	if resp, ok := h.authorize(r, body); !ok {
		writeResponse(w, resp.WithVersion())
		return
	}

	writeResponse(w, h.router.Dispatch(r.Context(), body).WithVersion())
}

// authorize checks the credential and that the caller may reach the method it
// asked for. A browser-side credential cannot invoke a server-side method.
func (h *Handler) authorize(r *http.Request, body []byte) (jsonrpc.Response, bool) {
	// Every refusal names the request it answers, as the documented envelope
	// does; a caller matching responses by id would otherwise lose them.
	id := jsonrpc.IDOf(body)

	auth, ok := httpx.ParseSubscribeAuth(r.Header.Get("X-Auth"))
	if !ok {
		return jsonrpc.NewError(id, payerr.ErrUnauthorized), false
	}

	creds, err := h.resolve(r.Context())
	if err != nil {
		return jsonrpc.NewError(id, payerr.ErrUnauthorized), false
	}

	if !auth.Authorize(creds.MerchantID, creds.Key) {
		return jsonrpc.NewError(id, payerr.ErrUnauthorized), false
	}

	if !auth.Backend && !browserMethods[methodOf(body)] {
		return jsonrpc.NewError(id, payerr.ErrUnauthorized), false
	}

	return jsonrpc.Response{}, true
}

// methodOf peeks at the method name so authorization can be decided before
// dispatch. An unreadable body yields an empty name, which no browser-side
// method matches, so the call is refused rather than let through.
func methodOf(body []byte) string {
	var probe struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Method
}

func writeResponse(w http.ResponseWriter, resp jsonrpc.Response) {
	// The provider answers with text/json, not application/json.
	w.Header().Set("Content-Type", "text/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
