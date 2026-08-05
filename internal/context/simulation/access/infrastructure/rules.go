// Package infrastructure implements the address rule store against PostgreSQL.
package infrastructure

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/jackc/pgx/v5"

	"github.com/bakhod1r/payme-mock/internal/context/simulation/access/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
)

// Repository stores the per-sandbox address rules.
type Repository struct {
	pool *postgres.Pool
}

// NewRepository wires the repository to a pool.
func NewRepository(pool *postgres.Pool) *Repository {
	return &Repository{pool: pool}
}

// BySandbox returns a stand's allowlist. A stand with no rules answers
// everyone, so an empty result is an answer rather than a failure.
func (r *Repository) BySandbox(ctx context.Context, sandboxID int64) (domain.Allowlist, error) {
	rows, err := postgres.From(ctx, r.pool).Query(ctx, `
		SELECT id, sandbox_id, cidr::text, note
		FROM control.ip_rules
		WHERE sandbox_id = $1
		ORDER BY id`, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("select ip rules: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Rule, error) {
		var (
			rule domain.Rule
			cidr string
		)

		if err := row.Scan(&rule.ID, &rule.SandboxID, &cidr, &rule.Note); err != nil {
			return domain.Rule{}, err
		}

		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return domain.Rule{}, fmt.Errorf("stored rule %q is not a prefix: %w", cidr, err)
		}
		rule.Prefix = prefix

		return rule, nil
	})
}
