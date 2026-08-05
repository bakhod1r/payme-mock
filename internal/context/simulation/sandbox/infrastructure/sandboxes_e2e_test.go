package infrastructure_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/simulation/sandbox/domain"
	"github.com/bakhod1r/payme-mock/internal/context/simulation/sandbox/infrastructure"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres/testdb"
)

const amount = int64(500000)

type stand struct {
	pool *postgres.Pool
	repo *infrastructure.Repository
	ctx  context.Context
	// seeded is the sandbox testdb inserts, which every test starts from.
	seeded int64
}

func newStand(t *testing.T) *stand {
	t.Helper()

	pool := testdb.New(t)

	return &stand{
		pool:   pool,
		repo:   infrastructure.NewRepository(pool),
		ctx:    context.Background(),
		seeded: testdb.Seed(t, pool, "qa"),
	}
}

// generator hands out fixed credentials so a test can assert on them.
type generator struct {
	merchantID string
	keys       []string
	issued     int
}

func (g *generator) MerchantID() string { return g.merchantID }

func (g *generator) Key() string {
	key := g.keys[g.issued%len(g.keys)]
	g.issued++
	return key
}

func newSandbox(t *testing.T, slug string) *domain.Sandbox {
	t.Helper()

	s, err := domain.New(slug, "", &generator{
		merchantID: "merchant-" + slug,
		keys:       []string{"live-" + slug, "test-" + slug},
	})
	require.NoError(t, err)

	return s
}

func TestE2ECreateAssignsTheIdentifier(t *testing.T) {
	s := newStand(t)
	sandbox := newSandbox(t, "acme")

	require.NoError(t, s.repo.Create(s.ctx, sandbox))

	assert.NotZero(t, sandbox.ID, "the row's identifier is assigned on insert")

	got, err := s.repo.BySlug(s.ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, sandbox.ID, got.ID)
	assert.Equal(t, "acme", got.Name, "a blank name falls back to the slug")
	assert.Equal(t, "merchant-acme", got.MerchantID)
	assert.Equal(t, "live-acme", got.Key)
	assert.Equal(t, "test-acme", got.TestKey)
	assert.False(t, got.Archived)
	assert.Nil(t, got.ConfigID)
}

// A slug appears in every endpoint URL already handed out, so a second
// sandbox may never take one that is in use.
func TestE2ECreateRejectsADuplicateSlug(t *testing.T) {
	s := newStand(t)
	require.NoError(t, s.repo.Create(s.ctx, newSandbox(t, "acme")))

	err := s.repo.Create(s.ctx, newSandbox(t, "acme"))

	assert.ErrorIs(t, err, domain.ErrDuplicate)
}

func TestE2ECreateRejectsADuplicateMerchantID(t *testing.T) {
	s := newStand(t)
	require.NoError(t, s.repo.Create(s.ctx, newSandbox(t, "acme")))

	clash := newSandbox(t, "other")
	clash.MerchantID = "merchant-acme"

	err := s.repo.Create(s.ctx, clash)

	assert.ErrorIs(t, err, domain.ErrDuplicate)
}

func TestE2EByMerchantIDFindsTheStand(t *testing.T) {
	s := newStand(t)
	sandbox := newSandbox(t, "acme")
	require.NoError(t, s.repo.Create(s.ctx, sandbox))

	got, err := s.repo.ByMerchantID(s.ctx, "merchant-acme")

	require.NoError(t, err)
	assert.Equal(t, sandbox.ID, got.ID)
}

func TestE2ELookupsReportAMiss(t *testing.T) {
	s := newStand(t)

	_, err := s.repo.BySlug(s.ctx, "nothing-here")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	_, err = s.repo.ByMerchantID(s.ctx, "nothing-here")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// Archived stands keep their traffic readable but stop serving, so they are
// left out of the list every service resolves against.
func TestE2EListSkipsArchivedStandsNewestFirst(t *testing.T) {
	s := newStand(t)

	first := newSandbox(t, "first")
	require.NoError(t, s.repo.Create(s.ctx, first))

	second := newSandbox(t, "second")
	require.NoError(t, s.repo.Create(s.ctx, second))

	gone := newSandbox(t, "archived")
	require.NoError(t, s.repo.Create(s.ctx, gone))
	gone.Archived = true
	require.NoError(t, s.repo.Update(s.ctx, gone))

	list, err := s.repo.List(s.ctx)

	require.NoError(t, err)
	require.Len(t, list, 3, "the two created stands plus the seeded one")
	assert.Equal(t, second.ID, list[0].ID)
	assert.Equal(t, first.ID, list[1].ID)
	assert.Equal(t, s.seeded, list[2].ID)
}

func TestE2EUpdatePersistsTheChange(t *testing.T) {
	s := newStand(t)
	sandbox := newSandbox(t, "acme")
	require.NoError(t, s.repo.Create(s.ctx, sandbox))

	configID := s.configID(t)
	sandbox.Name = "Acme staging"
	sandbox.ConfigID = &configID
	sandbox.MerchantGroup = "acme-group"

	require.NoError(t, s.repo.Update(s.ctx, sandbox))

	got, err := s.repo.BySlug(s.ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, "Acme staging", got.Name)
	require.NotNil(t, got.ConfigID)
	assert.Equal(t, configID, *got.ConfigID)
	assert.Equal(t, "live-acme", got.Key, "editing a name does not rotate credentials")
	assert.Equal(t, "acme-group", got.MerchantGroup,
		"the merchant these registers belong to is stored, and reads back")

	// Clearing it puts the stand back on its own cards.
	sandbox.MerchantGroup = ""
	require.NoError(t, s.repo.Update(s.ctx, sandbox))

	got, err = s.repo.BySlug(s.ctx, "acme")
	require.NoError(t, err)
	assert.Empty(t, got.MerchantGroup)
}

func TestE2EUpdateReportsAMissingStand(t *testing.T) {
	s := newStand(t)

	err := s.repo.Update(s.ctx, &domain.Sandbox{ID: 999999, Name: "ghost"})

	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestE2EDeleteRemovesTheStandAndItsData(t *testing.T) {
	s := newStand(t)
	testdb.SeedAccountAndOrder(t, s.pool, s.seeded, amount)

	require.NoError(t, s.repo.Delete(s.ctx, s.seeded))

	_, err := s.repo.BySlug(s.ctx, "qa")
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.Zero(t, s.count(t, `SELECT count(*) FROM merchant.accounts WHERE sandbox_id = $1`, s.seeded),
		"the schema cascades a deleted stand's data with it")
}

func TestE2EDeleteReportsAMissingStand(t *testing.T) {
	s := newStand(t)

	err := s.repo.Delete(s.ctx, 999999)

	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// Reset is what lets an integration start over without repointing at a new
// endpoint, so the data goes and the credentials stay.
func TestE2EResetClearsDataButKeepsCredentials(t *testing.T) {
	s := newStand(t)
	testdb.SeedAccountAndOrder(t, s.pool, s.seeded, amount)
	s.seedTraffic(t)

	require.NoError(t, s.repo.Reset(s.ctx, s.seeded))

	assert.Zero(t, s.count(t, `SELECT count(*) FROM merchant.accounts WHERE sandbox_id = $1`, s.seeded))
	assert.Zero(t, s.count(t, `SELECT count(*) FROM merchant.orders WHERE sandbox_id = $1`, s.seeded))
	assert.Zero(t, s.count(t, `SELECT count(*) FROM control.request_log WHERE sandbox_id = $1`, s.seeded))

	got, err := s.repo.BySlug(s.ctx, "qa")
	require.NoError(t, err)
	assert.Equal(t, s.seeded, got.ID)
	assert.Equal(t, "live-key", got.Key)
	assert.Equal(t, "test-key", got.TestKey)
}

func TestE2EResetReportsAMissingStand(t *testing.T) {
	s := newStand(t)

	err := s.repo.Reset(s.ctx, 999999)

	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// One stand's reset may not touch another's data.
func TestE2EResetIsScopedToOneStand(t *testing.T) {
	s := newStand(t)
	other := testdb.Seed(t, s.pool, "other")
	testdb.SeedAccountAndOrder(t, s.pool, other, amount)
	testdb.SeedAccountAndOrder(t, s.pool, s.seeded, amount)

	require.NoError(t, s.repo.Reset(s.ctx, s.seeded))

	assert.Equal(t, 1, s.count(t, `SELECT count(*) FROM merchant.accounts WHERE sandbox_id = $1`, other))
}

// A half-cleared stand would be worse than an uncleared one, because nothing
// would say which half survived, so a failure inside the reset rolls it back.
func TestE2EResetRollsBackWhenTheDatabaseGoesAway(t *testing.T) {
	s := newStand(t)
	testdb.SeedAccountAndOrder(t, s.pool, s.seeded, amount)
	s.pool.Close()

	err := s.repo.Reset(s.ctx, s.seeded)

	require.Error(t, err)
	assert.NotErrorIs(t, err, domain.ErrNotFound, "a lost database is not a missing stand")
}

// A stand that exists but cannot be cleared is reported as a failure rather
// than as a success that left data behind.
func TestE2EResetReportsAFailedDelete(t *testing.T) {
	s := newStand(t)
	testdb.SeedAccountAndOrder(t, s.pool, s.seeded, amount)

	// A table the reset names is taken away, which is what a half-applied
	// migration would look like from here.
	_, err := s.pool.Exec(s.ctx, `ALTER TABLE mock.cards RENAME TO cards_moved`)
	require.NoError(t, err)

	err = s.repo.Reset(s.ctx, s.seeded)

	assert.ErrorContains(t, err, "reset sandbox")

	// The deletes that did run are rolled back with the rest.
	assert.Equal(t, 1, s.count(t, `SELECT count(*) FROM merchant.accounts WHERE sandbox_id = $1`, s.seeded))
}

// A reset that cannot even establish whether the stand exists says so, rather
// than reporting the stand as missing and letting a caller move on.
func TestE2EResetReportsAFailedExistenceCheck(t *testing.T) {
	s := newStand(t)

	_, err := s.pool.Exec(s.ctx, `ALTER TABLE control.sandboxes RENAME TO sandboxes_moved`)
	require.NoError(t, err)

	err = s.repo.Reset(s.ctx, s.seeded)

	assert.ErrorContains(t, err, "check sandbox")
	assert.NotErrorIs(t, err, domain.ErrNotFound)
}

// A database that has gone away must surface as an error from every method,
// not as an empty result that would read as "no such stand".
func TestE2EEveryMethodReportsALostDatabase(t *testing.T) {
	s := newStand(t)
	s.pool.Close()

	t.Run("BySlug", func(t *testing.T) {
		_, err := s.repo.BySlug(s.ctx, "qa")

		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("List", func(t *testing.T) {
		_, err := s.repo.List(s.ctx)

		assert.ErrorContains(t, err, "select sandboxes")
	})

	t.Run("Create", func(t *testing.T) {
		err := s.repo.Create(s.ctx, newSandbox(t, "acme"))

		assert.ErrorContains(t, err, "insert sandbox")
	})

	t.Run("Update", func(t *testing.T) {
		err := s.repo.Update(s.ctx, &domain.Sandbox{ID: s.seeded})

		assert.ErrorContains(t, err, "update sandbox")
	})

	t.Run("Delete", func(t *testing.T) {
		err := s.repo.Delete(s.ctx, s.seeded)

		assert.ErrorContains(t, err, "delete sandbox")
	})
}

// A row that no longer fits the domain must be reported, not skipped: a list
// that silently dropped it would read as a shorter, healthy one.
func TestE2EScanReportsABadRow(t *testing.T) {
	s := newStand(t)

	// The schema forbids a NULL name, so the constraint is lifted to produce
	// the corrupt row a scan has to survive.
	_, err := s.pool.Exec(s.ctx, `ALTER TABLE control.sandboxes ALTER COLUMN name DROP NOT NULL`)
	require.NoError(t, err)

	_, err = s.pool.Exec(s.ctx, `UPDATE control.sandboxes SET name = NULL WHERE id = $1`, s.seeded)
	require.NoError(t, err)

	_, err = s.repo.BySlug(s.ctx, "qa")
	assert.ErrorContains(t, err, "scan sandbox")

	_, err = s.repo.List(s.ctx)
	assert.ErrorContains(t, err, "read sandboxes")
}

func (s *stand) configID(t *testing.T) int64 {
	t.Helper()

	var id int64
	require.NoError(t, s.pool.QueryRow(s.ctx,
		`SELECT id FROM control.configs LIMIT 1`).Scan(&id))

	return id
}

func (s *stand) seedTraffic(t *testing.T) {
	t.Helper()

	_, err := s.pool.Exec(s.ctx, `
		INSERT INTO control.request_log (sandbox_id, service, direction, method, duration_ms)
		VALUES ($1, 'merchant', 'in', 'CheckPerformTransaction', 12)`, s.seeded)
	require.NoError(t, err)
}

func (s *stand) count(t *testing.T, query string, args ...any) int {
	t.Helper()

	var n int
	require.NoError(t, s.pool.QueryRow(context.Background(), query, args...).Scan(&n))

	return n
}
