package httpx_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	access "github.com/bakhod1r/payme-mock/internal/context/simulation/access/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

type stubRules struct {
	list access.Allowlist
	err  error
}

func (s stubRules) BySandbox(context.Context, int64) (access.Allowlist, error) {
	return s.list, s.err
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// serveGuarded runs one request through the guard and reports the status and
// whether the handler behind it ran.
func serveGuarded(rules httpx.AllowlistLookup, trust bool, decorate func(*http.Request)) (int, bool) {
	var reached bool

	guarded := httpx.Allowlist(rules, trust, quietLog())(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		}))

	req := httptest.NewRequest(http.MethodPost, "/s/acme/api", nil)
	req.RemoteAddr = "203.0.113.7:5000"
	req = req.WithContext(sandboxctx.With(req.Context(), sandboxctx.Sandbox{ID: 1, Slug: "acme"}))
	if decorate != nil {
		decorate(req)
	}

	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)

	return rec.Code, reached
}

func rule(t *testing.T, raw string) access.Rule {
	t.Helper()

	prefix, err := access.ParsePrefix(raw)
	assert.NoError(t, err)
	return access.Rule{Prefix: prefix}
}

// A stand with no rules answers everyone, which is what every stand does until
// someone writes the first rule.
func TestAllowlistPassesWhenNoRules(t *testing.T) {
	status, reached := serveGuarded(stubRules{}, true, nil)

	assert.Equal(t, http.StatusOK, status)
	assert.True(t, reached)
}

func TestAllowlistAdmitsAListedAddress(t *testing.T) {
	rules := stubRules{list: access.Allowlist{rule(t, "203.0.113.7")}}

	status, reached := serveGuarded(rules, true, nil)

	assert.Equal(t, http.StatusOK, status)
	assert.True(t, reached)
}

// The refusal lands before the handler, so a call from the wrong address never
// reaches the point where a credential could be judged.
func TestAllowlistRefusesAnUnlistedAddress(t *testing.T) {
	rules := stubRules{list: access.Allowlist{rule(t, "10.0.0.0/8")}}

	status, reached := serveGuarded(rules, true, nil)

	assert.Equal(t, http.StatusForbidden, status)
	assert.False(t, reached)
}

func TestAllowlistReadsTheForwardedAddress(t *testing.T) {
	rules := stubRules{list: access.Allowlist{rule(t, "198.51.100.4")}}

	forwarded := func(r *http.Request) {
		r.RemoteAddr = "172.18.0.1:5000"
		r.Header.Set("X-Forwarded-For", "198.51.100.4")
	}

	status, reached := serveGuarded(rules, true, forwarded)
	assert.Equal(t, http.StatusOK, status)
	assert.True(t, reached)

	// Untrusted, the header is somebody's claim rather than a fact, so the
	// proxy's own address is judged instead and it is not on the list.
	status, reached = serveGuarded(rules, false, forwarded)
	assert.Equal(t, http.StatusForbidden, status)
	assert.False(t, reached)
}

// A guard wired above the resolver has no stand to check against; that is a
// mistake in the wiring, so it is reported as one rather than as a refusal.
func TestAllowlistWithoutASandbox(t *testing.T) {
	guarded := httpx.Allowlist(stubRules{}, true, quietLog())(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") }))

	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api", nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// A list that cannot be read is not an empty list: failing open would drop the
// restriction exactly when the database is in trouble.
func TestAllowlistFailsClosedOnAReadError(t *testing.T) {
	status, reached := serveGuarded(stubRules{err: errors.New("storage is down")}, true, nil)

	assert.Equal(t, http.StatusInternalServerError, status)
	assert.False(t, reached)
}
