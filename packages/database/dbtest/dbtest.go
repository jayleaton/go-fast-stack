// Package dbtest gives each test its own migrated Postgres database, cloned
// from a template so tests stay fast and can run in parallel.
//
// Tests need TEST_DATABASE_URL (any database on the test server; `make test`
// starts one). Without it, tests that ask for a database are skipped.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Setup migrates a fresh template database.
type Setup func(ctx context.Context, pool *pgxpool.Pool) error

// Template describes a migrated database to clone. Fingerprint must change
// whenever Setup would produce a different schema (hash the migrations).
type Template struct {
	Name        string
	Fingerprint string
	Setup       Setup
}

var (
	mu       sync.Mutex
	prepared = map[string]error{}
)

// New returns a pool on a new database cloned from tmpl. The database is
// dropped when the test ends.
func New(t testing.TB, tmpl Template) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set (run `make test`)")
	}
	ctx := context.Background()

	tmplName := fmt.Sprintf("tmpl_%s_%x", tmpl.Name, fnv64(tmpl.Fingerprint))
	if err := prepare(ctx, base, tmplName, tmpl.Setup); err != nil {
		t.Fatalf("dbtest: prepare template: %v", err)
	}

	admin := connect(t, base)
	name := "test_" + randomHex()
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s STRATEGY FILE_COPY`, name, tmplName)); err != nil {
		t.Fatalf("dbtest: clone template: %v", err)
	}

	pool, err := pgxpool.New(ctx, withDatabase(t, base, name))
	if err != nil {
		t.Fatalf("dbtest: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name)); err != nil {
			t.Errorf("dbtest: drop %s: %v", name, err)
		}
		_ = admin.Close(context.Background())
	})
	return pool
}

// prepare creates the template database once per server. Test binaries for
// different packages run concurrently, so creation is guarded by an advisory lock.
func prepare(ctx context.Context, base, name string, setup Setup) error {
	mu.Lock()
	defer mu.Unlock()
	if err, ok := prepared[name]; ok {
		return err
	}
	err := createTemplate(ctx, base, name, setup)
	prepared[name] = err
	return err
}

func createTemplate(ctx context.Context, base, name string, setup Setup) error {
	admin, err := connectWhenReady(ctx, base)
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close(ctx) }()

	if _, err := admin.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, name); err != nil {
		return err
	}
	defer func() { _, _ = admin.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1))`, name) }()

	var exists bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1 AND datistemplate)`, name).Scan(&exists); err != nil || exists {
		return err
	}

	// A half-built template from an interrupted run is not marked datistemplate yet.
	if _, err := admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name)); err != nil {
		return err
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, name)); err != nil {
		return err
	}
	url, err := replaceDatabase(base, name)
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return err
	}
	err = setup(ctx, pool)
	pool.Close()
	if err != nil {
		return err
	}
	_, err = admin.Exec(ctx, fmt.Sprintf(`ALTER DATABASE %s WITH IS_TEMPLATE true ALLOW_CONNECTIONS false`, name))
	return err
}

// connectWhenReady connects to dsn, retrying for up to 30s while the server
// is not accepting connections yet. A freshly started Postgres container runs
// initdb with a socket-only server and then restarts, so a readiness check
// over the socket can pass while TCP connections are still refused or reset;
// the first test binary to connect would then fail before any test runs.
func connectWhenReady(ctx context.Context, dsn string) (*pgx.Conn, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			if err = conn.Ping(ctx); err == nil {
				return conn, nil
			}
			_ = conn.Close(ctx)
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("postgres not ready after 30s: %w", err)
		}
		time.Sleep(200 * time.Millisecond) // readiness poll for the server, not test timing
	}
}

func connect(t testing.TB, url string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("dbtest: connect: %v", err)
	}
	return conn
}

func withDatabase(t testing.TB, base, name string) string {
	t.Helper()
	url, err := replaceDatabase(base, name)
	if err != nil {
		t.Fatalf("dbtest: %v", err)
	}
	return url
}

func replaceDatabase(base, name string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" {
		return "", fmt.Errorf("TEST_DATABASE_URL must be a postgres:// URL")
	}
	u.Path = "/" + name
	return u.String(), nil
}

func randomHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func fnv64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
