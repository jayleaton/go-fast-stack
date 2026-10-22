package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/jackc/pgx/v5"
)

// PgTools runs pg_dump and pg_restore from PATH (the backup image ships the
// ones matching the server: postgres:18-alpine). The password never goes on
// the command line, where `ps` would show it: it is passed as PGPASSWORD.
type PgTools struct {
	PgDump    string // default "pg_dump"
	PgRestore string // default "pg_restore"
	Psql      string // default "psql"
}

var _ Postgres = PgTools{}

// Dump writes a compressed custom-format dump (pg_dump -Fc) to w. Owners and
// grants are left out: the restore target's role owns everything.
func (p PgTools) Dump(ctx context.Context, dbURL string, w io.Writer) error {
	return run(ctx, or(p.PgDump, "pg_dump"), dbURL, nil, w, "--format=custom", "--compress=6", "--no-owner", "--no-acl")
}

// Restore loads a custom-format dump from r in one transaction, so a failed
// restore leaves the target as it was. clean replaces everything in the
// target: every user schema is dropped with CASCADE (pg_restore --clean
// cannot drop a table a newer migration's table depends on), public is
// recreated, and the dump is replayed, all in one psql transaction.
func (p PgTools) Restore(ctx context.Context, dbURL string, r io.Reader, clean bool) error {
	if !clean {
		return run(ctx, or(p.PgRestore, "pg_restore"), dbURL, r, io.Discard, "--no-owner", "--no-acl", "--exit-on-error", "--single-transaction")
	}
	// pg_restore turns the dump into SQL (no database needed); psql runs the
	// preamble and that SQL as one transaction.
	sqlR, sqlW := io.Pipe()
	toSQL := make(chan error, 1)
	go func() {
		err := runLocal(ctx, or(p.PgRestore, "pg_restore"), r, sqlW, "--no-owner", "--no-acl", "--file=-")
		_ = sqlW.CloseWithError(err)
		toSQL <- err
	}()
	in := io.MultiReader(strings.NewReader(replaceAll), sqlR)
	err := run(ctx, or(p.Psql, "psql"), dbURL, in, io.Discard, "--no-psqlrc", "--quiet", "--single-transaction", "--set=ON_ERROR_STOP=1")
	_ = sqlR.Close()
	if dumpErr := <-toSQL; dumpErr != nil && err == nil {
		err = dumpErr
	}
	return err
}

// replaceAll drops every user schema (and with it every table, view,
// sequence, function and extension in it) and recreates an empty public.
const replaceAll = `DO $$
DECLARE s name;
BEGIN
  FOR s IN SELECT nspname FROM pg_namespace
    WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'
  LOOP
    EXECUTE format('DROP SCHEMA %I CASCADE', s);
  END LOOP;
END $$;
CREATE SCHEMA public;
`

// Tables counts user objects in the database at url: tables, views,
// materialized views, sequences and foreign tables outside the system
// schemas, plus any schema other than public.
func (PgTools) Tables(ctx context.Context, dbURL string) (int, error) {
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return 0, fmt.Errorf("connect: %w", err) // pgx never prints the password
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var n int
	err = conn.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		        WHERE c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f') AND n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%')
		     + (SELECT count(*) FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname NOT IN ('information_schema', 'public'))`).Scan(&n)
	return n, err
}

// run executes tool against dbURL (password moved to PGPASSWORD); stderr is
// returned in the error, minus anything that looks like the connection string.
func run(ctx context.Context, tool, dbURL string, stdin io.Reader, stdout io.Writer, args ...string) error {
	conn, password, err := splitPassword(dbURL)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, tool, append(args, "--dbname="+conn)...)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+password, "PGCONNECT_TIMEOUT=15")
	cmd.Stdin, cmd.Stdout = stdin, stdout
	var stderr bytes.Buffer
	cmd.Stderr = &limitWriter{w: &stderr, n: 8 << 10}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if password != "" {
			msg = strings.ReplaceAll(msg, password, "[redacted]")
		}
		return fmt.Errorf("%s: %w: %s", tool, err, msg)
	}
	return nil
}

// runLocal runs a tool that needs no database (pg_restore --file=-).
func runLocal(ctx context.Context, tool string, stdin io.Reader, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Stdin, cmd.Stdout = stdin, stdout
	var stderr bytes.Buffer
	cmd.Stderr = &limitWriter{w: &stderr, n: 8 << 10}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", tool, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// splitPassword returns the URL without its password, and the password.
func splitPassword(dbURL string) (string, string, error) {
	u, err := url.Parse(dbURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return "", "", errors.New("backup: the database URL must be postgres://user:password@host:port/db")
	}
	password, _ := u.User.Password()
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.String(), password, nil
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// limitWriter keeps at most n bytes (tool stderr in errors).
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	_, _ = l.w.Write(q)
	return len(p), nil
}
