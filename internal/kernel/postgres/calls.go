package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
)

// CallStore remembers what a call was answered, so a repeat of it can be given
// the same answer rather than doing the work again.
type CallStore struct {
	pool *Pool
}

// NewCallStore wires the store to a pool.
func NewCallStore(pool *Pool) *CallStore {
	return &CallStore{pool: pool}
}

// Recall returns the response an earlier call with this key was answered.
//
// The body hash is part of the match rather than a check afterwards: an id
// reused for different parameters is a different call, and answering it with
// the earlier response would hide the mistake instead of the retry.
func (s *CallStore) Recall(ctx context.Context, key httpx.CallKey, window time.Duration) ([]byte, bool, error) {
	var response []byte

	err := From(ctx, s.pool).QueryRow(ctx, `
		SELECT response
		FROM control.idempotent_calls
		WHERE sandbox_id = $1 AND method = $2 AND request_id = $3
		  AND body_hash = $4 AND at > now() - $5::interval`,
		key.SandboxID, key.Method, key.RequestID, key.BodyHash,
		fmt.Sprintf("%d milliseconds", window.Milliseconds()),
	).Scan(&response)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("recall call: %w", err)
	}

	return response, true, nil
}

// Remember stores what a call was answered.
//
// A key already stored is overwritten only when the body is the same, which is
// how a retry that arrived while the first was still running settles on one
// answer; a different body under the same id is left to be recorded as its own
// row would be, which the primary key refuses, so the newer call keeps its own
// answer and nothing is replayed for it.
func (s *CallStore) Remember(ctx context.Context, key httpx.CallKey, response []byte) error {
	if _, err := From(ctx, s.pool).Exec(ctx, `
		INSERT INTO control.idempotent_calls
			(sandbox_id, method, request_id, body_hash, response)
		VALUES ($1, $2, $3, $4, $5::jsonb)
		ON CONFLICT (sandbox_id, method, request_id) DO UPDATE
		SET response = excluded.response, at = now()
		WHERE control.idempotent_calls.body_hash = excluded.body_hash`,
		key.SandboxID, key.Method, key.RequestID, key.BodyHash, response,
	); err != nil {
		return fmt.Errorf("remember call: %w", err)
	}

	return nil
}
