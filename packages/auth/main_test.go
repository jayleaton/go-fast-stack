package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool is a pool on a throwaway database with the auth migrations
// applied, or nil when no database is configured. Tests on it run in
// parallel, so they only touch rows they created (uniqueEmail, generateID);
// a test that reads table-wide state uses newDatabase instead.
var testPool *pgxpool.Pool

// testDSN is the server URL (TEST_DATABASE_URL or DATABASE_URL), and
// templateDB a migrated template database on it that newDatabase clones.
var testDSN, templateDB string

// TestMain migrates a template database (auth_tmpl_<random>) once on the
// server named by TEST_DATABASE_URL or DATABASE_URL and clones it into the
// shared test database, so it never touches existing data and can run
// alongside other packages' tests. Without a URL the database tests skip,
// except under CI where they fail.
func TestMain(m *testing.M) {
	testDSN = os.Getenv("TEST_DATABASE_URL")
	if testDSN == "" {
		testDSN = os.Getenv("DATABASE_URL")
	}
	if testDSN == "" {
		os.Exit(m.Run())
	}
	code, err := runWithDatabase(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "auth tests:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runWithDatabase(m *testing.M) (int, error) {
	ctx := context.Background()
	admin, err := connectWhenReady(ctx, testDSN)
	if err != nil {
		return 0, err
	}
	defer func() { _ = admin.Close(ctx) }()

	// Random, not pid+time: concurrent or repeated test binaries must never pick
	// the same name (a collision fails TestMain before any test runs).
	tmpl := "auth_tmpl_" + strings.ToLower(generateID(16))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+tmpl); err != nil {
		return 0, err
	}
	defer func() {
		// A template cannot be dropped; unmark it first.
		_, _ = admin.Exec(ctx, "ALTER DATABASE "+tmpl+" WITH IS_TEMPLATE false")
		_, _ = admin.Exec(ctx, "DROP DATABASE "+tmpl+" WITH (FORCE)")
	}()
	if err := migrateTemplate(ctx, admin, tmpl); err != nil {
		return 0, err
	}
	templateDB = tmpl

	name := "auth_test_" + strings.ToLower(generateID(16))
	if _, err := admin.Exec(ctx, cloneSQL(name)); err != nil {
		return 0, err
	}
	defer func() { _, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)") }()
	testPool, err = poolOn(ctx, name)
	if err != nil {
		return 0, err
	}
	defer testPool.Close()
	return m.Run(), nil
}

// migrateTemplate applies the auth migrations to tmpl and marks it a
// template nobody can connect to, which is what lets clones run concurrently.
func migrateTemplate(ctx context.Context, admin *pgx.Conn, tmpl string) error {
	pool, err := poolOn(ctx, tmpl)
	if err != nil {
		return err
	}
	err = Migrate(ctx, pool)
	pool.Close()
	if err != nil {
		return err
	}
	_, err = admin.Exec(ctx, "ALTER DATABASE "+tmpl+" WITH IS_TEMPLATE true ALLOW_CONNECTIONS false")
	return err
}

func cloneSQL(name string) string {
	return "CREATE DATABASE " + name + " TEMPLATE " + templateDB + " STRATEGY FILE_COPY"
}

// poolOn returns a pool on database name of the test server.
func poolOn(ctx context.Context, name string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(testDSN)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Database = name
	return pgxpool.NewWithConfig(ctx, cfg)
}

// newDatabase gives one test a migrated database of its own (a clone of the
// template), dropped when it ends: for tests that read table-wide state or
// change state other tests share (key rotation).
func newDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	needDB(t)
	return createDatabase(t, cloneSQL)
}

// newEmptyDatabase gives one test an empty database (no migrations), dropped
// when it ends.
func newEmptyDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	needDB(t)
	return createDatabase(t, func(name string) string { return "CREATE DATABASE " + name })
}

func createDatabase(t *testing.T, create func(name string) string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := "auth_extra_" + strings.ToLower(generateID(12))
	if _, err := admin.Exec(ctx, create(name)); err != nil {
		_ = admin.Close(ctx)
		t.Fatal(err)
	}
	pool, err := poolOn(ctx, name)
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})
	if err != nil {
		t.Fatal(err)
	}
	return pool
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
		time.Sleep(200 * time.Millisecond) //nolint:forbidigo // polls Postgres readiness before any test runs, not test timing
	}
}

func needDB(t *testing.T) {
	t.Helper()
	if testPool == nil {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL or DATABASE_URL must be set in CI")
		}
		t.Skip("no TEST_DATABASE_URL/DATABASE_URL: skipping database test")
	}
}

const testSecret = "test-secret-test-secret-test-secret-0123456789"
const testBase = "http://app.test"

type sentMail struct{ kind, to, url string }

// captureMailer records emails instead of sending them.
type captureMailer struct {
	mu   sync.Mutex
	sent []sentMail
}

func (m *captureMailer) SendVerificationEmail(_ context.Context, to, url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentMail{"verification", to, url})
	return nil
}

func (m *captureMailer) SendPasswordResetEmail(_ context.Context, to, url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentMail{"reset", to, url})
	return nil
}

func (m *captureMailer) SendSetPasswordEmail(_ context.Context, to, url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentMail{"set-password", to, url})
	return nil
}

// SendEmailChangeNotice records the new address in url.
func (m *captureMailer) SendEmailChangeNotice(_ context.Context, to, newEmail string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentMail{"change-notice", to, newEmail})
	return nil
}

type allowAll struct{}

func (allowAll) Allow(context.Context, string, RateRule) (time.Duration, error) { return 0, nil }

// testClock is a settable clock for expiry tests.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	t      *testing.T
	a      *Auth
	h      http.Handler
	mail   *captureMailer
	clock  *testClock
	logBuf *bytes.Buffer
}

// newHarness builds an Auth on the test database with a capturing mailer, no
// rate limiting and a settable clock. mutate adjusts the config.
func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	needDB(t)
	logBuf := &bytes.Buffer{}
	cfg := Config{
		Secret:         testSecret,
		BaseURL:        testBase,
		TrustedOrigins: MobileTrustedOrigins(false),
		RateLimiter:    allowAll{},
		Logger:         slog.New(slog.NewTextHandler(&lockedWriter{w: logBuf}, nil)),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	mail := &captureMailer{}
	a, err := New(cfg, testPool, mail)
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{t: time.Now()}
	a.now = clock.Now
	// Limiters run on the same clock as everything else, so a slow machine
	// cannot let a rate-limit window lapse mid-test.
	if m, ok := a.limiter.(*MemoryRateLimiter); ok {
		m.now = clock.Now
	}
	return &harness{t: t, a: a, h: a.Handler(), mail: mail, clock: clock, logBuf: logBuf}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// mails waits for background sends and returns what was sent.
func (h *harness) mails() []sentMail {
	h.t.Helper()
	if err := h.a.Shutdown(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	h.mail.mu.Lock()
	defer h.mail.mu.Unlock()
	return append([]sentMail(nil), h.mail.sent...)
}

var emailSeq atomic.Int64

func uniqueEmail(t *testing.T) string {
	name := strings.ToLower(regexp.MustCompile(`[^a-zA-Z0-9]+`).ReplaceAllString(t.Name(), "-"))
	return fmt.Sprintf("%s-%d@example.com", name, emailSeq.Add(1))
}

type hdr map[string]string

// do sends a request through the auth handler. body is JSON-encoded unless it
// is nil; Content-Type defaults to application/json.
func (h *harness) do(method, path string, body any, headers hdr) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.serve(h.h, method, path, body, headers)
}

func (h *harness) serve(handler http.Handler, method, path string, body any, headers hdr) *httptest.ResponseRecorder {
	h.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// jsonBody decodes the response body.
func jsonBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return m
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, status, rec.Body.String())
	}
}

func wantCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	wantStatus(t, rec, status)
	if got := jsonBody(t, rec)["code"]; got != code {
		t.Fatalf("code = %v, want %s; body: %s", got, code, rec.Body.String())
	}
}

// sessionCookie returns the session cookie set by rec, or nil.
func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if strings.HasSuffix(c.Name, ".session_token") {
			return c
		}
	}
	return nil
}

// cookieHeader turns the session cookie of rec into a Cookie header value.
func cookieHeader(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	c := sessionCookie(rec)
	if c == nil || c.MaxAge < 0 {
		t.Fatalf("no session cookie set; headers: %v", rec.Header())
	}
	return c.Name + "=" + c.Value
}

// signUp registers a user through the JSON endpoint and returns the response.
func (h *harness) signUp(email, password string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do("POST", BasePath+"/sign-up/email", map[string]any{"name": "Test User", "email": email, "password": password}, nil)
}

func (h *harness) signIn(email, password string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do("POST", BasePath+"/sign-in/email", map[string]any{"email": email, "password": password}, nil)
}

// getSession returns the decoded get-session body (nil when signed out).
func (h *harness) getSession(headers hdr) map[string]any {
	h.t.Helper()
	rec := h.do("GET", BasePath+"/get-session", nil, headers)
	wantStatus(h.t, rec, http.StatusOK)
	if strings.TrimSpace(rec.Body.String()) == "null" {
		return nil
	}
	return jsonBody(h.t, rec)
}

// pathOf strips the origin from an absolute link so it can be served by the
// in-process handler.
func pathOf(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.RequestURI()
}
