package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The console holds every stand's keys, so who reaches it is the first thing it
// decides. The operator at this machine's keyboard is not asked to sign in;
// anyone else is, and stays signed in on a session rather than on the password.

// remote is a request from somewhere other than this machine, which is what the
// sign-in rules exist for.
func (s *stand) remote(t *testing.T, method, path string, values url.Values,
	cookies ...*http.Cookie,
) *httptest.ResponseRecorder {
	t.Helper()

	var body io.Reader
	if values != nil {
		body = strings.NewReader(values.Encode())
	}

	r := httptest.NewRequest(method, path, body)
	if values != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.RemoteAddr = "203.0.113.9:41000"

	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}

	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)

	return w
}

// sessionOf reads the cookie a successful sign-in set.
func sessionOf(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()

	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == sessionCookie && cookie.Value != "" {
			return cookie
		}
	}

	t.Fatalf("no session cookie was set: %v", w.Result().Cookies())
	return nil
}

func TestE2ESignInLetsAnOperatorThroughAndKeepsThemThrough(t *testing.T) {
	s := newStand(t)

	form := s.remote(t, http.MethodGet, "/login", nil)
	require.Equal(t, http.StatusOK, form.Code)
	assert.Contains(t, form.Body.String(), "Sign in")

	signedIn := s.remote(t, http.MethodPost, "/login", url.Values{
		"username": {"admin"}, "password": {"s3cret"},
	})
	require.Equal(t, http.StatusSeeOther, signedIn.Code)
	assert.Equal(t, "/", signedIn.Header().Get("Location"))

	session := sessionOf(t, signedIn)

	page := s.remote(t, http.MethodGet, "/sandboxes", nil, session)
	assert.Equal(t, http.StatusOK, page.Code, "the session is what carries them from screen to screen")

	// The form is for people who are not through yet.
	back := s.remote(t, http.MethodGet, "/login", nil, session)
	assert.Equal(t, http.StatusSeeOther, back.Code)

	out := s.remote(t, http.MethodPost, "/logout", nil, session)
	require.Equal(t, http.StatusSeeOther, out.Code)
	assert.Equal(t, "/login", out.Header().Get("Location"))

	after := s.remote(t, http.MethodGet, "/sandboxes", nil, session)
	assert.Equal(t, http.StatusSeeOther, after.Code, "a revoked session is no session")
	assert.Equal(t, "/login", after.Header().Get("Location"))
}

// The refusal names neither field: telling an attacker which half was right
// would hand them a working username for free.
func TestE2ESignInRefusesTheWrongCredentials(t *testing.T) {
	s := newStand(t)

	for _, credentials := range []url.Values{
		{"username": {"admin"}, "password": {"wrong"}},
		{"username": {"nobody"}, "password": {"s3cret"}},
		{},
	} {
		w := s.remote(t, http.MethodPost, "/login", credentials)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Contains(t, w.Body.String(), "Wrong username or password")
		assert.Empty(t, w.Result().Cookies())
	}
}

// A form that cannot be read is the sender's mistake and is answered as one,
// rather than as a wrong password nobody typed.
func TestE2ESignInReportsAMalformedForm(t *testing.T) {
	s := newStand(t)

	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("%zz"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "203.0.113.9:41000"

	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Malformed form")
}

// A cookie that outlived its session is cleared rather than left in the
// browser, which would keep sending a token that can never work again.
func TestE2EAnExpiredSessionIsCleared(t *testing.T) {
	s := newStand(t)

	w := s.remote(t, http.MethodGet, "/sandboxes", nil,
		&http.Cookie{Name: sessionCookie, Value: "a token nobody issued"})

	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))

	cleared := w.Result().Cookies()
	require.NotEmpty(t, cleared)
	assert.Empty(t, cleared[0].Value, "the browser is told to forget it")
}

// Someone off-box with no cookie at all is sent to the form rather than shown
// the stand's keys.
func TestE2EAnUnknownVisitorIsSentToTheForm(t *testing.T) {
	s := newStand(t)

	w := s.remote(t, http.MethodGet, "/cards", nil)

	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

// The operator at the keyboard is through without a form, and the form knows
// it: a console opened locally is a console already in use.
func TestE2ETheOperatorAtTheKeyboardIsNotAskedToSignIn(t *testing.T) {
	s := newStand(t)

	w := s.get(t, "/login")

	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/", w.Header().Get("Location"))
}

// A console with no local shortcut asks everyone, including the machine it runs
// on, which is what a stand exposed beyond one desk must do.
func TestE2EWithoutTheLocalShortcutEveryoneSignsIn(t *testing.T) {
	s := newStand(t)

	closed, err := newApp(config{Username: "admin", Password: "s3cret"},
		s.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)

	r := httptest.NewRequest(http.MethodGet, "/cards", nil)
	r.RemoteAddr = "127.0.0.1:50000"
	w := httptest.NewRecorder()
	closed.routes().ServeHTTP(w, r)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

// The health check answers without a session, because it is what a container
// runtime asks and it holds no credentials of its own.
func TestE2EHealthzAnswersWithoutASession(t *testing.T) {
	s := newStand(t)

	w := s.remote(t, http.MethodGet, "/healthz", nil)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok", w.Body.String())
}
