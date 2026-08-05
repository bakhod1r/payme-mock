package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionIssueAndLookup(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	store := newSessions()

	token := store.issue("admin", now)
	require.NotEmpty(t, token)

	user, ok := store.lookup(token, now.Add(time.Hour))
	assert.True(t, ok)
	assert.Equal(t, "admin", user)
}

func TestSessionLookupRejectsAnUnknownToken(t *testing.T) {
	_, ok := newSessions().lookup("nonsense", time.Now())
	assert.False(t, ok)
}

// A cookie that outlived its session is not a login, however well-formed.
func TestSessionLookupRejectsAnExpiredToken(t *testing.T) {
	now := time.Now()
	store := newSessions()

	token := store.issue("admin", now)

	_, ok := store.lookup(token, now.Add(sessionTTL+time.Second))
	assert.False(t, ok)
}

func TestSessionRevokeEndsIt(t *testing.T) {
	now := time.Now()
	store := newSessions()
	token := store.issue("admin", now)

	store.revoke(token)

	_, ok := store.lookup(token, now)
	assert.False(t, ok)
}

// An unattended console must not accumulate dead sessions, so each new login
// sweeps the expired ones out.
func TestIssueDropsExpiredSessions(t *testing.T) {
	now := time.Now()
	store := newSessions()

	stale := store.issue("admin", now)
	store.issue("admin", now.Add(sessionTTL+time.Minute))

	store.mu.Lock()
	_, present := store.items[stale]
	count := len(store.items)
	store.mu.Unlock()

	assert.False(t, present)
	assert.Equal(t, 1, count)
}

func TestIssueGivesEverySessionItsOwnToken(t *testing.T) {
	now := time.Now()
	store := newSessions()

	assert.NotEqual(t, store.issue("admin", now), store.issue("admin", now))
}

func TestCheckCredentials(t *testing.T) {
	cfg := config{Username: "admin", Password: "s3cret"}

	assert.True(t, cfg.checkCredentials("admin", "s3cret"))
	assert.False(t, cfg.checkCredentials("admin", "wrong"))
	assert.False(t, cfg.checkCredentials("root", "s3cret"))
	assert.False(t, cfg.checkCredentials("", ""))
}

// The console skips the login for the operator at the keyboard and for nobody
// else: the screens carry every sandbox's Payme keys.
func TestLoopback(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:54321", true},
		{"[::1]:54321", true},
		{"127.10.20.30:80", true},
		{"192.168.1.10:54321", false},
		{"8.8.8.8:443", false},
		{"127.0.0.1", false},    // no port: not something the server produced
		{"", false},             // nor is an empty address
		{"host.name:80", false}, // a name is not an address
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			assert.Equal(t, tt.want, loopback(tt.addr))
		})
	}
}

func TestSetSessionCookie(t *testing.T) {
	w := httptest.NewRecorder()

	setSessionCookie(w, "token-value")

	cookie := w.Result().Cookies()[0]
	assert.Equal(t, sessionCookie, cookie.Name)
	assert.Equal(t, "token-value", cookie.Value)
	assert.True(t, cookie.HttpOnly, "the token is never read by a script")
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
	assert.Positive(t, cookie.MaxAge)
}

func TestClearSessionCookieExpiresIt(t *testing.T) {
	w := httptest.NewRecorder()

	clearSessionCookie(w)

	cookie := w.Result().Cookies()[0]
	assert.Equal(t, sessionCookie, cookie.Name)
	assert.Empty(t, cookie.Value)
	assert.Negative(t, cookie.MaxAge)
}

// The identifiers are handed out in the shapes the real provider uses, since a
// merchant pastes them straight into their settings.
func TestCredentialShapes(t *testing.T) {
	var gen credentials

	assert.Len(t, gen.MerchantID(), 24)
	assert.Len(t, gen.Key(), 32)
	assert.NotEqual(t, gen.Key(), gen.Key())
	assert.NotEqual(t, gen.MerchantID(), gen.MerchantID())
}

// Behind a container the peer is the bridge gateway rather than 127.0.0.1, so
// the console can be told to trust the private ranges — but only when it is
// asked to, and never for an address off the private network.
func TestPeerIsLocal(t *testing.T) {
	tests := []struct {
		name         string
		addr         string
		trustPrivate bool
		want         bool
	}{
		{"loopback is always local", "127.0.0.1:54321", false, true},
		{"IPv6 loopback is always local", "[::1]:54321", false, true},
		{"the docker bridge is not local on its own", "172.18.0.1:54321", false, false},
		{"the docker bridge is local when trusted", "172.18.0.1:54321", true, true},
		{"a LAN address is local when trusted", "192.168.1.20:54321", true, true},
		{"a public address is never local", "203.0.113.7:54321", true, false},
		{"a malformed address is never local", "not-an-address", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, peerIsLocal(tt.addr, tt.trustPrivate))
		})
	}
}
