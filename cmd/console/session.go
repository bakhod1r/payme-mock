package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"sync"
	"time"
)

// sessionCookie is the cookie name the console issues on a successful login.
const sessionCookie = "payme_console"

// sessionTTL is how long a login stays valid.
const sessionTTL = 12 * time.Hour

// sessions holds the live logins. The console is a single process serving one
// operator at a time, so keeping them in memory is enough; a restart simply
// asks everyone to sign in again.
type sessions struct {
	mu    sync.Mutex
	items map[string]session
}

type session struct {
	user      string
	expiresAt time.Time
}

func newSessions() *sessions {
	return &sessions{items: make(map[string]session)}
}

// issue creates a session and returns its token.
func (s *sessions) issue(user string, now time.Time) string {
	raw := make([]byte, 32)
	// rand.Read from crypto/rand cannot fail; it panics internally instead.
	_, _ = rand.Read(raw)
	token := hex.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Expired entries are dropped on the way past, so an unattended console
	// does not accumulate them.
	for t, item := range s.items {
		if now.After(item.expiresAt) {
			delete(s.items, t)
		}
	}
	s.items[token] = session{user: user, expiresAt: now.Add(sessionTTL)}

	return token
}

// lookup returns the user behind a token, if the token is live.
func (s *sessions) lookup(token string, now time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	item, ok := s.items[token]
	if !ok || now.After(item.expiresAt) {
		return "", false
	}

	return item.user, true
}

// revoke ends a session.
func (s *sessions) revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, token)
}

// checkCredentials compares a submitted login against the configured one.
//
// Both fields are compared in constant time and neither comparison is skipped
// on an early mismatch, so a wrong username cannot be told from a wrong
// password by timing the response.
func (c config) checkCredentials(user, password string) bool {
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(c.Username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(password), []byte(c.Password)) == 1
	return userOK && passOK
}

// loopback reports whether a request came from this machine.
//
// The console is a local stand, so a request from the loopback interface is the
// operator sitting at the keyboard and is let through without a login. Anything
// arriving over the network still has to sign in — the screens show every
// sandbox's Payme keys, and that is not something to leave open on a shared
// address.
func loopback(remoteAddr string) bool {
	return peerIsLocal(remoteAddr, false)
}

// peerIsLocal reports a caller the console may let through without a login.
//
// Behind a container the peer is the bridge gateway rather than 127.0.0.1, so
// trustPrivate widens the test to the private ranges. That is only sound where
// the port itself is bound to loopback: the check then says "the request came
// through the address only this machine can reach", which is what the login
// was skipping for in the first place.
func peerIsLocal(remoteAddr string, trustPrivate bool) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// An address with no port is not something the server produced; it is
		// safer to treat it as remote than to guess.
		return false
	}

	ip := net.ParseIP(host)

	if ip == nil {
		return false
	}

	return ip.IsLoopback() || (trustPrivate && ip.IsPrivate())
}

// setSessionCookie writes the session cookie.
//
// HttpOnly keeps the token away from scripts and SameSite=Lax means another
// site cannot drive the console through the operator's browser. Secure is left
// off because the stand is normally reached over plain HTTP on localhost.
func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// clearSessionCookie expires the cookie in the browser.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
