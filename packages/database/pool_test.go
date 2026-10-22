package database_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jayleaton/ai-patterns-go/packages/auth"
	"github.com/jayleaton/ai-patterns-go/packages/database"
	"github.com/jayleaton/ai-patterns-go/packages/database/dbtest"
	"github.com/jayleaton/ai-patterns-go/packages/database/model"
)

// syncBuffer is a log sink safe for the pool's background goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// twoDatabases returns the connection strings of two migrated databases and
// their names: a rotation from one to the other stands in for a new password
// or host.
func twoDatabases(t *testing.T) (urlA, urlB, nameA, nameB string) {
	a, b := dbtest.New(t, dbtest.App), dbtest.New(t, dbtest.App)
	return a.Config().ConnString(), b.Config().ConnString(), a.Config().ConnConfig.Database, b.Config().ConnConfig.Database
}

func currentDB(ctx context.Context, p *database.Pool) (string, error) {
	var name string
	err := p.QueryRow(ctx, "SELECT current_database()").Scan(&name)
	return name, err
}

func openPool(t *testing.T, url string, o database.PoolOptions) (*database.Pool, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	o.Logger = slog.New(slog.NewTextHandler(logs, nil))
	p, err := database.OpenPool(context.Background(), url, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p, logs
}

func TestPoolSwapUnderLoadLosesNoQueries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	urlA, urlB, nameA, nameB := twoDatabases(t)
	p, logs := openPool(t, urlA, database.PoolOptions{})
	repo := database.NewUserRepository(p)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var queries, failures atomic.Int64
	var firstErr atomic.Value
	for range 8 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, err := currentDB(ctx, p)
				if err == nil {
					_, err = repo.GetByID(ctx, "nobody")
					if errors.Is(err, model.ErrNotFound) {
						err = nil
					}
				}
				if err == nil {
					_, err = p.Exec(ctx, "SELECT 1")
				}
				queries.Add(1)
				if err != nil {
					failures.Add(1)
					firstErr.CompareAndSwap(nil, err)
				}
			}
		})
	}

	// Let the loop warm up on A, swap, let it run on B.
	for queries.Load() < 200 {
		runtime.Gosched()
	}
	if err := p.Swap(ctx, urlB); err != nil {
		t.Fatal(err)
	}
	for n := queries.Load(); queries.Load() < n+200; {
		runtime.Gosched()
	}
	close(stop)
	wg.Wait()

	if f := failures.Load(); f != 0 {
		t.Fatalf("%d of %d query rounds failed during the swap: %v", f, queries.Load(), firstErr.Load())
	}
	if got, err := currentDB(ctx, p); err != nil || got != nameB {
		t.Fatalf("after swap: db=%s err=%v, want %s", got, err, nameB)
	}
	if err := p.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	p.Close() // waits for the old pool to drain
	if out := logs.String(); !strings.Contains(out, "switched to a new pool") || !strings.Contains(out, nameA) || !strings.Contains(out, "drained and closed") {
		t.Fatalf("logs:\n%s", out)
	}
}

func TestPoolTransactionSurvivesSwap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	urlA, urlB, nameA, nameB := twoDatabases(t)
	p, _ := openPool(t, urlA, database.PoolOptions{})

	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Swap(ctx, urlB); err != nil {
		t.Fatal(err)
	}
	// The open transaction keeps its connection on A; new work goes to B.
	var inTx string
	if err := tx.QueryRow(ctx, "SELECT current_database()").Scan(&inTx); err != nil || inTx != nameA {
		t.Fatalf("tx db=%s err=%v, want %s", inTx, err, nameA)
	}
	if got, _ := currentDB(ctx, p); got != nameB {
		t.Fatalf("new queries on %s, want %s", got, nameB)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit after swap: %v", err)
	}
}

func TestPoolSwapFailureKeepsTheCurrentPool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	urlA, urlB, nameA, _ := twoDatabases(t)
	refuse := errors.New("schema mismatch")
	var probes atomic.Int32
	p, logs := openPool(t, urlA, database.PoolOptions{Probe: func(ctx context.Context, c *pgxpool.Pool) error {
		if probes.Add(1) > 1 {
			return refuse
		}
		return nil
	}})

	for _, tt := range []struct {
		url, want string
	}{
		{"postgres://app:hunter2-secret@127.0.0.1:1/app?connect_timeout=1", "ping"},
		{"postgres://app:hunter2-secret@[::1/app", "invalid connection string"},
		{urlB, "schema mismatch"},
	} {
		err := p.Swap(ctx, tt.url)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Fatalf("Swap(%s) = %v, want %q", tt.url, err, tt.want)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("error leaks the password: %v", err)
		}
		if got, err := currentDB(ctx, p); err != nil || got != nameA {
			t.Fatalf("after failed swap: db=%s err=%v, want %s", got, err, nameA)
		}
	}
	if !errors.Is(p.Swap(ctx, urlB), refuse) {
		t.Fatal("probe error not wrapped")
	}
	if strings.Contains(logs.String(), "hunter2") {
		t.Fatal("log leaks the password")
	}

	// Same URL: nothing to do. After Close, Swap refuses.
	if err := p.Swap(ctx, urlA); err != nil {
		t.Fatal(err)
	}
	p.Close()
	p.Close()
	if err := p.Swap(ctx, urlB); err == nil {
		t.Fatal("Swap after Close succeeded")
	}
	if _, err := database.OpenPool(ctx, "postgres://x:y@127.0.0.1:1/app?connect_timeout=1", database.PoolOptions{}); err == nil {
		t.Fatal("OpenPool on an unreachable server succeeded")
	}
}

func TestMigratedProbe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	migrated := dbtest.New(t, dbtest.App)
	sets := map[string]fs.FS{"goose_app": database.Migrations, "goose_auth": auth.Migrations}
	if err := database.MigratedProbe(sets)(ctx, migrated); err != nil {
		t.Fatalf("migrated database refused: %v", err)
	}
	ahead := fstest.MapFS{"00001_x.sql": {}, "99999_future.sql": {}, "README.md": {}}
	if err := database.MigratedProbe(map[string]fs.FS{"goose_app": ahead})(ctx, migrated); err == nil || !strings.Contains(err.Error(), "needs 99999") {
		t.Fatalf("stale database accepted: %v", err)
	}
	if err := database.MigratedProbe(map[string]fs.FS{"goose_missing": ahead})(ctx, migrated); err == nil {
		t.Fatal("database without the version table accepted")
	}
}
