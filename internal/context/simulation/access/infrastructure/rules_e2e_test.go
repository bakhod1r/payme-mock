package infrastructure_test

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/simulation/access/infrastructure"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres/testdb"
)

// The allowlist is the first thing a request meets: before a credential is
// looked at, the stand decides whether this address may reach it at all. Read
// it wrong and either everyone is let in or nobody is, so it is read here
// against a real database and the real cidr column.
type stand struct {
	pool    *postgres.Pool
	repo    *infrastructure.Repository
	ctx     context.Context
	sandbox int64
}

func newStand(t *testing.T) *stand {
	t.Helper()

	pool := testdb.New(t)

	return &stand{
		pool:    pool,
		repo:    infrastructure.NewRepository(pool),
		ctx:     context.Background(),
		sandbox: testdb.Seed(t, pool, "qa"),
	}
}

func (s *stand) allow(t *testing.T, cidr, note string) {
	t.Helper()

	_, err := s.pool.Exec(s.ctx, `
		INSERT INTO control.ip_rules (sandbox_id, cidr, note)
		VALUES ($1, $2::cidr, $3)`, s.sandbox, cidr, note)
	require.NoError(t, err)
}

// A stand nobody has restricted is not a stand nobody may reach, so an empty
// list is an answer rather than a failure.
func TestE2EBySandboxIsEmptyUntilARuleIsWritten(t *testing.T) {
	s := newStand(t)

	list, err := s.repo.BySandbox(s.ctx, s.sandbox)

	require.NoError(t, err)
	assert.Empty(t, list)
	assert.True(t, list.Allows(netip.MustParseAddr("10.0.0.1")),
		"an unrestricted stand answers everyone")
}

// Both shapes a rule takes come back as one thing: a single address is a
// full-length prefix, a network is the prefix it was written as.
func TestE2EBySandboxReadsAddressesAndNetworks(t *testing.T) {
	s := newStand(t)
	s.allow(t, "10.1.2.3/32", "the office machine")
	s.allow(t, "192.168.0.0/16", "the private network")

	list, err := s.repo.BySandbox(s.ctx, s.sandbox)
	require.NoError(t, err)
	require.Len(t, list, 2)

	assert.Equal(t, netip.MustParsePrefix("10.1.2.3/32"), list[0].Prefix)
	assert.Equal(t, "the office machine", list[0].Note)
	assert.Equal(t, s.sandbox, list[0].SandboxID)
	assert.NotZero(t, list[0].ID)

	assert.Equal(t, netip.MustParsePrefix("192.168.0.0/16"), list[1].Prefix)

	assert.True(t, list.Allows(netip.MustParseAddr("10.1.2.3")))
	assert.True(t, list.Allows(netip.MustParseAddr("192.168.44.9")))
	assert.False(t, list.Allows(netip.MustParseAddr("8.8.8.8")),
		"a restricted stand answers only who it was told to")
}

// A rule belongs to one stand. Reading another's would hand a stand an
// allowlist nobody wrote for it, which is the one mistake in an allowlist that
// cannot be noticed by looking at the stand it was meant for.
func TestE2EBySandboxKeepsStandsApart(t *testing.T) {
	s := newStand(t)
	s.allow(t, "10.0.0.1/32", "ours")

	other := testdb.Seed(t, s.pool, "other")

	list, err := s.repo.BySandbox(s.ctx, other)
	require.NoError(t, err)
	assert.Empty(t, list)
}

// A database that went away is reported rather than read as "this stand allows
// nobody" — which is what an empty list would mean here, and the difference
// between a stand nobody restricted and a stand nobody can reach.
func TestE2EBySandboxReportsALostDatabase(t *testing.T) {
	s := newStand(t)
	s.pool.Close()

	_, err := s.repo.BySandbox(s.ctx, s.sandbox)

	assert.ErrorContains(t, err, "select ip rules")
}

// A row that cannot be read is an error, not a rule quietly left out. The note
// is made nullable to produce one, which is the same shape of failure a column
// changed under a running service would cause.
func TestE2EBySandboxReportsAnUnreadableRow(t *testing.T) {
	s := newStand(t)
	s.allow(t, "10.0.0.1/32", "ours")

	_, err := s.pool.Exec(s.ctx, `ALTER TABLE control.ip_rules ALTER COLUMN note DROP NOT NULL`)
	require.NoError(t, err)
	_, err = s.pool.Exec(s.ctx, `UPDATE control.ip_rules SET note = NULL`)
	require.NoError(t, err)

	_, err = s.repo.BySandbox(s.ctx, s.sandbox)

	assert.Error(t, err)
}

// The column holds a network, so nothing but a network can be stored — unless
// the column itself is changed, which is the one way a stored rule can stop
// being a prefix. It is reported as the stored value it is, because a rule
// nobody can parse must not be silently dropped from an allowlist.
func TestE2EBySandboxReportsAStoredValueThatIsNotAPrefix(t *testing.T) {
	s := newStand(t)
	s.allow(t, "10.0.0.1/32", "ours")

	_, err := s.pool.Exec(s.ctx, `ALTER TABLE control.ip_rules ALTER COLUMN cidr TYPE text`)
	require.NoError(t, err)
	_, err = s.pool.Exec(s.ctx, `UPDATE control.ip_rules SET cidr = 'not a network'`)
	require.NoError(t, err)

	_, err = s.repo.BySandbox(s.ctx, s.sandbox)

	assert.ErrorContains(t, err, "not a prefix")
}
