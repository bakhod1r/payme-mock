package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/kernel/postgres/testdb"
)

// The console is the one screen that holds every stand's keys, so how it starts
// and who it lets in are as much a part of it as the screens themselves.

// An address rule is how a stand stops answering everyone. It is written and
// removed from the stand's own page, because that is where the operator is when
// they decide it.
func TestE2EAddressRuleIsAddedAndRemoved(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "access", "topup", 100000)
	stand := "/sandboxes/" + strconv.FormatInt(sandbox.ID, 10)

	added := s.post(t, stand+"/access", url.Values{
		"cidr": {"10.0.0.0/8"},
		"note": {"the office"},
	})
	path, message := location(t, added)
	assert.Equal(t, stand, path)
	assert.Contains(t, message, "admitted")

	page := s.get(t, stand)
	require.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), "10.0.0.0/8")
	assert.Contains(t, page.Body.String(), "the office")

	var ruleID int64
	require.NoError(t, s.store.pool.QueryRow(context.Background(),
		`SELECT id FROM control.ip_rules WHERE sandbox_id = $1`, sandbox.ID).Scan(&ruleID))

	removed := s.post(t, "/access/"+strconv.FormatInt(ruleID, 10)+"/delete",
		url.Values{"back": {stand}})
	back, gone := location(t, removed)
	assert.Equal(t, stand, back)
	assert.Contains(t, gone, "removed")

	var left int
	require.NoError(t, s.store.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM control.ip_rules WHERE id = $1`, ruleID).Scan(&left))
	assert.Zero(t, left, "the stand answers everyone again")
}

// A single address is written as a full-length prefix, so an operator can type
// either and mean what they typed.
func TestE2EAddressRuleTakesASingleAddress(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "access-one", "topup", 100000)

	w := s.post(t, "/sandboxes/"+strconv.FormatInt(sandbox.ID, 10)+"/access",
		url.Values{"cidr": {"203.0.113.9"}})
	_, message := location(t, w)

	assert.Contains(t, message, "admitted")

	var stored string
	require.NoError(t, s.store.pool.QueryRow(context.Background(),
		`SELECT cidr::text FROM control.ip_rules WHERE sandbox_id = $1`, sandbox.ID).Scan(&stored))
	assert.Equal(t, "203.0.113.9/32", stored)
}

// An address that is not one is refused on the stand's own page, where the
// operator can see what they typed.
func TestE2EAddressRuleRefusesWhatIsNotAnAddress(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "access-bad", "topup", 100000)

	w := s.post(t, "/sandboxes/"+strconv.FormatInt(sandbox.ID, 10)+"/access",
		url.Values{"cidr": {"somewhere"}})

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "somewhere")
}

// Acting on a row that is not there is answered rather than crashed into.
func TestE2EAddressRuleActionsNeedARowThatExists(t *testing.T) {
	s := newStand(t)

	for _, path := range []string{"/sandboxes/nope/access", "/access/nope/delete"} {
		w := s.post(t, path, url.Values{"cidr": {"10.0.0.0/8"}})

		assert.Equal(t, http.StatusOK, w.Code, path)
		assert.Contains(t, w.Body.String(), "That row is gone", path)
	}
}

// A logged call can be copied as the command that would make it again, which is
// the one thing a page of bodies cannot be pasted into a terminal as.
func TestE2ETrafficEntryIsCopiedAsACurl(t *testing.T) {
	s := newStand(t)
	sandbox := s.newSandbox(t, "curl", "topup", 100000)

	var id int64
	require.NoError(t, s.store.pool.QueryRow(context.Background(), `
		INSERT INTO control.request_log
			(sandbox_id, service, direction, method, http_status, duration_ms,
			 request_body, request_headers)
		VALUES ($1, 'paymemock', 'in', 'cards.check', 200, 3,
		        '{"method":"cards.check"}'::jsonb, '{"X-Auth":"merchant-id"}'::jsonb)
		RETURNING id`, sandbox.ID).Scan(&id))

	entry := "/traffic/" + strconv.FormatInt(id, 10)

	page := s.get(t, entry)
	require.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), "curl -i -X POST")

	command := s.get(t, entry+"/curl")
	require.Equal(t, http.StatusOK, command.Code)
	assert.Contains(t, command.Body.String(), "-H 'X-Auth: merchant-id'")
	assert.Contains(t, command.Header().Get("Content-Type"), "text/plain")
}

// A command for a call that is not there is a wrong address, the same as the
// page for it.
func TestE2ETrafficCurlOfACallThatIsNotThere(t *testing.T) {
	s := newStand(t)

	assert.Equal(t, http.StatusNotFound, s.get(t, "/traffic/999999/curl").Code)
	assert.Equal(t, http.StatusNotFound, s.get(t, "/traffic/nope/curl").Code)
}

// ---------- the service itself ----------

// The console boots the way its own main boots it, serves a screen, and stops
// when it is told to.
func TestE2EConsoleStartsServesAndStops(t *testing.T) {
	_, url := testdb.NewWithURL(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	t.Setenv("DATABASE_URL", url)
	t.Setenv("HTTP_ADDR", addr)
	t.Setenv("CONSOLE_USER", "admin")
	t.Setenv("CONSOLE_PASSWORD", "s3cret")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode == http.StatusOK
	}, 30*time.Second, 100*time.Millisecond, "the console never came up")

	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the console did not stop when it was told to")
	}
}

// A database it cannot reach is reported rather than served around: a console
// that came up without one would show every stand as gone.
func TestConsoleReportsADatabaseItCannotReach(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://payme:payme@127.0.0.1:1/paymemock?sslmode=disable&connect_timeout=1")
	t.Setenv("CONSOLE_USER", "admin")
	t.Setenv("CONSOLE_PASSWORD", "s3cret")

	err := run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	assert.Error(t, err)
}

// A configuration that cannot be read stops the console before it opens
// anything.
func TestConsoleReportsAConfigurationItCannotRead(t *testing.T) {
	// The password has no default: a console that came up without one would be
	// a console anyone can open.
	t.Setenv("DATABASE_URL", "postgres://payme:payme@127.0.0.1:1/paymemock?sslmode=disable")
	t.Setenv("CONSOLE_PASSWORD", "")

	err := run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	assert.Error(t, err)
}

// An address already taken is the ordinary way a service fails to start.
func TestE2EConsoleReportsAnAddressItCannotListenOn(t *testing.T) {
	_, dsn := testdb.NewWithURL(t)

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = taken.Close() }()

	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("HTTP_ADDR", taken.Addr().String())
	t.Setenv("CONSOLE_USER", "admin")
	t.Setenv("CONSOLE_PASSWORD", "s3cret")

	err = run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	assert.Error(t, err)
}

// main is the entry point, and what it does with a failure — say so and leave
// with a status a container runtime can read — is the one decision it makes.
func TestMainReportsAFailedStart(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://payme:payme@127.0.0.1:1/paymemock?sslmode=disable")
	t.Setenv("CONSOLE_PASSWORD", "")

	var status int
	exit = func(code int) { status = code }
	defer func() { exit = osExit }()

	main()

	assert.Equal(t, 1, status)
}
