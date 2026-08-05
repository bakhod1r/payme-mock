package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Amounts travel in tiyin and are read in so'm. The console prints the second
// shape everywhere, so these fix what "everywhere" means.
func TestFormatSum(t *testing.T) {
	tests := []struct {
		name  string
		tiyin int64
		want  string
	}{
		{"zero", 0, "0.00"},
		{"under a so'm", 7, "0.07"},
		{"one so'm", 100, "1.00"},
		{"tiyin are not rounded away", 101, "1.01"},
		{"three digits stay ungrouped", 99900, "999.00"},
		{"four digits are grouped", 100000, "1 000.00"},
		{"a full register", 100000000, "1 000 000.00"},
		{"grouping follows the leading digits", 1234567890, "12 345 678.90"},
		{"negative keeps its sign", -250000, "-2 500.00"},
		{"negative under a so'm", -5, "-0.05"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatSum(tt.tiyin))
		})
	}
}

// A balance move is read by its direction first, so the sign is always shown.
func TestFormatSigned(t *testing.T) {
	assert.Equal(t, "+1 000.00", formatSigned(100000))
	assert.Equal(t, "-1 000.00", formatSigned(-100000))
	assert.Equal(t, "0.00", formatSigned(0), "zero carries no direction")
}

func TestGroupInsertsAGapEveryThreeDigits(t *testing.T) {
	tests := map[string]string{
		"":        "",
		"1":       "1",
		"123":     "123",
		"1234":    "1 234",
		"12345":   "12 345",
		"123456":  "123 456",
		"1234567": "1 234 567",
	}

	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			assert.Equal(t, want, group(in))
		})
	}
}

func TestTemplateFuncsExposesBothFormatters(t *testing.T) {
	funcs := templateFuncs()

	require.Contains(t, funcs, "sum")
	require.Contains(t, funcs, "signed")
}

// Every page must parse, or the console starts and then fails on the first
// request to a screen nobody opened during development.
func TestParseTemplatesCoversEveryPage(t *testing.T) {
	templates, err := parseTemplates()
	require.NoError(t, err)

	for _, page := range []string{
		"login", "sandboxes", "sandbox", "traffic", "entry", "rules", "rule",
		"transactions", "transaction", "cards", "card",
	} {
		assert.Contains(t, templates, page)
	}
}

// A write returns to where it was made. Only a path is accepted: a form that
// could name another site would let a crafted page bounce an operator off the
// console after an action they took on it.
func TestBackTo(t *testing.T) {
	tests := []struct {
		name string
		back string
		want string
	}{
		{"empty falls back", "", "/fallback"},
		{"a path is honoured", "/sandboxes/7", "/sandboxes/7"},
		{"a path with a query is honoured", "/rules?x=1", "/rules?x=1"},
		{"an absolute URL is refused", "https://evil.example/x", "/fallback"},
		{"a scheme-relative URL is refused", "//evil.example/x", "/fallback"},
		{"a bare word is refused", "sandboxes", "/fallback"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{"back": {tt.back}}
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			assert.Equal(t, tt.want, backTo(r, "/fallback"))
		})
	}
}

func TestNoticeReadsWhatTheLastWriteSaid(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/?ok=Sandbox+updated.", nil)
	assert.Equal(t, "Sandbox updated.", notice(r))

	assert.Empty(t, notice(httptest.NewRequest(http.MethodGet, "/", nil)))
}

func TestDoneRedirectsCarryingTheMessage(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/sandboxes/1", nil)

	done(w, r, "/sandboxes/1", "Balance is now 1 000.00.")

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/sandboxes/1?ok=Balance+is+now+1+000.00.", w.Header().Get("Location"))
}

func TestOrZero(t *testing.T) {
	assert.Equal(t, "0", orZero(""))
	assert.Equal(t, "42", orZero("42"))
}
