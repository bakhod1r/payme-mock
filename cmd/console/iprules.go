package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	access "github.com/bakhod1r/payme-mock/internal/context/simulation/access/domain"
)

// A stand with no rules answers everyone. Writing the first rule is what turns
// the check on, which is why the screen says so rather than offering a switch:
// an empty list and a disabled list would be two ways to say one thing.

// ipRuleRow is one address rule as the screen shows it.
type ipRuleRow struct {
	ID        int64
	SandboxID int64
	Sandbox   string
	CIDR      string
	Note      string
	Created   string
}

// IPRules lists one stand's address rules.
func (s *store) IPRules(ctx context.Context, sandboxID int64) ([]ipRuleRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.sandbox_id, s.slug, r.cidr::text, r.note,
		       `+stamp("r.created_at")+`
		FROM control.ip_rules r
		JOIN control.sandboxes s ON s.id = r.sandbox_id
		WHERE r.sandbox_id = $1
		ORDER BY r.id`, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("select ip rules: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ipRuleRow, error) {
		var out ipRuleRow
		err := row.Scan(&out.ID, &out.SandboxID, &out.Sandbox, &out.CIDR,
			&out.Note, &out.Created)
		return out, err
	})
}

// errRuleTaken reports an address a stand already admits.
var errRuleTaken = errors.New("that stand already admits that address")

// CreateIPRule admits an address, or a network, to a stand.
func (s *store) CreateIPRule(ctx context.Context, sandboxID int64, raw, note string) error {
	prefix, err := access.ParsePrefix(raw)
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO control.ip_rules (sandbox_id, cidr, note)
		VALUES ($1, $2, $3)`, sandboxID, prefix.String(), note)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return errRuleTaken
	}
	if err != nil {
		return fmt.Errorf("insert ip rule: %w", err)
	}

	return nil
}

// DeleteIPRule stops admitting an address. Removing the last rule opens the
// stand to everyone again, which is the same thing as never having written one.
func (s *store) DeleteIPRule(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM control.ip_rules WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete ip rule: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return errNotFound
	}

	return nil
}

// ---------- screens ----------

func (a *app) createIPRule(w http.ResponseWriter, r *http.Request, user string) {
	id, ok := a.pathID(w, r, user, a.renderSandboxes)
	if !ok {
		return
	}

	back := "/sandboxes/" + strconv.FormatInt(id, 10)

	err := a.store.CreateIPRule(r.Context(), id, r.PostFormValue("cidr"), r.PostFormValue("note"))
	if err != nil {
		a.showSandboxWith(w, r, user, id, err.Error())
		return
	}

	a.log.Info("ip rule created", "sandbox", id, "cidr", r.PostFormValue("cidr"), "by", user)

	// Redirecting after the write keeps a refresh from adding a second rule.
	done(w, r, back, "Address admitted. This stand now answers nobody else.")
}

func (a *app) deleteIPRule(w http.ResponseWriter, r *http.Request, user string) {
	id, ok := a.pathID(w, r, user, a.renderSandboxes)
	if !ok {
		return
	}

	if err := a.store.DeleteIPRule(r.Context(), id); err != nil {
		a.finish(w, r, user, "/sandboxes", "delete ip rule", err, a.renderSandboxes)
		return
	}

	a.log.Info("ip rule deleted", "id", id, "by", user)
	done(w, r, backTo(r, "/sandboxes"), "Address removed.")
}
