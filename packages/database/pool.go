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
	cur atomic.Pointer[pgxpool.Pool]
	o   PoolOptions

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
func OpenPool(ctx context.Context, url string, o PoolOptions) (*Pool, error) {
	if o.DrainTimeout <= 0 {
		o.DrainTimeout = DefaultDrainTimeout
	}
	p := &Pool{o: o}
	next, err := p.open(ctx, url)
	if err != nil {
		return nil, err
	}
	p.cur.Store(next)
	p.url = url
	return p, nil
}

// Swap switches to url. It is a no-op when url is unchanged. If the new
// database cannot be reached or fails the probe, the current pool stays in
// use and the error is returned.
func (p *Pool) Swap(ctx context.Context, url string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("database: pool is closed")
	}
	if url == p.url {
		return nil
	}
	next, err := p.open(ctx, url)
	if err != nil {
		return err
	}
	old := p.cur.Swap(next)
	p.url = url
	p.log().Info("database: switched to a new pool, draining the old one", "to", describe(next), "from", describe(old))
	p.drain(old)
	return nil
}

// Current returns the pool in use now. Prefer the Pool's own methods: a
// *pgxpool.Pool kept by a caller is closed after the next Swap.
func (p *Pool) Current() *pgxpool.Pool { return p.cur.Load() }

// Close closes the current pool and waits (up to DrainTimeout) for replaced
// pools still draining.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	cur := p.cur.Load()
	p.mu.Unlock()
	cur.Close()
	done := make(chan struct{})
	go func() { p.draining.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(p.o.DrainTimeout):
		p.log().Warn("database: replaced pool still draining at shutdown", "timeout", p.o.DrainTimeout)
	}
}

// Exec runs sql on the current pool.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return retry(p, func(c *pgxpool.Pool) (pgconn.CommandTag, error) { return c.Exec(ctx, sql, args...) })
}

// Query runs sql on the current pool.
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return retry(p, func(c *pgxpool.Pool) (pgx.Rows, error) { return c.Query(ctx, sql, args...) })
}

// QueryRow runs sql on the current pool. The retry happens in Scan, where
// pgx reports a failed acquire.
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c := p.cur.Load()
	return &retryRow{p: p, used: c, row: c.QueryRow(ctx, sql, args...), retry: func(c *pgxpool.Pool) pgx.Row { return c.QueryRow(ctx, sql, args...) }}
}

// Begin starts a transaction on the current pool. The transaction keeps its
// connection until Commit or Rollback, even across a Swap.
func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	return retry(p, func(c *pgxpool.Pool) (pgx.Tx, error) { return c.Begin(ctx) })
}

// Ping checks the current pool (server /healthz).
func (p *Pool) Ping(ctx context.Context) error {
	_, err := retry(p, func(c *pgxpool.Pool) (struct{}, error) { return struct{}{}, c.Ping(ctx) })
	return err
}

func retry[T any](p *Pool, fn func(*pgxpool.Pool) (T, error)) (T, error) {
	c := p.cur.Load()
	v, err := fn(c)
	if next := p.cur.Load(); errors.Is(err, puddle.ErrClosedPool) && next != c {
		return fn(next)
	}
	return v, err
}

type retryRow struct {
	p     *Pool
	used  *pgxpool.Pool
	row   pgx.Row
	retry func(*pgxpool.Pool) pgx.Row
}

func (r *retryRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	if next := r.p.cur.Load(); errors.Is(err, puddle.ErrClosedPool) && next != r.used {
		return r.retry(next).Scan(dest...)
	}
	return err
}

func (p *Pool) open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	next, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("database: %w", redactConnErr(err))
	}
	probe := p.o.Probe
	if probe == nil {
		probe = func(ctx context.Context, c *pgxpool.Pool) error {
			var one int
			return c.QueryRow(ctx, "SELECT 1").Scan(&one)
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
func (p *Pool) drain(old *pgxpool.Pool) {
	p.draining.Add(1)
	done := make(chan struct{})
	go func() { old.Close(); close(done); p.draining.Done() }()
	go func() {
		select {
		case <-done:
			p.log().Info("database: old pool drained and closed", "pool", describe(old))
		case <-time.After(p.o.DrainTimeout):
			p.log().Warn("database: old pool still has queries running; it closes when they finish", "pool", describe(old), "timeout", p.o.DrainTimeout)
		}
	}()
}

func (p *Pool) log() *slog.Logger {
	if p.o.Logger != nil {
		return p.o.Logger
	}
	return slog.Default()
}

// MigratedProbe is a PoolOptions.Probe that refuses a database whose goose
// version tables are behind the migrations this binary embeds (table ->
// migrations), so a rotated DATABASE_URL can't switch the server to an empty
// or stale database. Newer versions pass: during a rolling deploy old
// replicas keep working.
func MigratedProbe(sets map[string]fs.FS) func(ctx context.Context, candidate *pgxpool.Pool) error {
	return func(ctx context.Context, c *pgxpool.Pool) error {
		for table, fsys := range sets {
			want, err := latestVersion(fsys)
			if err != nil {
				return err
			}
			var got int64
			err = c.QueryRow(ctx, "SELECT COALESCE(max(version_id), 0) FROM "+pgx.Identifier{table}.Sanitize()+" WHERE is_applied").Scan(&got)
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
	for _, f := range files {
		prefix, _, _ := strings.Cut(f, "_")
		if v, err := strconv.ParseInt(prefix, 10, 64); err == nil {
			latest = max(latest, v)
		}
	}
	return latest, nil
}

// describe names a pool without its password: user@host:port/database.
func describe(c *pgxpool.Pool) string {
	cc := c.Config().ConnConfig
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
