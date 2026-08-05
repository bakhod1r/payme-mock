package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	subscribehttp "github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/interfaces/http"
	sandboxdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/sandbox/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
	"github.com/bakhod1r/payme-mock/internal/kernel/jsonrpc"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// resolveSandbox puts the stand named in the path onto the request context.
//
// Every repository reads it and adds a sandbox_id condition, so a request that
// arrives without a resolvable stand is rejected here rather than running
// unscoped queries against every stand's cards and receipts.
func resolveSandbox(repo sandboxLookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sandbox, err := repo.BySlug(r.Context(), r.PathValue("slug"))
			if err != nil {
				// An unknown slug is a wrong address, not a protocol error: no
				// JSON-RPC body could say which stand it came from.
				writeResolveError(w, err)
				return
			}

			next.ServeHTTP(w, r.WithContext(scoped(r.Context(), sandbox)))
		})
	}
}

// resolveSandboxByAuth puts the stand named by the X-Auth credential onto the
// request context.
//
// The real provider serves every cash register from one address and tells them
// apart by the merchant id in X-Auth, so a client configured with a single API
// URL and three registers reaches three stands here the same way. The key is
// not checked here: the Subscribe API handler already authorizes it against the
// resolved stand, and doing it twice would split that rule across two places.
func resolveSandboxByAuth(repo sandboxLookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth, ok := httpx.ParseSubscribeAuth(r.Header.Get("X-Auth"))
			if !ok {
				// This address is the provider's own: every register is served
				// from it and told apart by the credential. A caller that sent
				// no credential, or one naming a register that is not here, has
				// made a protocol mistake and is owed the protocol's answer —
				// -32504 in a JSON-RPC envelope, not an HTTP status page it
				// would have to be written to understand.
				writeUnauthorized(w)
				return
			}

			sandbox, err := repo.ByMerchantID(r.Context(), auth.MerchantID)
			if err != nil {
				if errors.Is(err, sandboxdomain.ErrNotFound) {
					writeUnauthorized(w)
					return
				}
				writeResolveError(w, err)
				return
			}

			next.ServeHTTP(w, r.WithContext(scoped(r.Context(), sandbox)))
		})
	}
}

// scoped returns the context every repository reads the stand from.
func scoped(ctx context.Context, sandbox *sandboxdomain.Sandbox) context.Context {
	return sandboxctx.With(ctx, sandboxctx.Sandbox{
		ID:           sandbox.ID,
		Slug:         sandbox.Slug,
		MerchantID:   sandbox.MerchantID,
		MerchantName: sandbox.MerchantName,
		Key:          sandbox.Key,
		TestKey:      sandbox.TestKey,
		ConfigID:     configID(sandbox),
	})
}

// writeUnauthorized answers a credential this address cannot serve, in the
// envelope the Subscribe API answers everything else in.
//
// The status stays 200: the provider reports its refusals inside the body, and
// a client that reads the code out of the JSON would see nothing at all if this
// one arrived as a bare 401.
func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")

	// The envelope is a fixed shape of strings and numbers, so there is nothing
	// in it that can fail to encode.
	body, _ := json.Marshal(jsonrpc.NewError(nil, payerr.ErrUnauthorized).WithVersion())

	_, _ = w.Write(body)
}

// writeResolveError answers a lookup that found no stand. A stand that cannot
// be resolved is a wrong address rather than a protocol error, so the answer is
// an HTTP status and not a JSON-RPC error body.
func writeResolveError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, sandboxdomain.ErrNotFound) {
		status = http.StatusNotFound
	}
	http.Error(w, http.StatusText(status), status)
}

// sandboxLookup is the slice of the sandbox repository this service needs.
type sandboxLookup interface {
	BySlug(ctx context.Context, slug string) (*sandboxdomain.Sandbox, error)
	ByMerchantID(ctx context.Context, merchantID string) (*sandboxdomain.Sandbox, error)
}

// configID flattens the optional profile reference. A stand with no profile
// runs on the defaults, which is what zero means downstream.
func configID(s *sandboxdomain.Sandbox) int64 {
	if s.ConfigID == nil {
		return 0
	}
	return *s.ConfigID
}

// resolveCredentials hands the Subscribe API handler the X-Auth credential the
// incoming call must match, which differs per stand.
func resolveCredentials(ctx context.Context) (subscribehttp.Credentials, error) {
	sandbox, ok := sandboxctx.Get(ctx)
	if !ok {
		return subscribehttp.Credentials{}, sandboxctx.ErrNoSandbox
	}

	return subscribehttp.Credentials{
		MerchantID: sandbox.MerchantID,
		Key:        sandbox.TestKey,
	}, nil
}
