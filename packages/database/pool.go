package database

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/puddle/v2"
)

// DefaultDrainTimeout bounds how long Swap's background drain and Close wait
// for queries still running on a replaced pool.
const DefaultDrainTimeout = 30 * time.Second

// Pool is a Postgres pool that can be replaced while the server runs, so a
// rotated DATABASE_URL (new password, new role, new host) takes effect without
// a restart. Repositories, packages/auth and /healthz use it like a
// *pgxpool.Pool.
//
// Swap opens the new pool, pings it and runs the probe, then switches
// atomically. Queries already running finish on the old pool, which is
// closed in the background once they are done (bounded by DrainTimeout). A
// call that reached the old pool just as it closed is retried once on the new
// one: pgx fails such a call while acquiring a connection, before any SQL is
// sent, so the retry never runs a statement twice. Safe for concurrent use.
type Pool struct {
	cur     atomic.Pointer[pgxpool.Pool]
	options PoolOptions

	mu       sync.Mutex // serializes Swap and Close
	url      string     // current connection string; never logged
	closed   bool
	draining sync.WaitGroup
}

// PoolOptions configure a Pool. Zero values take the defaults.
type PoolOptions struct {
	// Probe runs against a candidate pool before Swap switches to it (after a
	// ping). Use it to refuse a database without the expected schema. Nil runs
	// "SELECT 1".
	Probe        func(ctx context.Context, candidate *pgxpool.Pool) error
	DrainTimeout time.Duration // default DefaultDrainTimeout
	Logger       *slog.Logger  // default slog.Default() at the time of logging
}

// OpenPool connects to url, pings it and runs the probe.
func OpenPool(ctx context.Context, url string, options PoolOptions) (*Pool, error) {
	if options.DrainTimeout <= 0 {
		options.DrainTimeout = DefaultDrainTimeout
	}
	pool := &Pool{options: options}
	next, err := pool.open(ctx, url)
	if err != nil {
		return nil, err
	}
	pool.cur.Store(next)
	pool.url = url
	return pool, nil
}

// Swap switches to url. It is a no-op when url is unchanged. If the new
// database cannot be reached or fails the probe, the current pool stays in
// use and the error is returned.
func (pool *Pool) Swap(ctx context.Context, url string) error {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.closed {
		return errors.New("database: pool is closed")
	}
	if url == pool.url {
		return nil
	}
	next, err := pool.open(ctx, url)
	if err != nil {
		return err
	}
	old := pool.cur.Swap(next)
	pool.url = url
	pool.log().Info("database: switched to a new pool, draining the old one", "to", describe(next), "from", describe(old))
	pool.drain(old)
	return nil
}

// Current returns the pool in use now. Prefer the Pool's own methods: a
// *pgxpool.Pool kept by a caller is closed after the next Swap.
func (pool *Pool) Current() *pgxpool.Pool { return pool.cur.Load() }

// Close closes the current pool and waits (up to DrainTimeout) for replaced
// pools still draining.
func (pool *Pool) Close() {
	pool.mu.Lock()
	if pool.closed {
		pool.mu.Unlock()
		return
	}
	pool.closed = true
	cur := pool.cur.Load()
	pool.mu.Unlock()
	cur.Close()
	done := make(chan struct{})
	go func() { pool.draining.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(pool.options.DrainTimeout):
		pool.log().Warn("database: replaced pool still draining at shutdown", "timeout", pool.options.DrainTimeout)
	}
}

// Exec runs sql on the current pool.
func (pool *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return retry(pool, func(pgPool *pgxpool.Pool) (pgconn.CommandTag, error) { return pgPool.Exec(ctx, sql, args...) })
}

// Query runs sql on the current pool.
func (pool *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return retry(pool, func(pgPool *pgxpool.Pool) (pgx.Rows, error) { return pgPool.Query(ctx, sql, args...) })
}

// QueryRow runs sql on the current pool. The retry happens in Scan, where
// pgx reports a failed acquire.
func (pool *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	pgPool := pool.cur.Load()
	return &retryRow{pool: pool, used: pgPool, row: pgPool.QueryRow(ctx, sql, args...), retry: func(pgPool *pgxpool.Pool) pgx.Row { return pgPool.QueryRow(ctx, sql, args...) }}
}

// Begin starts a transaction on the current pool. The transaction keeps its
// connection until Commit or Rollback, even across a Swap.
func (pool *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	return retry(pool, func(pgPool *pgxpool.Pool) (pgx.Tx, error) { return pgPool.Begin(ctx) })
}

// Ping checks the current pool (server /healthz).
func (pool *Pool) Ping(ctx context.Context) error {
	_, err := retry(pool, func(pgPool *pgxpool.Pool) (struct{}, error) { return struct{}{}, pgPool.Ping(ctx) })
	return err
}

func retry[Value any](pool *Pool, fn func(*pgxpool.Pool) (Value, error)) (Value, error) {
	pgPool := pool.cur.Load()
	value, err := fn(pgPool)
	if next := pool.cur.Load(); errors.Is(err, puddle.ErrClosedPool) && next != pgPool {
		return fn(next)
	}
	return value, err
}

type retryRow struct {
	pool  *Pool
	used  *pgxpool.Pool
	row   pgx.Row
	retry func(*pgxpool.Pool) pgx.Row
}

func (row *retryRow) Scan(dest ...any) error {
	err := row.row.Scan(dest...)
	if next := row.pool.cur.Load(); errors.Is(err, puddle.ErrClosedPool) && next != row.used {
		return row.retry(next).Scan(dest...)
	}
	return err
}

func (pool *Pool) open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	next, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("database: %w", redactConnErr(err))
	}
	probe := pool.options.Probe
	if probe == nil {
		probe = func(ctx context.Context, pgPool *pgxpool.Pool) error {
			var one int
			return pgPool.QueryRow(ctx, "SELECT 1").Scan(&one)
		}
	}
	if err := next.Ping(ctx); err != nil {
		next.Close()
		return nil, fmt.Errorf("database: ping %s: %w", describe(next), err)
	}
	if err := probe(ctx, next); err != nil {
		next.Close()
		return nil, fmt.Errorf("database: probe %s: %w", describe(next), err)
	}
	return next, nil
}

// drain closes old in the background once its in-flight queries finish.
func (pool *Pool) drain(old *pgxpool.Pool) {
	pool.draining.Add(1)
	done := make(chan struct{})
	go func() { old.Close(); close(done); pool.draining.Done() }()
	go func() {
		select {
		case <-done:
			pool.log().Info("database: old pool drained and closed", "pool", describe(old))
		case <-time.After(pool.options.DrainTimeout):
			pool.log().Warn("database: old pool still has queries running; it closes when they finish", "pool", describe(old), "timeout", pool.options.DrainTimeout)
		}
	}()
}

func (pool *Pool) log() *slog.Logger {
	if pool.options.Logger != nil {
		return pool.options.Logger
	}
	return slog.Default()
}

// MigratedProbe is a PoolOptions.Probe that refuses a database whose goose
// version tables are behind the migrations this binary embeds (table ->
// migrations), so a rotated DATABASE_URL can't switch the server to an empty
// or stale database. Newer versions pass: during a rolling deploy old
// replicas keep working.
func MigratedProbe(sets map[string]fs.FS) func(ctx context.Context, candidate *pgxpool.Pool) error {
	return func(ctx context.Context, pgPool *pgxpool.Pool) error {
		for table, fsys := range sets {
			want, err := latestVersion(fsys)
			if err != nil {
				return err
			}
			var got int64
			err = pgPool.QueryRow(ctx, "SELECT COALESCE(max(version_id), 0) FROM "+pgx.Identifier{table}.Sanitize()+" WHERE is_applied").Scan(&got)
			if err != nil {
				return fmt.Errorf("read %s: %w", table, err)
			}
			if got < want {
				return fmt.Errorf("%s is at version %d, this binary needs %d: run migrations first", table, got, want)
			}
		}
		return nil
	}
}

// latestVersion is the highest NNNNN_ prefix among fsys's .sql files.
func latestVersion(fsys fs.FS) (int64, error) {
	files, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, file := range files {
		prefix, _, _ := strings.Cut(file, "_")
		if value, err := strconv.ParseInt(prefix, 10, 64); err == nil {
			latest = max(latest, value)
		}
	}
	return latest, nil
}

// describe names a pool without its password: user@host:port/database.
func describe(pgPool *pgxpool.Pool) string {
	cc := pgPool.Config().ConnConfig
	return fmt.Sprintf("%s@%s:%d/%s", cc.User, cc.Host, cc.Port, cc.Database)
}

// redactConnErr replaces a parse error with a static message: pgx's own
// redaction is best effort and its cause (a *url.Error) holds the full URL.
func redactConnErr(err error) error {
	if _, ok := errors.AsType[*pgconn.ParseConfigError](err); ok {
		return errors.New("invalid connection string (value not shown)")
	}
	return err
}
