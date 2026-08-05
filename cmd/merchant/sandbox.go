package main

import (
	"context"
	"errors"
	"net/http"

	sandboxdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/sandbox/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// resolveSandbox puts the stand named in the path onto the request context.
//
// Every repository reads it and adds a sandbox_id condition, so a request that
// arrives without a resolvable stand is rejected here rather than running
// unscoped queries against every stand's data.
func resolveSandbox(repo sandboxLookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slug := r.PathValue("slug")

			sandbox, err := repo.BySlug(r.Context(), slug)
			if err != nil {
				// An unknown slug is a wrong address, not a protocol error, so
				// it is answered at the HTTP level: no JSON-RPC body could say
				// which stand it came from.
				status := http.StatusInternalServerError
				if errors.Is(err, sandboxdomain.ErrNotFound) {
					status = http.StatusNotFound
				}
				http.Error(w, http.StatusText(status), status)
				return
			}

			ctx := sandboxctx.With(r.Context(), sandboxctx.Sandbox{
				ID:         sandbox.ID,
				Slug:       sandbox.Slug,
				MerchantID: sandbox.MerchantID,
				Key:        sandbox.Key,
				TestKey:    sandbox.TestKey,
				ConfigID:   configID(sandbox),
				Kind:       sandbox.Kind,
			})

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// sandboxLookup is the slice of the sandbox repository this service needs.
type sandboxLookup interface {
	BySlug(ctx context.Context, slug string) (*sandboxdomain.Sandbox, error)
}

// configID flattens the optional profile reference. A stand with no profile
// runs on the defaults, which is what zero means downstream.
func configID(s *sandboxdomain.Sandbox) int64 {
	if s.ConfigID == nil {
		return 0
	}
	return *s.ConfigID
}

// resolveKey hands the Merchant API handler the key the incoming request must
// authenticate against, which differs per stand.
func resolveKey(ctx context.Context) (string, error) {
	sandbox, ok := sandboxctx.Get(ctx)
	if !ok {
		return "", sandboxctx.ErrNoSandbox
	}
	return sandbox.Key, nil
}
