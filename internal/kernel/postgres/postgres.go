// Package postgres holds the connection pool, the migration runner and the
// unit-of-work helper every repository uses.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/bakhod1r/payme-mock/migrations"
)

// Pool is the connection pool shared by every repository.
type Pool = pgxpool.Pool

// Connect opens a pool and verifies it can reach the database.
//
// pgxpool.New parses and builds in one step: the pool settings are validated
// during parsing, so splitting the two would leave a branch that cannot fail.
func Connect(ctx context.Context, dsn string) (*Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}

// migrationLocker serialises the migration run.
//
// Every service migrates on start, so a stand coming up runs migrations from
// three processes at once against one database. Without a lock they all read
// the same empty schema table, all try to create the schema, and two of the
// three die on the duplicate. With it the losers wait and then find the work
// already done.
//
// The error is discarded because it reports a bad option and none are given:
// the constructor cannot fail here, and a branch for it would be one no test
// could reach.
var migrationLocker, _ = lock.NewPostgresSessionLocker()

// Migrate brings the schema up to date using the embedded migrations.
func Migrate(ctx context.Context, pool *Pool) error {
	return MigrateFS(ctx, pool, migrations.FS)
}

// MigrateFS runs the migrations in fsys. The filesystem is a parameter so a
// test can supply an empty one and prove that a build shipped without its
// migrations fails loudly instead of starting against an empty schema.
//
// Every service migrates on start, so a stand coming up runs this from three
// processes at once against one database. Without a lock they all read the
// same empty schema table and all try to create the schema, and two of the
// three die on the duplicate. The session lock makes the losers wait and then
// find the work already done.
func MigrateFS(ctx context.Context, pool *Pool, fsys fs.FS) error {
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys,
		goose.WithSessionLocker(migrationLocker))
	if err != nil {
		return fmt.Errorf("prepare migrations: %w", err)
	}

	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	return nil
}

// Querier is what repositories run statements against: either the pool or an
// open transaction, so the same code works inside and outside one.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconnCommandTag, error)
}

// pgconnCommandTag mirrors pgconn.CommandTag without importing it into every
// repository signature.
type pgconnCommandTag interface {
	RowsAffected() int64
	String() string
}

// txKey carries an open transaction through the context, so a use case can
// span several repositories without them knowing about each other.
type txKey struct{}

// WithTx runs fn inside a database transaction, committing on success and
// rolling back on any error or panic.
func WithTx(ctx context.Context, pool *Pool, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		// Already inside a transaction; joining it keeps the whole use case
		// atomic rather than opening a nested one.
		return fn(ctx)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
	}()

	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			return errors.Join(err, rollbackErr)
		}
		return err
	}

	return tx.Commit(ctx)
}

// From returns the transaction in the context, or the pool when there is none.
func From(ctx context.Context, pool *Pool) Querier {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return txQuerier{tx}
	}
	return poolQuerier{pool}
}

type poolQuerier struct{ pool *Pool }

func (p poolQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return p.pool.Query(ctx, sql, args...)
}

func (p poolQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return p.pool.QueryRow(ctx, sql, args...)
}

func (p poolQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconnCommandTag, error) {
	return p.pool.Exec(ctx, sql, args...)
}

type txQuerier struct{ tx pgx.Tx }

func (t txQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.tx.Query(ctx, sql, args...)
}

func (t txQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.tx.QueryRow(ctx, sql, args...)
}

func (t txQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconnCommandTag, error) {
	return t.tx.Exec(ctx, sql, args...)
}
