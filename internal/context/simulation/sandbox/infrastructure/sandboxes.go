// Package infrastructure implements the sandbox repository against PostgreSQL.
//
// Sandboxes are the one table not scoped to a sandbox: they are what the
// scoping is resolved from, so every service reads them before a request has a
// stand to belong to.
package infrastructure

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bakhod1r/payme-mock/internal/context/simulation/sandbox/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
)

// uniqueViolation is the SQLSTATE PostgreSQL raises when a unique index
// rejects a row; here it means the slug or cash register id is taken.
const uniqueViolation = "23505"

// Repository stores sandboxes.
type Repository struct {
	pool *postgres.Pool
}

// NewRepository wires the repository to a pool.
func NewRepository(pool *postgres.Pool) *Repository {
	return &Repository{pool: pool}
}

const columns = `id, slug, name, merchant_id, key, test_key, active_config_id,
	archived, kind, merchant_group, coalesce(merchant_name, '')`

// BySlug loads the sandbox behind an endpoint URL.
// An archived stand is not found. Archiving is how a stand is taken out of use
// while its traffic stays readable, and one that went on answering the API
// after it had been archived would be taken out of use in the console only.
func (r *Repository) BySlug(ctx context.Context, slug string) (*domain.Sandbox, error) {
	row := postgres.From(ctx, r.pool).QueryRow(ctx, `
		SELECT `+columns+` FROM control.sandboxes WHERE slug = $1 AND NOT archived`, slug)

	return scan(row)
}

// ByMerchantID loads the sandbox a caller authenticated as. An archived stand
// is not found here either: the credential is still valid paper, and the stand
// it names is closed.
func (r *Repository) ByMerchantID(ctx context.Context, merchantID string) (*domain.Sandbox, error) {
	row := postgres.From(ctx, r.pool).QueryRow(ctx, `
		SELECT `+columns+` FROM control.sandboxes
		WHERE merchant_id = $1 AND NOT archived`, merchantID)

	return scan(row)
}

// List returns the live sandboxes, newest first. Archived ones are left out:
// they exist to keep their traffic readable, not to serve requests.
func (r *Repository) List(ctx context.Context) ([]*domain.Sandbox, error) {
	rows, err := postgres.From(ctx, r.pool).Query(ctx, `
		SELECT `+columns+` FROM control.sandboxes WHERE NOT archived ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("select sandboxes: %w", err)
	}

	// CollectRows drains, closes and reports a stream that failed partway, so
	// a connection lost mid-result cannot be mistaken for a short list.
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*domain.Sandbox, error) {
		return scan(row)
	})
	if err != nil {
		return nil, fmt.Errorf("read sandboxes: %w", err)
	}

	return out, nil
}

// Create stores a new sandbox, assigning its identifier.
func (r *Repository) Create(ctx context.Context, s *domain.Sandbox) error {
	err := postgres.From(ctx, r.pool).QueryRow(ctx, `
		INSERT INTO control.sandboxes (slug, name, merchant_id, key, test_key,
		                               active_config_id, kind, merchant_group,
		                               merchant_name)
		VALUES ($1, $2, $3, $4, $5, $6, coalesce(nullif($7, ''), 'topup'),
		        nullif($8, ''), nullif($9, ''))
		RETURNING id`,
		s.Slug, s.Name, s.MerchantID, s.Key, s.TestKey, s.ConfigID, s.Kind,
		s.MerchantGroup, s.MerchantName).Scan(&s.ID)

	if isUniqueViolation(err) {
		return domain.ErrDuplicate
	}
	if err != nil {
		return fmt.Errorf("insert sandbox: %w", err)
	}

	return nil
}

// Update persists a rename, a profile switch or an archival.
//
// The credentials are deliberately not updatable here: rotating a key changes
// what every integration must send, so it is its own operation rather than a
// side effect of editing a name.
func (r *Repository) Update(ctx context.Context, s *domain.Sandbox) error {
	tag, err := postgres.From(ctx, r.pool).Exec(ctx, `
		UPDATE control.sandboxes
		SET name = $1, active_config_id = $2, archived = $3,
		    kind = coalesce(nullif($4, ''), kind),
		    merchant_group = nullif($5, ''),
		    merchant_name = nullif($6, '')
		WHERE id = $7`, s.Name, s.ConfigID, s.Archived, s.Kind, s.MerchantGroup,
		s.MerchantName, s.ID)
	if err != nil {
		return fmt.Errorf("update sandbox: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}

	return nil
}

// Delete removes a sandbox and, through the schema's cascades, everything
// recorded under it.
func (r *Repository) Delete(ctx context.Context, id int64) error {
	tag, err := postgres.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM control.sandboxes WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete sandbox: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}

	return nil
}

// Reset clears a sandbox's data while keeping its credentials, so an
// integration under test can start over without repointing at a new endpoint.
//
// The deletes run in one transaction: a half-cleared stand would be worse than
// one that was never cleared, because nothing would say which half survived.
func (r *Repository) Reset(ctx context.Context, id int64) error {
	// Accounts cascade to orders and transactions, and transactions cascade to
	// their events, so the tables Payme owns and the request log are all that
	// need naming alongside them.
	statements := []string{
		`DELETE FROM merchant.accounts WHERE sandbox_id = $1`,
		`DELETE FROM mock.transactions WHERE sandbox_id = $1`,
		`DELETE FROM mock.receipts WHERE sandbox_id = $1`,
		`DELETE FROM mock.cards WHERE sandbox_id = $1`,
		`DELETE FROM control.request_log WHERE sandbox_id = $1`,
	}

	return postgres.WithTx(ctx, r.pool, func(inner context.Context) error {
		var exists bool
		err := postgres.From(inner, r.pool).QueryRow(inner,
			`SELECT EXISTS (SELECT 1 FROM control.sandboxes WHERE id = $1)`, id).Scan(&exists)
		if err != nil {
			return fmt.Errorf("check sandbox: %w", err)
		}
		if !exists {
			return domain.ErrNotFound
		}

		for _, statement := range statements {
			if _, err := postgres.From(inner, r.pool).Exec(inner, statement, id); err != nil {
				return fmt.Errorf("reset sandbox: %w", err)
			}
		}

		return nil
	})
}

// scanner is satisfied by both a single row and a row from a result set.
type scanner interface {
	Scan(dest ...any) error
}

func scan(row scanner) (*domain.Sandbox, error) {
	var (
		s     domain.Sandbox
		group *string
	)

	err := row.Scan(&s.ID, &s.Slug, &s.Name, &s.MerchantID, &s.Key, &s.TestKey,
		&s.ConfigID, &s.Archived, &s.Kind, &group, &s.MerchantName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan sandbox: %w", err)
	}

	if group != nil {
		s.MerchantGroup = *group
	}

	return &s, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}
