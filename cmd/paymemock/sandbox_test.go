package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sandboxdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/sandbox/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// stubLookup answers from a fixed set of stands, so resolution can be tested
// without a database.
type stubLookup struct {
	bySlug     map[string]*sandboxdomain.Sandbox
	byMerchant map[string]*sandboxdomain.Sandbox
}

func (s stubLookup) BySlug(_ context.Context, slug string) (*sandboxdomain.Sandbox, error) {
	if found, ok := s.bySlug[slug]; ok {
		return found, nil
	}
	return nil, sandboxdomain.ErrNotFound
}

func (s stubLookup) ByMerchantID(_ context.Context, merchantID string) (*sandboxdomain.Sandbox, error) {
	if found, ok := s.byMerchant[merchantID]; ok {
		return found, nil
	}
	return nil, sandboxdomain.ErrNotFound
}

// recordScope captures the stand the middleware resolved.
func recordScope(into *sandboxctx.Sandbox) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sandbox, ok := sandboxctx.Get(r.Context()); ok {
			*into = sandbox
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestResolveSandboxByAuth(t *testing.T) {
	topup := &sandboxdomain.Sandbox{ID: 4, Slug: "cashbox-topup", MerchantID: "aaa", TestKey: "topup-key"}
	dividend := &sandboxdomain.Sandbox{ID: 5, Slug: "cashbox-dividend", MerchantID: "bbb", TestKey: "dividend-key"}

	repo := stubLookup{byMerchant: map[string]*sandboxdomain.Sandbox{
		topup.MerchantID:    topup,
		dividend.MerchantID: dividend,
	}}

	// One API URL and several registers is how the real provider is
	// configured, so each credential must reach its own stand.
	t.Run("each credential reaches its own stand", func(t *testing.T) {
		for _, want := range []*sandboxdomain.Sandbox{topup, dividend} {
			var got sandboxctx.Sandbox

			req := httptest.NewRequest(http.MethodPost, "/api", nil)
			req.Header.Set("X-Auth", want.MerchantID+":"+want.TestKey)
			rec := httptest.NewRecorder()

			resolveSandboxByAuth(repo)(recordScope(&got)).ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			if got.ID != want.ID {
				t.Errorf("sandbox = %d, want %d", got.ID, want.ID)
			}
			if got.TestKey != want.TestKey {
				t.Errorf("test key = %q, want %q", got.TestKey, want.TestKey)
			}
		}
	})

	// The key is the handler's business; resolution only needs the id. A wrong
	// key must still resolve, or the handler could never answer with the
	// protocol's own authorization error.
	t.Run("a wrong key still resolves", func(t *testing.T) {
		var got sandboxctx.Sandbox

		req := httptest.NewRequest(http.MethodPost, "/api", nil)
		req.Header.Set("X-Auth", topup.MerchantID+":wrong")
		rec := httptest.NewRecorder()

		resolveSandboxByAuth(repo)(recordScope(&got)).ServeHTTP(rec, req)

		if got.ID != topup.ID {
			t.Fatalf("sandbox = %d, want %d", got.ID, topup.ID)
		}
	})

	// This address serves every register and tells them apart by the credential,
	// so a credential it cannot serve is a protocol refusal — the same -32504
	// the handler answers a wrong key with, in the same envelope. A bare HTTP
	// status would reach a client that only reads JSON-RPC as no answer at all.
	t.Run("an unknown merchant is refused in the protocol", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api", nil)
		req.Header.Set("X-Auth", "nobody:key")
		rec := httptest.NewRecorder()

		resolveSandboxByAuth(repo)(recordScope(new(sandboxctx.Sandbox))).ServeHTTP(rec, req)

		assertUnauthorizedEnvelope(t, rec)
	})

	t.Run("a missing credential is refused in the protocol", func(t *testing.T) {
		for _, header := range []string{"", ":key"} {
			req := httptest.NewRequest(http.MethodPost, "/api", nil)
			if header != "" {
				req.Header.Set("X-Auth", header)
			}
			rec := httptest.NewRecorder()

			resolveSandboxByAuth(repo)(recordScope(new(sandboxctx.Sandbox))).ServeHTTP(rec, req)

			assertUnauthorizedEnvelope(t, rec)
		}
	})
}

// assertUnauthorizedEnvelope checks the refusal a caller actually parses: a 200
// carrying the documented -32504 body.
func assertUnauthorizedEnvelope(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if !strings.Contains(rec.Body.String(), "-32504") {
		t.Fatalf("body = %q, want the unauthorized code", rec.Body.String())
	}
}

// noGuard is the address check turned off, which is what a stand with no
// rules already is; the guard has its own tests.
func noGuard(next http.Handler) http.Handler { return next }

func TestResolveSandboxBySlug(t *testing.T) {
	stand := &sandboxdomain.Sandbox{ID: 4, Slug: "cashbox-topup", MerchantID: "aaa"}
	repo := stubLookup{bySlug: map[string]*sandboxdomain.Sandbox{stand.Slug: stand}}

	t.Run("a known slug scopes the request", func(t *testing.T) {
		var got sandboxctx.Sandbox

		rec := httptest.NewRecorder()
		routes(repo, noGuard, recordScope(&got)).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/s/cashbox-topup/api", nil))

		if got.ID != stand.ID {
			t.Fatalf("sandbox = %d, want %d", got.ID, stand.ID)
		}
	})

	t.Run("an unknown slug is not found", func(t *testing.T) {
		rec := httptest.NewRecorder()
		routes(repo, noGuard, recordScope(new(sandboxctx.Sandbox))).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/s/missing/api", nil))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
	})
}
