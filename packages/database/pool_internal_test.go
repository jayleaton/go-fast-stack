package database

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The race a Swap opens: a caller loads the old pool, the old pool closes,
// then the caller acquires. These tests close the pool at exactly that point.

func racedPool(t *testing.T) (p *Pool, old, next *pgxpool.Pool) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL") // no dbtest here: it imports this package
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set (run `make test`)")
	}
	var err error
	if old, err = pgxpool.New(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	if next, err = pgxpool.New(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(old.Close)
	t.Cleanup(next.Close)
	p = &Pool{o: PoolOptions{DrainTimeout: DefaultDrainTimeout}}
	p.cur.Store(old)
	return p, old, next
}

func TestRetryOnAPoolClosedBySwap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, old, next := racedPool(t)

	calls := 0
	_, err := retry(p, func(c *pgxpool.Pool) (struct{}, error) {
		calls++
		if calls == 1 {
			p.cur.Store(next)
			old.Close()
		}
		_, err := c.Exec(ctx, "SELECT 1")
		return struct{}{}, err
	})
	if err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d, want a single retry on the new pool", err, calls)
	}

	// No swap happened: a closed pool error is returned, not retried.
	next.Close()
	calls = 0
	if _, err := retry(p, func(c *pgxpool.Pool) (struct{}, error) {
		calls++
		_, err := c.Exec(ctx, "SELECT 1")
		return struct{}{}, err
	}); err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestQueryRowRetriesInScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, old, next := racedPool(t)

	old.Close() // the row below acquires from a closed pool...
	row := &retryRow{p: p, used: old, row: old.QueryRow(ctx, "SELECT 1"), retry: func(c *pgxpool.Pool) pgx.Row { return c.QueryRow(ctx, "SELECT 1") }}
	p.cur.Store(next) // ...that a Swap already replaced
	var one int
	if err := row.Scan(&one); err != nil || one != 1 {
		t.Fatalf("Scan = %v (%d), want a retry on the new pool", err, one)
	}
}
