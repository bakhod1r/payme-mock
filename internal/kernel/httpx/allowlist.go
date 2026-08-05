package httpx

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/bakhod1r/payme-mock/internal/context/simulation/access/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// AllowlistLookup reads a stand's address rules.
type AllowlistLookup interface {
	BySandbox(ctx context.Context, sandboxID int64) (domain.Allowlist, error)
}

// Allowlist refuses a request from an address the stand does not list.
//
// It runs after the stand is resolved and before anything else: the real
// provider drops unregistered traffic at the edge, so a call from the wrong
// address must not reach the point where a credential could be judged, or the
// stand would be answering "wrong key" to a request it never accepted.
//
// The answer is an HTTP status rather than a protocol error, for the same
// reason an unknown stand is: the request was never admitted, so there is
// nothing to answer it as.
func Allowlist(rules AllowlistLookup, trustForwarded bool, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sandbox, ok := sandboxctx.Get(r.Context())
			if !ok {
				// Without a stand there is no list to check against. Reaching
				// here means the middleware is wired above the resolver, which
				// is a mistake in the wiring rather than in the request.
				http.Error(w, http.StatusText(http.StatusInternalServerError),
					http.StatusInternalServerError)
				return
			}

			allowlist, err := rules.BySandbox(r.Context(), sandbox.ID)
			if err != nil {
				log.Error("read ip rules", "sandbox", sandbox.Slug, "error", err)
				http.Error(w, http.StatusText(http.StatusInternalServerError),
					http.StatusInternalServerError)
				return
			}

			if len(allowlist) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			addr, ok := domain.ClientAddr(r.RemoteAddr, r.Header.Get("X-Forwarded-For"), trustForwarded)
			if !ok || !allowlist.Allows(addr) {
				// The address is logged because a refused call is exactly what
				// someone is about to ask about, and the answer says nothing.
				log.Warn("refused by ip allowlist", "sandbox", sandbox.Slug,
					"remote", r.RemoteAddr, "forwarded", r.Header.Get("X-Forwarded-For"))
				http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
