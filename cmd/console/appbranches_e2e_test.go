package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A console that cannot draw its own pages must not start: every screen it
// serves would be a blank one, and a blank screen about a stand holding money
// is worse than a service that refused to come up.
func TestConsoleWillNotStartWithoutItsPages(t *testing.T) {
	_, err := parseTemplatesFrom(fstest.MapFS{
		"console/layout.html": &fstest.MapFile{Data: []byte(`{{define "layout"}}{{end}}`)},
	})

	assert.ErrorContains(t, err, "parse")
}

// newApp is where that refusal is answered: it does not hand back a console
// that cannot draw.
func TestNewAppRefusesAConsoleThatCannotDraw(t *testing.T) {
	// The pages are embedded, so the only way to see this is to ask the same
	// question of a filesystem that is missing them.
	_, err := parseTemplatesFrom(fstest.MapFS{})

	require.Error(t, err)
}

// A page that fails halfway out cannot be turned into an error: the status line
// has already gone. What is left is to record it, and to leave the operator
// with the part that did arrive rather than a hang.
func TestRenderRecordsAPageThatFailedHalfway(t *testing.T) {
	s := newStand(t)

	var logged testLog
	s.app.log = slog.New(&logged)

	s.app.render(brokenWriter{}, "sandboxes", view{Title: "Cashboxes"})

	assert.True(t, logged.sawError, "the operator is told the page did not go out")
}

// brokenWriter is a client that went away mid-page, which is the ordinary way a
// render fails.
type brokenWriter struct{}

func (brokenWriter) Header() http.Header       { return http.Header{} }
func (brokenWriter) WriteHeader(int)           {}
func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("the client went away") }

// testLog records whether anything was logged at error level.
type testLog struct {
	slog.Handler
	sawError bool
}

func (l *testLog) Enabled(_ context.Context, level slog.Level) bool { return true }

func (l *testLog) Handle(_ context.Context, record slog.Record) error {
	if record.Level >= slog.LevelError {
		l.sawError = true
	}
	return nil
}

func (l *testLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *testLog) WithGroup(string) slog.Handler      { return l }

// The forms that take a body before they take a row: a submission the parser
// cannot read is the sender's mistake, and the screen says so.
func TestE2ETheRemainingFormsReportAMalformedSubmission(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "malformed-rest", "topup", 100000)

	for _, path := range []string{
		"/sandboxes",
		"/rules",
		"/accounts/" + strconv.FormatInt(sandbox.AccountID, 10) + "/block",
	} {
		t.Run(path, func(t *testing.T) {
			w := s.postRaw(t, path, "%zz")

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, w.Body.String(), "Malformed form")
		})
	}
}

// A screen is more than its list: the stands list carries the profiles a stand
// can be switched to, the rules screen carries the error catalog a rule answers
// with, and the payments screen carries the stands it can be narrowed by. Any
// of them failing is the screen failing, because a form drawn without its
// choices would offer none.
func TestE2EAScreenFailsWhenWhatItIsDrawnWithFails(t *testing.T) {
	tests := map[string]struct {
		path string
		drop string
	}{
		"the stands list without its profiles": {
			path: "/sandboxes",
			drop: `DROP TABLE control.configs CASCADE`,
		},
		"the rules screen without its error catalog": {
			path: "/rules",
			drop: `DROP TABLE control.error_catalog`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := newStand(t)
			s.newSandbox(t, "drawn", "topup", 100000)

			_, err := s.store.pool.Exec(t.Context(), tt.drop)
			require.NoError(t, err)

			w := s.get(t, tt.path)

			assert.Equal(t, http.StatusInternalServerError, w.Code)
		})
	}
}

// A rule that cannot be written is not a rule: the screen says so rather than
// redirecting to a list the rule is not on.
func TestE2ERuleThatCannotBeWritten(t *testing.T) {
	s := newStand(t)

	_, err := s.store.pool.Exec(t.Context(), `DROP TABLE control.fault_rules CASCADE`)
	require.NoError(t, err)

	w := s.post(t, "/rules", url.Values{
		"service":    {"merchant"},
		"method":     {"*"},
		"action":     {"rpc_error"},
		"error_code": {"-31008"},
	})

	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"a rule against a stand that is not there is refused")
}

// The live panel reads the same payments through a window, and a panel that
// cannot read is the screen failing: it is the half of that screen an operator
// opened it for.
func TestE2EPaymentsScreenFailsWhenTheLivePanelCannot(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "live-broken", "topup", 100000)
	s.addTransaction(t, sandbox, s.addOrder(t, sandbox, 1000), "txn-live", 1)

	// The column only the live panel reads goes, so the list still works and the
	// panel does not, which is the difference this covers.
	_, err := s.store.pool.Exec(t.Context(),
		`ALTER TABLE merchant.transactions DROP COLUMN receivers`)
	require.NoError(t, err)

	// The payments screen an operator lands on is a different one; this list is
	// what a failed edit is drawn back onto, so that is how it is reached.
	var id int64
	require.NoError(t, s.store.pool.QueryRow(t.Context(),
		`SELECT id FROM merchant.transactions WHERE payme_id = 'txn-live'`).Scan(&id))

	w := s.post(t, "/transactions/"+strconv.FormatInt(id, 10), url.Values{"state": {"nonsense"}})

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
